package transport

import (
	"context"
	"sync/atomic"
	"time"

	"github.com/sirupsen/logrus"

	"github.com/backpack/backpack/internal/metrics"
)

// How big the connection pool should be, decided once.
//
// Every client transport keeps a pool of connections dialled ahead of the
// traffic that will use them, and every one of them had its own copy of the
// loop that sizes it. The copies were the same loop — the same two tickers, the
// same two factors, the same growth and shrink conditions — and they had
// drifted exactly the way copies do.
//
// Three of the seven were still character-for-character identical. The other
// four had each been edited at a different time, and the drift was not
// cosmetic:
//
//   - **`udp` never reported its pool at all**, so the panel's connection-pool
//     card was simply absent on a udp tunnel — not empty, not zero, missing —
//     while it appeared for every other transport.
//   - **`udp` never grew its pool on throughput either.** The load signal that
//     lets a pool grow when its connections are each working hard, rather than
//     only when somebody is waiting for one, was added to the others and not to
//     it. A udp tunnel under sustained load therefore sat at its configured
//     size while the same load grew every other transport's pool.
//   - `ws` had already been through the first of those two, and the comment
//     recording it is still in the history: the card "was simply missing on a
//     ws or wss tunnel". It was fixed there and not looked for anywhere else.
//
// That is the whole argument for one copy. The policy here is a judgement about
// when a tunnel needs more connections, and a judgement that exists seven times
// is seven judgements.
//
// # What a transport still decides
//
// How to dial one connection, and what a connection is. `dial` is the only
// thing passed in, and every difference between the transports lives behind it.

// poolSizer is everything the sizing loop needs from the transport that owns
// the pool.
//
// The counters are pointers because they belong to the transport and are
// written by its dialler and its handler, on other goroutines; this loop only
// reads and resets them.
type poolSizer struct {
	ctx context.Context
	log *logrus.Logger

	// size is the configured pool size — the floor the pool returns to, and
	// the figure the panel shows beside what is actually open.
	size int
	// aggressive selects the tighter factors: grow sooner, shrink later.
	aggressive bool
	// mux says open includes traffic-carrying physical sessions.
	mux bool

	// open counts connections sitting in the pool right now.
	open *int32
	// taken counts connections handed to traffic since the last load tick.
	taken *int32

	// shrink is the channel the transport listens on to retire one connection.
	shrink chan struct{}
	// dial starts one new pool connection. It is expected to return when that
	// connection ends, so the loop starts it on its own goroutine.
	dial func()
	// spawn registers workers with their generation; nil is useful to callers
	// that run the sizing policy on its own.
	spawn   func(func())
	pending *int32
	maxSize int
}

func (p poolSizer) launch() {
	if p.spawn != nil {
		p.spawn(p.dial)
	} else {
		go p.dial()
	}
}

// maintain reacts once a second. Growing by one every ten seconds took minutes
// to absorb a burst while the server gives queued users only three seconds.
func (p poolSizer) maintain() {
	var quietSince time.Time
	for i := 0; i < p.size; i++ { // initial pool filling
		p.launch()
	}

	// The factors. a and b decide when the pool is too small, x and y when it
	// is too large; aggressive tightens both so a burst is met sooner and the
	// pool takes longer to give the connections back.
	a, b := 4, 5
	x, y := 3, 4.0
	if p.aggressive {
		p.log.Info("aggressive pool management enabled")
		a, b = 1, 2
		x, y = 0, 0.75
	}

	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()

	newPoolSize := p.size // initial value
	ceiling := p.maxSize
	if ceiling <= 0 {
		ceiling = p.size * poolGrowthLimit
	}
	var load poolLoad // throughput signal, see poolload.go
	load.spare = !p.mux
	load.mbps() // establish a baseline before the first sizing decision
	lastSample := time.Now()

	for {
		select {
		case <-p.ctx.Done():
			return

		case now := <-ticker.C:
			// Use elapsed time: scheduler delays must not turn a multi-second
			// request count into a one-second burst.
			seconds := max(now.Sub(lastSample).Seconds(), 1)
			lastSample = now
			taken := int(float64(atomic.SwapInt32(p.taken, 0)) / seconds)
			openAvg := int(atomic.LoadInt32(p.open))

			// Throughput carried since the previous tick. A pool whose
			// connections are each working hard should grow even when nobody
			// is asking for new ones — see poolload.go.
			// It is the same figure for every transport because there is one
			// process per tunnel and every one of them counts its bytes into
			// the same place — through CountedConn or through AddBytes.
			mbps := load.mbps()

			// A mux session may be carrying a long transfer without asking for
			// another stream. Retire it only after actual traffic goes quiet.
			quiet := float64(taken+x) < float64(openAvg)*y && mbps < max(openAvg, 1)*(poolScaleMbpsPerConn/2) && (!p.mux || mbps == 0)
			if !quiet {
				quietSince = time.Time{}
			} else if quietSince.IsZero() {
				quietSince = time.Now()
			}

			// The pool is allowed to outgrow its configured size, which from
			// outside is indistinguishable from a leak. Publish what it is
			// doing and why, so the panel can say "8 configured, 19 open,
			// carrying 240 Mbit/s" instead of leaving somebody to guess.
			grow := newPoolSize < ceiling && (((taken + a) > openAvg*b) ||
				load.wantsMore(mbps, openAvg, newPoolSize, p.size))

			switch {
			case grow:
				quietSince = time.Time{}
				next := poolGrowthTarget(newPoolSize, ceiling, taken, b, mbps, p.mux)
				p.log.Debugf("increasing pool size: %d -> %d, avg pool conn: %d, avg load conn: %d, throughput: %d Mbit/s",
					newPoolSize, next, openAvg, taken, mbps)
				if p.pending == nil {
					for i := newPoolSize; i < next; i++ {
						p.launch()
					}
				}
				newPoolSize = next

			case quiet && time.Since(quietSince) >= 30*time.Second && newPoolSize > p.size:
				quietSince = time.Now()
				p.log.Debugf("decreasing pool size: %d -> %d, avg pool conn: %d, avg load conn: %d",
					newPoolSize, newPoolSize-1, openAvg, taken)
				newPoolSize--
				select {
				case p.shrink <- struct{}{}:
				case <-p.ctx.Done():
					return
				default:
					// A full retirement queue already covers future requests.
				}
			}
			// Repairs and growth use one reconciler, so a growth decision does
			// not launch the same missing connections twice.
			if p.pending != nil {
				missing := newPoolSize - int(atomic.LoadInt32(p.open)+atomic.LoadInt32(p.pending))
				for i := 0; i < missing && p.ctx.Err() == nil; i++ {
					p.launch()
				}
			}
			metrics.ReportPool(openAvg, newPoolSize, p.size, mbps)
		}
	}
}

// A burst earns a batch of capacity, bounded by the same socket/memory ceiling
// as control-channel requests. Spare connections do not carry the measured
// traffic, so only physical mux sessions scale on bytes per second.
func poolGrowthTarget(current, ceiling, requests, factor, mbps int, mux bool) int {
	next := max(current+max(current/2, 1), (requests+factor-1)/factor)
	if mux {
		next = max(next, (mbps+poolScaleMbpsPerConn-1)/poolScaleMbpsPerConn)
	}
	return min(next, ceiling)
}
