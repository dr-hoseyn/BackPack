package config

// KernelConfig selects an independent Linux network tunnel. Bare kernel
// methods interoperate with standard endpoints; they do not use L3's Noise wire.
// Reverse wraps the standard tunnel in eight authenticated TCP lanes, except
// WireGuard, whose Kharej endpoint simply initiates its native handshake.
type KernelConfig struct {
	Method         string   `toml:"method"`
	Role           string   `toml:"role"` // iran or kharej; independent of connection direction
	LocalAddr      string   `toml:"local_addr"`
	RemoteAddr     string   `toml:"remote_addr"`
	LocalIP        string   `toml:"local_ip"`
	PeerIP         string   `toml:"peer_ip"`
	Interface      string   `toml:"interface"`
	Token          string   `toml:"token"`
	Port           int      `toml:"port"`
	MTU            int      `toml:"mtu"`
	ID             uint32   `toml:"id"` // shared GRE key, VNI or pseudowire ID
	Lanes          int      `toml:"lanes"`
	Reverse        bool     `toml:"reverse"`
	Ports          []string `toml:"ports"`
	AcceptUDP      bool     `toml:"accept_udp"`
	MaxConnections int      `toml:"max_connections"`
	BandwidthMbps  int      `toml:"bandwidth_mbps"`
}

func (c KernelConfig) Enabled() bool { return c.Method != "" }
