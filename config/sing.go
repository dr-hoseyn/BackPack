package config

// SingClientConfig carries the reverse engine through a restricted sing-box
// helper. The helper is a carrier, never a general-purpose proxy.
type SingClientConfig struct {
	// Tailcat is the experimental point-to-point identity and explicit DERP selection.
	Tailcat TailcatConfig `toml:"tailcat,omitempty"`
	// ObfsType is salamander (legacy default) or gecko; Hysteria2 only.
	ObfsType string `toml:"obfs_type,omitempty"`
	// GeckoMin/Max bound on-wire padded packets (512..1400); zero uses 512/1200.
	GeckoMin int `toml:"gecko_min,omitempty"`
	// GeckoMax bounds the maximum Gecko on-wire packet size; zero uses 1200.
	GeckoMax int `toml:"gecko_max,omitempty"`
	// ECHConfig contains public PEM ECH CONFIGS. TLS verification remains enabled.
	ECHConfig string `toml:"ech_config,omitempty"`
	// Realm optionally discovers the listening Hysteria2 peer through an HTTPS rendezvous.
	Realm HysteriaRealm `toml:"realm,omitempty"`
	// BBRProfile is standard, conservative or aggressive; empty uses standard.
	BBRProfile string `toml:"bbr_profile,omitempty"`
	// NativeUDP carries reverse forwarded UDP as native datagrams (Hysteria2/TUIC only); paired.
	NativeUDP bool `toml:"native_udp,omitempty"`
	// UDPMaxFlows caps native associations on this machine; 0 uses 128, maximum 4096.
	UDPMaxFlows int `toml:"udp_max_flows,omitempty"`
	// UDPBandwidthMbps caps native UDP separately, dropping excess packets; 0 is unlimited.
	UDPBandwidthMbps int `toml:"udp_bandwidth_mbps,omitempty"`
	// Binary is the absolute path to the pinned sing-box executable.
	Binary string `toml:"binary"`
	// Protocol is hysteria2, anytls, shadowtls (v3 with SS2022), or tuic (v5).
	Protocol string `toml:"protocol"`
	// Server is the public helper endpoint, as a hostname or IP and port.
	Server string `toml:"server"`
	// ServerName is the identity verified by TLS, including the cover for ShadowTLS.
	ServerName string `toml:"server_name"`
	// Password authenticates this helper separately from the reverse token.
	Password string `toml:"password"`
	// UUID identifies the TUIC account; unused for the other protocols.
	UUID string `toml:"uuid,omitempty"`
	// ObfsPassword enables Hysteria2 Salamander when nonempty; must match the server.
	ObfsPassword string `toml:"obfs_password,omitempty"`
	// ShadowsocksKey is a canonical base64 32-byte SS2022 key for ShadowTLS only.
	ShadowsocksKey string `toml:"shadowsocks_key,omitempty"`
	// CAFile optionally adds a private PEM trust anchor; TLS verification stays on.
	CAFile string `toml:"ca_file,omitempty"`
}

func (s SingClientConfig) Enabled() bool { return s != (SingClientConfig{}) }

// SingServerConfig owns one public helper and only permits the paired local
// reverse target. ShadowTLS delegates to an SS2022 inbound inside the helper.
type SingServerConfig struct {
	// Tailcat provisions one authorized peer with a separately pinned helper.
	Tailcat TailcatConfig `toml:"tailcat,omitempty"`
	// ObfsType selects salamander (legacy default) or gecko for Hysteria2.
	ObfsType string `toml:"obfs_type,omitempty"`
	// GeckoMin is the minimum padded packet size; zero uses 512.
	GeckoMin int `toml:"gecko_min,omitempty"`
	// GeckoMax is the maximum padded packet size; zero uses 1200.
	GeckoMax int `toml:"gecko_max,omitempty"`
	// ECHKey is private PEM ECH KEYS, retained only in the server configuration.
	ECHKey string `toml:"ech_key,omitempty" json:"-"`
	// ECHConfig is the matching public PEM ECH CONFIGS exported to the peer.
	ECHConfig string `toml:"ech_config,omitempty"`
	// Realm registers this listener with an existing HTTPS rendezvous service.
	Realm HysteriaRealm `toml:"realm,omitempty"`
	// BBRProfile selects standard, conservative or aggressive for Hysteria2.
	BBRProfile string `toml:"bbr_profile,omitempty"`
	// NativeUDP selects native reverse UDP rather than reliable stream forwarding; paired.
	NativeUDP bool `toml:"native_udp,omitempty"`
	// UDPMaxFlows caps native forwarded flows; 0 uses 128, maximum 4096.
	UDPMaxFlows int `toml:"udp_max_flows,omitempty"`
	// UDPBandwidthMbps caps native UDP in Mbit/s; 0 is unlimited.
	UDPBandwidthMbps int `toml:"udp_bandwidth_mbps,omitempty"`
	// Binary is the absolute path to the pinned sing-box executable.
	Binary string `toml:"binary"`
	// Protocol is hysteria2, anytls, shadowtls, or tuic.
	Protocol string `toml:"protocol"`
	// Listen is the public helper bind address, separate from the reverse port.
	Listen string `toml:"listen"`
	// ServerName is the TLS identity, or the verified cover identity for ShadowTLS.
	ServerName string `toml:"server_name"`
	// Password is the helper account secret, independent of the reverse token.
	Password string `toml:"password"`
	// UUID is the canonical TUIC v5 account identity.
	UUID string `toml:"uuid,omitempty"`
	// ObfsPassword enables Hysteria2 Salamander; empty keeps ordinary QUIC framing.
	ObfsPassword string `toml:"obfs_password,omitempty"`
	// ShadowsocksKey is the base64 32-byte SS2022 key, required for ShadowTLS v3.
	ShadowsocksKey string `toml:"shadowsocks_key,omitempty"`
	// Handshake is the explicit ShadowTLS cover endpoint; no wildcard SNI routing.
	Handshake string `toml:"handshake,omitempty"`
	// Certificate is the verified PEM certificate chain; unused for ShadowTLS.
	Certificate string `toml:"certificate,omitempty"`
	// Key is the PEM private key, which remains on this server.
	Key string `toml:"key,omitempty"`
}

func (s SingServerConfig) Enabled() bool { return s != (SingServerConfig{}) }

func SingProtocol(s string) bool {
	switch s {
	case "hysteria2", "anytls", "shadowtls", "tuic", "tailcat":
		return true
	}
	return false
}

// SingDatagram reports protocols whose native datagrams can carry an L3 tunnel.
// AnyTLS and ShadowTLS are reliable streams and cannot be L3 carriers.
func SingDatagram(s string) bool { return s == "hysteria2" || s == "tuic" }

func (c ClientConfig) Managed() bool {
	return len(c.Paths) != 0 || c.Connect.Enabled() || c.Short.Enabled() || c.Naive.Enabled() || c.Xray.Enabled() || c.Sing.Enabled()
}
func (c ServerConfig) Managed() bool {
	return len(c.Paths) != 0 || c.Connect.Enabled() || c.Short.Enabled() || c.Naive.Enabled() || c.Xray.Enabled() || c.Sing.Enabled()
}
