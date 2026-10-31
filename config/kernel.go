package config

// KernelConfig selects an independent Linux network tunnel. Bare kernel
// methods interoperate with standard endpoints; they do not use L3's Noise wire.
// Reverse wraps the standard tunnel in eight authenticated TCP lanes, except
// WireGuard, whose Kharej endpoint simply initiates its native handshake.
type KernelConfig struct {
	// Method selects one of the standard kernel L3 or L2 tunnel methods.
	Method string `toml:"method"`
	// Role is iran or kharej and is independent of connection direction.
	Role string `toml:"role"` // iran or kharej; independent of connection direction
	// LocalAddr is this endpoint's literal underlay IP address.
	LocalAddr string `toml:"local_addr"`
	// RemoteAddr is the peer's literal underlay IP address.
	RemoteAddr string `toml:"remote_addr"`
	// LocalIP is this endpoint's tunnel address, including its prefix length.
	LocalIP string `toml:"local_ip"`
	// PeerIP is the peer's tunnel address without a prefix.
	PeerIP string `toml:"peer_ip"`
	// Interface exclusively names this tunnel; empty derives a name from its secret.
	Interface string `toml:"interface"`
	// Token is a random shared secret of at least 24 characters. Bare tunnels remain unencrypted.
	Token string `toml:"token"`
	// Port is the method's UDP port, or the reverse TCP carrier port; zero takes the method default.
	Port int `toml:"port"`
	// MTU is the inner interface MTU; zero takes 1380.
	MTU int `toml:"mtu"`
	// ID is the paired GRE key, VNI or pseudowire ID; zero derives one from Token.
	ID uint32 `toml:"id"` // shared GRE key, VNI or pseudowire ID
	// Lanes selects 1..8 parallel GRE interfaces; ECMP defaults to four.
	Lanes int `toml:"lanes"`
	// Reverse makes Kharej initiate eight encrypted TCP lanes, or native UDP for WireGuard.
	Reverse bool `toml:"reverse"`
	// Ports contains the Iran-side forwarded port mappings; bare targets reach PeerIP.
	Ports []string `toml:"ports"`
	// AcceptUDP enables UDP alongside TCP on forwarded ports.
	AcceptUDP bool `toml:"accept_udp"`
	// MaxConnections caps simultaneous forwarded connections; zero is unlimited.
	MaxConnections int `toml:"max_connections"`
	// BandwidthMbps caps forwarded traffic in Mbit/s; zero is unlimited.
	BandwidthMbps int `toml:"bandwidth_mbps"`
}

func (c KernelConfig) Enabled() bool { return c.Method != "" }
