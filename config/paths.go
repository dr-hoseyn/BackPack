package config

// ManagedServerPath owns one public carrier to the same private Reverse target.
// Exactly one of naive/xray/sing/short must be set. Names pair the two machines.
type ManagedServerPath struct {
	// Name is the stable path identity, unique within this tunnel.
	Name string `toml:"name"`
	// Endpoint optionally overrides the public host:port exported to the peer.
	// Empty uses the setup link's public host and this path's listener port.
	Endpoint string `toml:"endpoint,omitempty"`
	// Naive is this path's optional Naive listener and local TLS material.
	Naive NaiveServerConfig `toml:"naive,omitempty"`
	// Xray is this path's optional XHTTP or REALITY listener.
	Xray XrayServerConfig `toml:"xray,omitempty"`
	// Sing is this path's optional Hysteria2, TUIC, AnyTLS or ShadowTLS listener.
	Sing SingServerConfig `toml:"sing,omitempty"`
	// Short is this path's optional bounded-request HTTPS listener.
	Short ShortServerConfig `toml:"short,omitempty"`
	// Connect is this path's restricted TrustTunnel or WebTransport listener.
	Connect ConnectServerConfig `toml:"connect,omitempty"`
}

type ManagedClientPath struct {
	// Name matches the paired server path; it is used in health reports.
	Name string `toml:"name"`
	// Naive contains the paired public endpoint, credentials and local helper path.
	Naive NaiveClientConfig `toml:"naive,omitempty"`
	// Xray contains the paired XHTTP/REALITY endpoint and verified identity.
	Xray XrayClientConfig `toml:"xray,omitempty"`
	// Sing contains the paired sing-box endpoint and verified identity.
	Sing SingClientConfig `toml:"sing,omitempty"`
	// Short contains the paired bounded-request endpoint and local trust file.
	Short ShortClientConfig `toml:"short,omitempty"`
	// Connect contains the paired in-process HTTP carrier endpoint and trust.
	Connect ConnectClientConfig `toml:"connect,omitempty"`
}

// PathPolicy governs new TCP connections; active forwards remain on their path.
type PathPolicy struct {
	// StandbyInterval slows verified probes on healthy unselected paths; 0 keeps ProbeInterval.
	StandbyInterval int `toml:"standby_interval,omitempty"`
	// ProbeInterval is seconds between verified transfers per path; 0 uses 5.
	ProbeInterval int `toml:"probe_interval,omitempty"`
	// ProbeTimeout is the total seconds for a verified transfer; 0 uses 5.
	ProbeTimeout int `toml:"probe_timeout,omitempty"`
	// ProbeBytes is verified in each direction; 0 uses 16384, range 1024..65536.
	ProbeBytes int `toml:"probe_bytes,omitempty"`
	// Failures is consecutive failures before a healthy path is withdrawn; 0 uses 2.
	Failures int `toml:"failures,omitempty"`
	// Recoveries is verified successes required for a failed path to return; 0 uses 2.
	Recoveries int `toml:"recoveries,omitempty"`
	// MinDwell is minimum seconds before leaving a healthy path; 0 uses 30.
	MinDwell int `toml:"min_dwell,omitempty"`
	// Improvement is minimum score improvement in percent for a healthy switch; 0 uses 20.
	Improvement int `toml:"improvement,omitempty"`
}

func (c ServerConfig) NativeSing() SingServerConfig {
	if c.Sing.NativeUDP {
		return c.Sing
	}
	for _, p := range c.Paths {
		if p.Sing.NativeUDP {
			return p.Sing
		}
	}
	return SingServerConfig{}
}
func (c ClientConfig) NativeSing() SingClientConfig {
	if c.Sing.NativeUDP {
		return c.Sing
	}
	for _, p := range c.Paths {
		if p.Sing.NativeUDP {
			return p.Sing
		}
	}
	return SingClientConfig{}
}
