package manage

// TunnelTransportLabel is presentation only. Managed reverse protocols use
// TCP internally; changing the discovered engine transport would also change
// health checks and edit behavior, so resolve the public name separately.
func TunnelTransportLabel(t Tunnel) string {
	transport := t.Transport
	if !IsDirectKind(t) {
		if spec, err := LoadSpec(t.Name); err == nil {
			transport = selectedTransport(spec)
		}
	}
	return transportLabel(transport)
}
