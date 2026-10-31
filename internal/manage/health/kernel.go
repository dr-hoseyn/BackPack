package health

import (
	"context"
	"os/exec"
	"sync"
	"time"

	"github.com/backpack/backpack/config"
)

type kernelObservation struct {
	at        time.Time
	reachable bool
}

var kernelObservations = struct {
	sync.Mutex
	items map[string]kernelObservation
}{items: map[string]kernelObservation{}}

// Bind the probe to the selected interface: a different tunnel or host route
// must not make this service appear connected. Failed/unsupported ping remains
// unknown, not a watchdog restart signal. Cache only for this exact generation.
func kernelPeerReachable(c config.KernelConfig) bool {
	key := c.Interface + "/" + c.LocalIP + "/" + c.PeerIP + "/" + c.Token
	kernelObservations.Lock()
	last, ok := kernelObservations.items[key]
	kernelObservations.Unlock()
	if ok && time.Since(last.at) < 10*time.Second {
		return last.reachable
	}
	ctx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
	defer cancel()
	reachable := exec.CommandContext(ctx, "ping", "-n", "-c", "1", "-W", "1", "-I", c.Interface, c.PeerIP).Run() == nil
	kernelObservations.Lock()
	// Keep this observation cache bounded even after many removed generations.
	if len(kernelObservations.items) > 256 {
		clear(kernelObservations.items)
	}
	kernelObservations.items[key] = kernelObservation{at: time.Now(), reachable: reachable}
	kernelObservations.Unlock()
	return reachable
}
