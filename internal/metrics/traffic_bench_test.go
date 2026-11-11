package metrics

import (
	"sync/atomic"
	"testing"
)

// Keep every update observable to Traffic, as the pool and stall detector
// require. Each worker represents one relay direction; assignment occurs once,
// outside its timed update loop. This isolates accounting CPU, not socket I/O.
func BenchmarkTrafficUpdates(b *testing.B) {
	for _, mode := range []string{"inbound", "outbound", "duplex"} {
		b.Run(mode, func(b *testing.B) {
			var workers atomic.Uint64
			b.ReportAllocs()
			b.RunParallel(func(pb *testing.PB) {
				inbound := mode == "inbound" || (mode == "duplex" && workers.Add(1)%2 == 1)
				if inbound {
					for pb.Next() {
						AddBytes(16384, 0)
					}
				} else {
					for pb.Next() {
						AddBytes(0, 16384)
					}
				}
			})
		})
	}
}
