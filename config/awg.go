package config

// AWGObfuscation configures the pinned AmneziaWG wire protocol. Every setting
// is paired; validation bounds padding and excludes standard WireGuard headers.
type AWGObfuscation struct {
	// JunkCount is the bounded number of junk packets before a handshake.
	JunkCount int `json:"junk_count" toml:"junk_count"`
	// JunkMin is the minimum junk packet length.
	JunkMin int `json:"junk_min" toml:"junk_min"`
	// JunkMax is the maximum junk packet length.
	JunkMax int `json:"junk_max" toml:"junk_max"`
	// S1 is initiation packet padding.
	S1 int `json:"s1" toml:"s1"`
	// S2 is response packet padding.
	S2 int `json:"s2" toml:"s2"`
	// S3 is cookie packet padding.
	S3 int `json:"s3" toml:"s3"`
	// S4 is data packet padding.
	S4 int `json:"s4" toml:"s4"`
	// H1 is the disjoint initiation header range.
	H1 string `json:"h1" toml:"h1"`
	// H2 is the disjoint response header range.
	H2 string `json:"h2" toml:"h2"`
	// H3 is the disjoint cookie header range.
	H3 string `json:"h3" toml:"h3"`
	// H4 is the disjoint data header range.
	H4 string `json:"h4" toml:"h4"`
	// HeaderProtectionKey is a canonical base64 shared 32-byte header key.
	HeaderProtectionKey string `json:"header_protection_key" toml:"header_protection_key"`
	// ContentPadding bounds extra data padding to fit the outer MTU budget.
	ContentPadding int `json:"content_padding" toml:"content_padding"`
}

// AWGConfig is an independent AmneziaWG point-to-point engine. Normal configs
// and systemd management use [awg]; the standalone JSON command remains valid.
type AWGConfig struct {
	// Interface is the new exclusive bpawg-prefixed TUN interface.
	Interface string `json:"interface" toml:"interface"`
	// Address is this endpoint's unicast host prefix (/32 or /128).
	Address string `json:"address" toml:"address"`
	// PeerAddress is the peer's unicast host prefix; the only installed route.
	PeerAddress string `json:"peer_address" toml:"peer_address"`
	// ListenPort is the public UDP port (nonzero for a listener).
	ListenPort int `json:"listen_port" toml:"listen_port"`
	// Endpoint is the listener's literal public IP:UDP-port on the dialling side.
	Endpoint string `json:"endpoint,omitempty" toml:"endpoint,omitempty"`
	// PrivateKey is this machine's canonical base64 X25519 private identity.
	PrivateKey string `json:"private_key" toml:"private_key"`
	// PeerPublicKey is the paired endpoint's public X25519 identity.
	PeerPublicKey string `json:"peer_public_key" toml:"peer_public_key"`
	// PresharedKey is the independent shared 32-byte PSK.
	PresharedKey string `json:"preshared_key" toml:"preshared_key"`
	// MTU is the TUN MTU, validated with the worst-case outer IPv6 overhead.
	MTU int `json:"mtu" toml:"mtu"`
	// Keepalive is the persistent keepalive period in seconds (0..120).
	Keepalive int `json:"keepalive" toml:"keepalive"`
	// Obfuscation contains the paired protocol header and padding parameters.
	Obfuscation AWGObfuscation `json:"obfuscation" toml:"obfuscation"`
	// Ports are optional mappings through the peer host route, including TCP.
	Ports []string `json:"ports,omitempty" toml:"ports,omitempty"`
	// AcceptUDP also forwards UDP on the configured ports.
	AcceptUDP bool `json:"accept_udp,omitempty" toml:"accept_udp,omitempty"`
	// MaxConnections bounds forwarded connections (zero is unlimited).
	MaxConnections int `json:"max_connections,omitempty" toml:"max_connections,omitempty"`
	// BandwidthMbps limits forwarded traffic, independent of routed IP traffic.
	BandwidthMbps int `json:"bandwidth_mbps,omitempty" toml:"bandwidth_mbps,omitempty"`
	// PeerSetupKey is the generated peer identity kept only by a pair's creator
	// for regenerating its setup link. It never appears in generic API JSON.
	PeerSetupKey string `json:"-" toml:"peer_setup_key,omitempty"`
}

func (c AWGConfig) Enabled() bool {
	return c.Interface != "" || c.PrivateKey != "" || c.Address != "" || c.Endpoint != "" || c.ListenPort != 0
}
