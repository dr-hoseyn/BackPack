package transport

import (
	"fmt"
	"strings"
	"testing"
)

var parsedBackendSink []string
var firstBackendSink string

// Include oversized peer-supplied targets: the configured member bound must
// bound parsing work too, not just the length of the final group.
func BenchmarkBackendListParsing(b *testing.B) {
	for _, members := range []int{2, backendMaxMembers, 4096} {
		target := strings.Repeat("127.0.0.1:8443|", members)
		b.Run(fmt.Sprint(members), func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				parsedBackendSink = splitBackends(target)
			}
		})
	}
}

func BenchmarkFirstUDPBackend(b *testing.B) {
	for _, members := range []int{1, 32, 4096} {
		target := strings.Repeat("127.0.0.1:8443|", members)
		b.Run(fmt.Sprint(members), func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				firstBackendSink = firstBackend(target)
			}
		})
	}
}
