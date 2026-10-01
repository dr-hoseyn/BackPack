package limits

import (
	"context"
	"io"
	"net"
	"testing"
	"time"
)

func TestRecoveryRegressionCloseCancelsBandwidthWait(t *testing.T) {
	l := New(Config{BandwidthMbps: 1})
	client, server := net.Pipe()
	defer client.Close()
	defer server.Close()
	go io.Copy(io.Discard, server)
	wrapped := l.Wrap(context.Background(), client)
	l.bucket.AllowN(time.Now(), l.bucket.Burst())
	done := make(chan error, 1)
	go func() { _, err := wrapped.Write(make([]byte, l.bucket.Burst())); done <- err }()
	time.Sleep(30 * time.Millisecond)
	wrapped.Close()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("write succeeded after closing")
		}
	case <-time.After(200 * time.Millisecond):
		<-done
		t.Fatal("closing the connection left its bandwidth wait alive")
	}
}
