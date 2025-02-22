package p2pd

import (
	"context"
	"fmt"
	"time"

	"os"
	"sync"

	"github.com/learning-at-home/go-libp2p-daemon/config"
	"github.com/learning-at-home/go-libp2p-daemon/internal/utils"

	"github.com/chiangmaioneluv/go-libp2p"
	"github.com/chiangmaioneluv/go-libp2p/core/host"
	"github.com/chiangmaioneluv/go-libp2p/core/metrics"
	"github.com/chiangmaioneluv/go-libp2p/core/peer"
	"github.com/chiangmaioneluv/go-libp2p/core/protocol"
	"github.com/chiangmaioneluv/go-libp2p/core/routing"
	"github.com/chiangmaioneluv/go-libp2p/p2p/host/resource-manager"
	"github.com/chiangmaioneluv/go-libp2p/p2p/protocol/circuitv2/relay"

	multierror "github.com/hashicorp/go-multierror"
	logging "github.com/ipfs/go-log"
	dht "github.com/libp2p/go-libp2p-kad-dht"
	dhtopts "github.com/libp2p/go-libp2p-kad-dht/opts"
	ps "github.com/libp2p/go-libp2p-pubsub"
	ma "github.com/multiformats/go-multiaddr"
	manet "github.com/multiformats/go-multiaddr/net"
)

var log = logging.Logger("p2pd")

type Daemon struct {
	ctx      context.Context
	host     host.Host
	listener manet.Listener

	dht    *dht.IpfsDHT
	pubsub *ps.PubSub

	peerSourceChan       chan peer.AddrInfo // potential relay peers go through this channel; nil means no relay disovery
	cancelRelayDiscovery context.CancelFunc

	mx sync.Mutex
	// stream handlers: map of protocol.ID to multi-addresses, balanced by round robin
	handlers map[protocol.ID]*utils.RoundRobin
	// closed is set when the daemon is shutting down
	closed bool

	// unary protocols handlers: map of protocol.ID to wirte ends of pipe, balanced by round robin
	registeredUnaryProtocols map[protocol.ID]*utils.RoundRobin

	// callID (int64) to chan *pb.PersistentConnectionResponse
	// used to return responses to goroutines awating them
	responseWaiters sync.Map
	// callID (int64) to chan context.CancelFunc
	// used to cancel request handlers
	cancelUnary sync.Map

	// this sync.Once ensures the goroutine awaiting deamon termination is
	// only run once
	terminateOnce        sync.Once
	terminateWG          sync.WaitGroup
	cancelTerminateTimer context.CancelFunc

	persistentConnMsgMaxSize int

	bandwidth_metrics *metrics.BandwidthCounter
}

func NewDaemon(
	ctx context.Context,
	maddr ma.Multiaddr,
	dhtMode string,
	relayDiscovery bool,
	bandwidthMetricsEnabled bool,
	trustedRelays []string,
	persistentConnMsgMaxSize int,
	opts ...libp2p.Option,
) (*Daemon, error) {
	d := &Daemon{
		ctx:                      ctx,
		handlers:                 make(map[protocol.ID]*utils.RoundRobin),
		registeredUnaryProtocols: make(map[protocol.ID]*utils.RoundRobin),
		persistentConnMsgMaxSize: persistentConnMsgMaxSize,
	}
	// setup resource usage limits; see https://github.com/chiangmaioneluv/go-libp2p/tree/master/p2p/host/resource-manager
	rm, err := rcmgr.NewResourceManager(rcmgr.NewFixedLimiter(rcmgr.InfiniteLimits))
	if err != nil {
		panic(err)
	}
	opts = append(opts, libp2p.ResourceManager(rm))

	opts, d.peerSourceChan = MaybeConfigureAutoRelay(opts, relayDiscovery, trustedRelays)

	if dhtMode != "" {
		var dhtOpts []dhtopts.Option
		if dhtMode == config.DHTClientMode {
			dhtOpts = append(dhtOpts, dht.Mode(dht.ModeClient))
		} else if dhtMode == config.DHTServerMode {
			dhtOpts = append(dhtOpts, dht.Mode(dht.ModeServer))
		}

		opts = append(opts, libp2p.Routing(d.DHTRoutingFactory(dhtOpts)))
	}

	if bandwidthMetricsEnabled {
		d.bandwidth_metrics = metrics.NewBandwidthCounter()
		opts = append(opts, libp2p.BandwidthReporter(d.bandwidth_metrics))
	} else {
		d.bandwidth_metrics = nil
	}

	h, err := libp2p.New(opts...)
	if err != nil {
		return nil, err
	}
	d.host = h

	l, err := manet.Listen(maddr)
	if err != nil {
		h.Close()
		return nil, err
	}
	d.listener = l

	if d.peerSourceChan != nil {
		d.cancelRelayDiscovery = BeginRelayDiscovery(d.host, d.dht, trustedRelays, d.peerSourceChan)
	}

	go d.trapSignals()

	return d, nil
}

func (d *Daemon) Listener() manet.Listener {
	return d.listener
}

func (d *Daemon) DHTRoutingFactory(opts []dhtopts.Option) func(host.Host) (routing.PeerRouting, error) {
	makeRouting := func(h host.Host) (routing.PeerRouting, error) {
		dhtInst, err := dht.New(d.ctx, h, opts...)
		if err != nil {
			return nil, err
		}
		d.dht = dhtInst
		return dhtInst, nil
	}

	return makeRouting
}

func (d *Daemon) EnableRelayV2() error {
	_, err := relay.New(d.host)
	return err
}

func (d *Daemon) EnablePubsub(router string, sign, strict bool) error {
	var opts []ps.Option

	if !sign {
		opts = append(opts, ps.WithMessageSigning(false))
	} else if !strict {
		opts = append(opts, ps.WithStrictSignatureVerification(false))
	} else {
		opts = append(opts, ps.WithMessageSignaturePolicy(ps.StrictSign))
	}

	switch router {
	case "floodsub":
		pubsub, err := ps.NewFloodSub(d.ctx, d.host, opts...)
		if err != nil {
			return err
		}
		d.pubsub = pubsub
		return nil

	case "gossipsub":
		pubsub, err := ps.NewGossipSub(d.ctx, d.host, opts...)
		if err != nil {
			return err
		}
		d.pubsub = pubsub
		return nil

	default:
		return fmt.Errorf("unknown pubsub router: %s", router)
	}

}

func (d *Daemon) ID() peer.ID {
	return d.host.ID()
}

func (d *Daemon) Addrs() []ma.Multiaddr {
	return d.host.Addrs()
}

func (d *Daemon) Serve() error {
	for {
		if d.isClosed() {
			return nil
		}

		c, err := d.listener.Accept()
		if err != nil {
			log.Errorw("error accepting connection", "error", err)
			continue
		}

		log.Debug("incoming connection")
		go d.handleConn(c)
	}
}

func (d *Daemon) isClosed() bool {
	d.mx.Lock()
	defer d.mx.Unlock()
	return d.closed
}

func clearUnixSockets(path ma.Multiaddr) error {
	c, _ := ma.SplitFirst(path)
	if c.Protocol().Code != ma.P_UNIX {
		return nil
	}

	if err := os.Remove(c.Value()); err != nil {
		return err
	}

	return nil
}

func (d *Daemon) Close() error {
	d.mx.Lock()
	d.closed = true
	d.mx.Unlock()

	var merr *multierror.Error
	if err := d.host.Close(); err != nil {
		merr = multierror.Append(err)
	}

	listenAddr := d.listener.Multiaddr()
	if err := d.listener.Close(); err != nil {
		merr = multierror.Append(merr, err)
	}

	if err := clearUnixSockets(listenAddr); err != nil {
		merr = multierror.Append(merr, err)
	}

	if d.cancelRelayDiscovery != nil {
		d.cancelRelayDiscovery()
		d.cancelRelayDiscovery = nil
	}
	if d.peerSourceChan != nil {
		close(d.peerSourceChan)
		d.peerSourceChan = nil
	}

	return merr.ErrorOrNil()
}

func (d *Daemon) awaitTermination() {
	d.terminateWG.Wait()
	d.Close()
}

func (d *Daemon) KillOnTimeout(timeout time.Duration) {
	var ctx context.Context
	ctx, d.cancelTerminateTimer = context.WithCancel(d.ctx)
	go func() {
		select {
		case <-ctx.Done():
			return
		case <-time.NewTimer(timeout).C:
			d.Close()
		}
	}()
}
