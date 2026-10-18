//go:build !windows

package adaptive

import (
	"context"
	"sync"
	"sync/atomic"

	"github.com/backpack/backpack/config"
	"github.com/backpack/backpack/internal/tunnel/naive"
	"github.com/sirupsen/logrus"
)

type ServerPaths struct {
	cancel context.CancelFunc
	wg     sync.WaitGroup
	once   sync.Once
}

func StartServer(parent context.Context, cfg *config.ServerConfig, log *logrus.Logger) (*ServerPaths, error) {
	if log == nil {
		log = logrus.StandardLogger()
	}
	if err := Validate(&config.Config{Server: *cfg}); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(parent)
	s := &ServerPaths{cancel: cancel}
	ready := make([]atomic.Bool, len(cfg.Paths))
	var readyMu sync.Mutex
	for i, p := range cfg.Paths {
		child := serverConfig(*cfg, p)
		pathCtx := naive.WithReadiness(ctx, func(value bool) {
			readyMu.Lock()
			defer readyMu.Unlock()
			ready[i].Store(value)
			anyReady := false
			for j := range ready {
				anyReady = anyReady || ready[j].Load()
			}
			naive.ReportReadiness(parent, anyReady)
		})
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			h, err := naive.StartWithRetry(pathCtx, log, func() (*naive.Helper, error) { return startServerHelper(pathCtx, &child, log) })
			if err != nil {
				return
			}
			defer h.Close()
			log.Infof("managed path %s listener ready; authenticated transfer is still required", p.Name)
			<-ctx.Done()
		}()
	}
	return s, nil
}

func startServerHelper(ctx context.Context, cfg *config.ServerConfig, log *logrus.Logger) (*naive.Helper, error) {
	switch {
	case cfg.Connect.Enabled():
		return naive.StartConnectServer(ctx, cfg, log)
	case cfg.Short.Enabled():
		return naive.StartShortServer(ctx, cfg, log)
	case cfg.Sing.Enabled():
		return naive.StartSingServer(ctx, cfg, log)
	case cfg.Xray.Enabled():
		return naive.StartXrayServer(ctx, cfg, log)
	default:
		return naive.StartServer(ctx, cfg, log)
	}
}

func (s *ServerPaths) Close() { s.once.Do(s.cancel); s.wg.Wait() }
