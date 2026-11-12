package manage

import (
	"fmt"

	"github.com/backpack/backpack/config"
	"github.com/backpack/backpack/internal/tui"
)

// SetHealthFailover enables quality-based switching between server addresses.
// IPv4 and IPv6 may be mixed when both reach the same server listener.
func SetHealthFailover(name string, on bool, apply ...func(TunnelSpec) error) error {
	s, err := LoadSpec(name)
	if err != nil {
		return err
	}
	if err := setHealthFailover(&s, on); err != nil {
		return err
	}
	return applySpecUsing(s, apply)
}

func supportsAddressHealth(s *TunnelSpec) bool {
	if config.HasTCPControl(config.TransportType(s.Transport)) {
		return true
	}
	for _, fallback := range s.FallbackTransports {
		if config.HasTCPControl(config.TransportType(fallback)) {
			return true
		}
	}
	return false
}

func setHealthFailover(s *TunnelSpec, on bool) error {
	if s.Role != "client" {
		return fmt.Errorf("automatic address failover is a client-side setting")
	}
	if on {
		if !supportsAddressHealth(s) {
			return fmt.Errorf("health-based switching requires a transport with a TCP control port in the chain")
		}
		clean, err := config.NormalizeFallbackAddrs(s.RemoteAddr, s.FallbackAddrs)
		if err != nil {
			return err
		}
		if len(clean) == 0 {
			return fmt.Errorf("add at least one backup server address first (for IPv4 to IPv6, add the same server's IPv6 address)")
		}
		s.FallbackAddrs = clean
	}
	s.HealthFailover = on
	if on {
		s.LoadBalance = false
	}
	return nil
}

func toggleHealthFailover(name string, spec TunnelSpec) {
	fmt.Println()
	tui.Title("Automatic Address Failover")
	tui.Info("Main    : " + spec.RemoteAddr)
	tui.Info("Backups : " + fallbackSummary(spec.FallbackAddrs))
	tui.Info("Now     : " + onOff(spec.HealthFailover))
	tui.Info("For IPv4 to IPv6: add [IPv6]:port in Backup Addresses first.")
	tui.Info("All addresses must reach the same server, listening on both IP families.")
	tui.Info("Runs while the active transport has a TCP control port.")
	switch config.TransportType(spec.Transport) {
	case config.WS, config.WSS, config.WSMUX, config.WSSMUX:
		tui.Info("WebSocket scores steer new data connections. Update both peers to enable these probes.")
	default:
		tui.Warn("A switch reconnects the tunnel and interrupts existing connections.")
	}
	verb := "Enable"
	if spec.HealthFailover {
		verb = "Disable"
	}
	if !tui.Confirm(verb+" Automatic Address Failover", false) {
		return
	}
	if err := SetHealthFailover(name, !spec.HealthFailover); err != nil {
		tui.Error("Failed: " + err.Error())
	} else {
		tui.Success("Automatic Address Failover: " + onOff(!spec.HealthFailover) + " — Restarted.")
	}
	tui.PressEnter()
}
