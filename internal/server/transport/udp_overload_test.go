package transport

import (
	"bytes"
	"context"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sirupsen/logrus"
)

func TestUDPOverloadBoundsConcurrentReportsAndCountsSuppressedDrops(t *testing.T) {
	var log udpOverloadLog
	now := time.Now()
	var reported atomic.Uint64
	var workers sync.WaitGroup
	for range 16 {
		workers.Go(func() {
			for range 1000 {
				reported.Add(log.add(now))
			}
		})
	}
	workers.Wait()
	if got := reported.Load(); got != 1 {
		t.Fatalf("one burst produced %d reported drops; want only the first report", got)
	}
	if got := log.add(now.Add(udpOverloadLogInterval - time.Nanosecond)); got != 0 {
		t.Fatal("overload was logged before the report interval")
	}
	if got := log.add(now.Add(udpOverloadLogInterval)); got != 16001 {
		t.Fatalf("next report counted %d drops; want 16001 including suppressed packets", got)
	}
	if got := testing.AllocsPerRun(100, func() { log.add(now) }); got != 0 {
		t.Fatalf("suppressed overload report allocated %.1f objects", got)
	}
}

func TestUDPOverloadOnAFullAssociationDoesNotLogEveryPacket(t *testing.T) {
	var output bytes.Buffer
	logger := logrus.New()
	logger.SetOutput(&output)
	logger.SetLevel(logrus.WarnLevel)
	queue := make(chan []byte, 1)
	queue <- []byte("opening packet")
	s := &UdpTransport{
		lifecycle:         lifecycle{logger: logger},
		limits:            newLimiter(Limits{}),
		activeConnections: map[string]*TunnelUDPConn{"peer": {payload: queue}},
	}
	for range 1000 {
		if !s.deliverTunnelPayload(context.Background(), "peer", []byte("drop")) {
			t.Fatal("full association disappeared")
		}
	}
	if got := strings.Count(output.String(), "UDP overload:"); got != 1 {
		t.Fatalf("1000 discarded packets generated %d overload warnings, want 1", got)
	}
	if string(<-queue) != "opening packet" {
		t.Fatal("overload discarded the queued opening packet")
	}
}
