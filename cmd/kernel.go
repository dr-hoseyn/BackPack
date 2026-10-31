package cmd

import (
	"context"
	"sync/atomic"
	"time"

	"github.com/backpack/backpack/config"
	"github.com/backpack/backpack/internal/tunnel/kernel"
	"github.com/backpack/backpack/internal/tunnel/naive"
)

func runKernelTunnel(cfg *config.Config, ctx context.Context, path string) {
	c, err := kernel.Resolve(cfg.Kernel)
	if err != nil {
		logger.Errorf("kernel tunnel: %v", err)
		return
	}
	var in, out atomic.Uint64
	stop := startMetricsWithTraffic(context.WithoutCancel(ctx), path, "kernel-"+c.Method, c.Role, in.Load, out.Load)
	defer stop()
	for ctx.Err() == nil {
		baseIn, baseOut := in.Load(), out.Load()
		generation, finish := context.WithCancel(ctx)
		sampled := make(chan struct{})
		go func() {
			defer close(sampled)
			tick := time.NewTicker(time.Second)
			defer tick.Stop()
			for {
				select {
				case <-generation.Done():
					return
				case <-tick.C:
					a, b := kernel.Counters(c.Interface)
					in.Store(baseIn + a)
					out.Store(baseOut + b)
				}
			}
		}()
		err = kernel.RunManaged(generation, c, func(ready bool) { naive.ReportReadiness(ctx, ready) })
		finish()
		<-sampled
		if ctx.Err() != nil {
			return
		}
		logger.Errorf("kernel tunnel stopped: %v; retrying in %s", err, l3RestartDelay)
		select {
		case <-ctx.Done():
			return
		case <-time.After(l3RestartDelay):
		}
	}
}
