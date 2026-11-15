package transport

import (
	"context"
	"io"
	"testing"

	"github.com/sirupsen/logrus"
)

type countedBindCloser struct{ calls int }

func (l *countedBindCloser) Close() error { l.calls++; return nil }

func TestCancelledListenerDoesNotBind(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	logger := logrus.New()
	logger.SetOutput(io.Discard)
	listener, ok := bindTunnelPort(ctx, logger, "unused", func() (*countedBindCloser, error) {
		t.Fatal("a cancelled generation attempted to bind")
		return nil, nil
	})
	if ok || listener != nil {
		t.Fatal("cancelled generation retained a listener")
	}
}

func TestCancelledDuringBindClosesTheNewListener(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	logger := logrus.New()
	logger.SetOutput(io.Discard)
	created := &countedBindCloser{}
	listener, ok := bindTunnelPort(ctx, logger, "unused", func() (*countedBindCloser, error) {
		cancel()
		return created, nil
	})
	if ok || listener != nil || created.calls != 1 {
		t.Fatalf("cancelled bind retained its port: ok=%v, listener=%v, closes=%d", ok, listener, created.calls)
	}
}
