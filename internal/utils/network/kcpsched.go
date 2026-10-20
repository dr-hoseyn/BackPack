package network

import (
	"runtime"

	"github.com/xtaci/kcp-go/v5"
)

// A single scheduler reduced idle wakeups but serialized every active KCP,
// XDI and PCK session on one CPU. IdleAwareKCP already slows unused sessions;
// active traffic should use every CPU Go can run on, including cgroup quotas.
func init() {
	kcp.SystemTimedSched.Close()
	kcp.SystemTimedSched = kcp.NewTimedSched(max(runtime.GOMAXPROCS(0), 1))
}
