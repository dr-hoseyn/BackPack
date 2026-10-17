package config

// ShortClientConfig carries TCP Reverse through bounded fresh HTTPS requests.
type ShortClientConfig struct {
	// Server is the public HTTPS host:port. The certificate verifies this host.
	Server string `toml:"server"`
	// CAFile is an optional private PEM trust anchor; empty uses system roots.
	CAFile string `toml:"ca_file,omitempty"`
}

func (s ShortClientConfig) Enabled() bool { return s != (ShortClientConfig{}) }

// ShortServerConfig owns an in-process HTTPS listener, without helper binaries.
type ShortServerConfig struct {
	// Listen is the public TCP host:port, distinct from the private reverse port.
	Listen string `toml:"listen"`
	// ServerName is the certificate's public hostname or IP.
	ServerName string `toml:"server_name"`
	// Certificate is the static PEM chain; excluded when ACME is selected.
	Certificate string `toml:"certificate,omitempty"`
	// Key is its local PEM private key; never exported in setup links.
	Key string `toml:"key,omitempty"`
	// ACMEDomain enables automatic TLS-ALPN certificate issuance/renewal on 443.
	ACMEDomain string `toml:"acme_domain,omitempty"`
	// ACMEEmail is an optional contact for certificate renewal problems.
	ACMEEmail string `toml:"acme_email,omitempty"`
}

func (s ShortServerConfig) Enabled() bool { return s != (ShortServerConfig{}) }
