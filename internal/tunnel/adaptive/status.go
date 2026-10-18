//go:build !windows

package adaptive

import "github.com/backpack/backpack/internal/metrics"

func (c *ClientPaths) Status() metrics.ManagedPathsStats {
	states, selected := c.selector.statusSnapshot()
	s := metrics.ManagedPathsStats{Paths: make([]metrics.ManagedPathStatus, len(states))}
	for i, state := range states {
		p := c.cfg.Paths[i]
		protocol := "naive"
		if p.Connect.Enabled() {
			protocol = p.Connect.Protocol
		}
		if p.Xray.Enabled() {
			protocol = p.Xray.Mode
		}
		if p.Short.Enabled() {
			protocol = "short-https"
		}
		if p.Sing.Enabled() {
			protocol = p.Sing.Protocol
		}
		s.Paths[i] = metrics.ManagedPathStatus{Name: p.Name, Protocol: protocol, HelperReady: c.paths[i].ready.Load(), Healthy: state.Healthy, RTTMillis: state.RTTMillis, Mbps: state.Mbps, FailurePercent: state.FailurePercent, ConsecutiveFailures: state.Failures, Checked: state.Checked}
		s.Paths[i].ProbeSentBytes = c.paths[i].probeSent.Load()
		s.Paths[i].ProbeReceivedBytes = c.paths[i].probeReceived.Load()
		s.Paths[i].ProbeAttempts = c.paths[i].probeAttempts.Load()
		s.Paths[i].NextProbeSeconds = probeDelay(c.policy, state.Healthy, i == selected[0] || i == selected[1]).Seconds()
		c.mu.Lock()
		h := c.paths[i].helper
		c.mu.Unlock()
		if h != nil {
			s.Paths[i].HelperRSSBytes, s.Paths[i].HelperCPUSeconds, s.Paths[i].HelperResourcesMeasured = h.Resources()
		}
	}
	if i := selected[0]; i >= 0 {
		s.Active = c.cfg.Paths[i].Name
	}
	if i := selected[1]; i >= 0 {
		s.NativeUDPActive = c.cfg.Paths[i].Name
	}
	return s
}
