package transport

import (
	"sync"
	"time"
)

const udpOverloadLogInterval = 5 * time.Second

// One budget for all flows and listeners: a full queue must not turn every
// discarded packet into synchronous formatting and a journal write. Keep a
// count of suppressed drops so sustained overload remains visible.
type udpOverloadLog struct {
	mu      sync.Mutex
	next    time.Time
	dropped uint64
}

func (l *udpOverloadLog) add(now time.Time) uint64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.dropped++
	if now.Before(l.next) {
		return 0
	}
	dropped := l.dropped
	l.dropped = 0
	l.next = now.Add(udpOverloadLogInterval)
	return dropped
}

func (s *UdpTransport) logUDPOverload(reason string) {
	if dropped := s.overloadLog.add(time.Now()); dropped != 0 {
		s.logger.Warnf("UDP overload: dropped %d packets since the previous report (latest cause: %s)", dropped, reason)
	}
}
