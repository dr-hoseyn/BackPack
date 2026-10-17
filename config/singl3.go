package config

// SingL3Config is local helper/TLS configuration for native Hysteria2 and TUIC
// datagram carriers. The L3 token derives independent helper credentials.
type SingL3Config struct {
	// Binary is the absolute pinned sing-box executable path on this machine.
	Binary string `toml:"sing_binary" json:"binary,omitempty"`
	// Target is the listener's fixed numeric loopback UDP target, paired on both ends.
	Target string `toml:"sing_target" json:"target,omitempty"`
	// ServerName is the certificate identity verified by the dialling side.
	ServerName string `toml:"sing_server_name" json:"serverName,omitempty"`
	// CertFile is the PEM TLS chain on the listening side only.
	CertFile string `toml:"sing_cert" json:"certFile,omitempty"`
	// KeyFile is the corresponding private key; never exported in a setup link.
	KeyFile string `toml:"sing_key" json:"keyFile,omitempty"`
	// CAFile optionally trusts a private PEM CA on the dialling side only.
	CAFile string `toml:"sing_ca" json:"caFile,omitempty"`
}

func (s SingL3Config) TargetAddr() string {
	if s.Target == "" {
		return "127.0.0.1:62102"
	}
	return s.Target
}
