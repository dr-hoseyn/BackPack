package transport

import (
	"sync"

	"github.com/xtaci/smux"
)

// Sampling once per pool tick keeps bookkeeping out of each stream's hot path.
// Entries live exactly as long as the session handler, including reconnects.
type muxPoolSessions struct {
	mu       sync.Mutex
	sessions map[*smux.Session]bool
}

func (p *muxPoolSessions) add(session *smux.Session, retirement bool) func() {
	p.mu.Lock()
	if p.sessions == nil {
		p.sessions = make(map[*smux.Session]bool)
	}
	p.sessions[session] = retirement
	p.mu.Unlock()
	return func() {
		p.mu.Lock()
		delete(p.sessions, session)
		p.mu.Unlock()
	}
}

func (p *muxPoolSessions) snapshot() (active, total int, retirement bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	retirement = len(p.sessions) > 0
	for session, supported := range p.sessions {
		total++
		if session.NumStreams() > 0 {
			active++
		}
		retirement = retirement && supported
	}
	return
}
