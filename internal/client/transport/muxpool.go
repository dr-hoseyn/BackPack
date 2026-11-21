package transport

import (
	"sync"

	"github.com/xtaci/smux"
)

// Sampling once per pool tick keeps bookkeeping out of each stream's hot path.
// Entries live exactly as long as the session handler, including reconnects.
type muxPoolSessions struct {
	mu       sync.Mutex
	sessions map[*smux.Session]muxRetirement
}

type muxRetirement struct {
	supported bool
	localIdle bool
}

func (p *muxPoolSessions) add(session *smux.Session, retirement, localIdle bool) func() {
	p.mu.Lock()
	if p.sessions == nil {
		p.sessions = make(map[*smux.Session]muxRetirement)
	}
	p.sessions[session] = muxRetirement{supported: retirement, localIdle: localIdle}
	p.mu.Unlock()
	return func() {
		p.mu.Lock()
		delete(p.sessions, session)
		p.mu.Unlock()
	}
}

func (p *muxPoolSessions) snapshot() (active, total int, retirement, localIdle bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	retirement = len(p.sessions) > 0
	localIdle = retirement
	for session, policy := range p.sessions {
		total++
		if session.NumStreams() > 0 {
			active++
		}
		retirement = retirement && policy.supported
		localIdle = localIdle && policy.localIdle
	}
	return
}
