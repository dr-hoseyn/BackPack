package cmd

import (
	"context"
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
	stop := startMetrics(context.WithoutCancel(ctx), path, "kernel-"+c.Method, c.Role)
	defer stop()
	for ctx.Err() == nil {
		err = kernel.RunManaged(ctx, c, func(ready bool) { naive.ReportReadiness(ctx, ready) })
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
