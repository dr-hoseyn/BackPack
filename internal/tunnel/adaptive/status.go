//go:build !windows

package adaptive

import "github.com/backpack/backpack/internal/metrics"

func (c *ClientPaths) Status() metrics.ManagedPathsStats {
	states := c.selector.Status()
	s := metrics.ManagedPathsStats{Paths: make([]metrics.ManagedPathStatus, len(states))}
	for i, state := range states {
		p := c.cfg.Paths[i]
		protocol := "naive"
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
	}
	if i := c.selector.Pick(false); i >= 0 {
		s.Active = c.cfg.Paths[i].Name
	}
	if i := c.selector.Pick(true); i >= 0 {
		s.NativeUDPActive = c.cfg.Paths[i].Name
	}
	return s
}
