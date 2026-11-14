package transport

import (
	"sync"

	"github.com/xtaci/smux"
)

// Sampling once per pool tick keeps bookkeeping out of each stream's hot path.
// Entries live exactly as long as the session handler, including reconnects.
type muxPoolSessions struct {
	mu       sync.Mutex
	sessions map[*smux.Session]muxPoolEntry
}

type muxPoolEntry struct {
	retirement bool
	pressure   func() (score int, known bool)
}

type muxPoolSample struct {
	active, total, streams int
	pressureObserved       int
	pressured              int
	retirement             bool
}

func (p *muxPoolSessions) add(session *smux.Session, retirement bool) func() {
	return p.addObserved(session, retirement, nil)
}

func (p *muxPoolSessions) addObserved(session *smux.Session, retirement bool, pressure func() (int, bool)) func() {
	p.mu.Lock()
	if p.sessions == nil {
		p.sessions = make(map[*smux.Session]muxPoolEntry)
	}
	p.sessions[session] = muxPoolEntry{retirement: retirement, pressure: pressure}
	p.mu.Unlock()
	return func() {
		p.mu.Lock()
		delete(p.sessions, session)
		p.mu.Unlock()
	}
}

func (p *muxPoolSessions) snapshot() (active, total int, retirement bool) {
	s := p.sample(false)
	return s.active, s.total, s.retirement
}

// Pressure is optional: the default occupancy sample does no socket syscalls.
// A failed/unsupported observation is unknown, never evidence of spare capacity.
func (p *muxPoolSessions) sample(pressure bool) muxPoolSample {
	p.mu.Lock()
	defer p.mu.Unlock()
	s := muxPoolSample{retirement: len(p.sessions) > 0}
	for session, entry := range p.sessions {
		s.total++
		streams := session.NumStreams()
		s.streams += streams
		if streams > 0 {
			s.active++
		}
		s.retirement = s.retirement && entry.retirement
		if pressure && entry.pressure != nil && !session.IsClosed() {
			if score, known := entry.pressure(); known {
				s.pressureObserved++
				if score > 0 {
					s.pressured++
				}
			}
		}
	}
	return s
}
