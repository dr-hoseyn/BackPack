package config

// ConnectClientConfig carries the Reverse engine through authenticated HTTP
// streams. These carriers run in process and never expose a general proxy.
type ConnectClientConfig struct {
	// Protocol selects trusttunnel or webtransport.
	Protocol string `toml:"protocol"`
	// Mode selects TrustTunnel auto, h2 or h3; WebTransport uses h3.
	Mode string `toml:"mode,omitempty"` // TrustTunnel: auto, h2 or h3; WebTransport: h3.
	// Server is the paired public endpoint, as host:port.
	Server string `toml:"server"`
	// ServerName is the explicit verified certificate hostname or IP.
	ServerName string `toml:"server_name"`
	// Username is the carrier account, separate from the Reverse token.
	Username string `toml:"username"`
	// Password is the carrier account secret, containing at least 16 bytes.
	Password string `toml:"password"`
	// CAFile optionally supplies private trust; empty uses system roots.
	CAFile string `toml:"ca_file,omitempty"`
	// Path is the paired private WebTransport session endpoint, unused by TrustTunnel.
	Path string `toml:"path,omitempty"` // WebTransport session endpoint.
}

func (c ConnectClientConfig) Enabled() bool { return c != (ConnectClientConfig{}) }

type ConnectServerConfig struct {
	// Protocol selects trusttunnel or webtransport.
	Protocol string `toml:"protocol"`
	// Mode selects TrustTunnel auto, h2 or h3; WebTransport uses h3.
	Mode string `toml:"mode,omitempty"`
	// Listen is the public bind address. TrustTunnel auto reserves both TCP and UDP.
	Listen string `toml:"listen"`
	// ServerName must match the local certificate identity.
	ServerName string `toml:"server_name"`
	// Username identifies the single paired carrier account.
	Username string `toml:"username"`
	// Password authenticates the carrier independently of the Reverse engine.
	Password string `toml:"password"`
	// Certificate is the local PEM certificate chain.
	Certificate string `toml:"certificate"`
	// Key is its local PEM private key and never enters a Setup Link.
	Key string `toml:"key"`
	// Path is the shared private WebTransport path; TrustTunnel leaves it empty.
	Path string `toml:"path,omitempty"`
}

func (c ConnectServerConfig) Enabled() bool { return c != (ConnectServerConfig{}) }
func ConnectProtocol(p string) bool         { return p == "trusttunnel" || p == "webtransport" }
