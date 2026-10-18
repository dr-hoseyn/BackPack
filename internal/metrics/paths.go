package metrics

import (
	"sync"
	"time"
)

type ManagedPathStatus struct {
	ProbeSentBytes          uint64    `json:"probe_sent_bytes"`
	ProbeReceivedBytes      uint64    `json:"probe_received_bytes"`
	ProbeAttempts           uint64    `json:"probe_attempts"`
	NextProbeSeconds        float64   `json:"probe_interval_seconds"`
	HelperRSSBytes          uint64    `json:"helper_rss_bytes,omitempty"`
	HelperCPUSeconds        float64   `json:"helper_cpu_seconds,omitempty"`
	HelperResourcesMeasured bool      `json:"helper_resources_measured"`
	Name                    string    `json:"name"`
	Protocol                string    `json:"protocol"`
	HelperReady             bool      `json:"helper_ready"`
	Healthy                 bool      `json:"healthy"`
	RTTMillis               float64   `json:"rtt_ms"`
	Mbps                    float64   `json:"mbps"`
	FailurePercent          float64   `json:"failure_percent"`
	ConsecutiveFailures     int       `json:"consecutive_failures"`
	Checked                 time.Time `json:"checked"`
}

type ManagedPathsStats struct {
	Active          string              `json:"active"`
	NativeUDPActive string              `json:"native_udp_active,omitempty"`
	Paths           []ManagedPathStatus `json:"paths"`
}

var managedPathsState struct {
	sync.Mutex
	next, id uint64
	read     func() ManagedPathsStats
}

func RegisterManagedPaths(read func() ManagedPathsStats) func() {
	managedPathsState.Lock()
	managedPathsState.next++
	id := managedPathsState.next
	managedPathsState.id, managedPathsState.read = id, read
	managedPathsState.Unlock()
	return func() {
		managedPathsState.Lock()
		if managedPathsState.id == id {
			managedPathsState.read = nil
		}
		managedPathsState.Unlock()
	}
}
func managedPathsSnapshot() *ManagedPathsStats {
	managedPathsState.Lock()
	read := managedPathsState.read
	managedPathsState.Unlock()
	if read == nil {
		return nil
	}
	s := read()
	return &s
}
