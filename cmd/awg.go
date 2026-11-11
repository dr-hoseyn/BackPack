package cmd

import (
	"context"
	"io"
	"net/netip"
	"sync/atomic"
	"time"

	"github.com/backpack/backpack/config"
	"github.com/backpack/backpack/internal/amneziawg"
	"github.com/backpack/backpack/internal/metrics"
	"github.com/backpack/backpack/internal/tunnel/l3"
)

func awgForwarder(c config.AWGConfig) (*l3.Forwarder, error) {
	p, err := netip.ParsePrefix(c.PeerAddress)
	if err != nil {
		return nil, err
	}
	return l3.NewForwarder(l3.Config{PeerIP: p.Addr().String(), Ports: c.Ports, AcceptUDP: c.AcceptUDP, MaxConnections: c.MaxConnections, BandwidthMbps: c.BandwidthMbps}, nil)
}

var runManagedAWG = amneziawg.RunManaged

func runAWGTunnel(cfg *config.Config, ctx context.Context, path string) {
	var up atomic.Bool
	role := "server"
	if cfg.AWG.Endpoint != "" {
		role = "client"
	}
	metrics.ClearPeer()
	defer metrics.ClearPeer()
	stop := startMetrics(context.WithoutCancel(ctx), path, "awg", role)
	defer stop()
	forwarder, err := awgForwarder(cfg.AWG)
	if err != nil {
		logger.Errorf("AmneziaWG port mappings: %v", err)
		return
	}
	forwardctx, stopForward := context.WithCancel(ctx)
	defer stopForward()
	done := make(chan struct{})
	if forwarder != nil {
		forwarder.SetTunnelState(up.Load)
		go func() {
			defer close(done)
			for forwardctx.Err() == nil {
				if err := forwarder.Run(forwardctx); err != nil && forwardctx.Err() == nil {
					logger.Errorf("AmneziaWG forwarding: %v", err)
				}
				select {
				case <-forwardctx.Done():
					return
				case <-time.After(l3RestartDelay):
				}
			}
		}()
		defer func() { stopForward(); <-done }()
	}
	for ctx.Err() == nil {
		err := runManagedAWG(ctx, amneziawg.Config(cfg.AWG), io.Discard, func(s amneziawg.RuntimeStats) {
			up.Store(s.Connected())
			if s.Connected() {
				metrics.ReportPeer(s.Endpoint)
			} else {
				metrics.ClearPeer()
			}
		})
		up.Store(false)
		metrics.ClearPeer()
		if ctx.Err() != nil {
			return
		}
		logger.Errorf("AmneziaWG stopped: %v; retrying in %s", err, l3RestartDelay)
		select {
		case <-ctx.Done():
			return
		case <-time.After(l3RestartDelay):
		}
	}
}
