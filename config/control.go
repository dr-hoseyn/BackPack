package config

// HasTCPControl reports whether reverse address health can check the tunnel's
// actual TCP port. Raw UDP forwarding also has a TCP control channel; KCP,
// QUIC and raw-packet carriers do not. Ping alone cannot validate their service.
func HasTCPControl(tr TransportType) bool {
	switch tr {
	case TCP, TCPMUX, WS, WSS, WSMUX, WSSMUX, STEALTH, UDP:
		return true
	}
	return false
}
