package config

import (
	"fmt"
	"strings"
)

// CarrierOptions configures stream carriers without changing the reverse engine.
// SSH credentials belong on the dialing host; setup links never contain them.
type CarrierOptions struct {
	// SNI sent by TLS and gRPC clients; empty uses the endpoint hostname.
	ServerName string `toml:"server_name" json:"serverName,omitempty"`
	// TLS/gRPC browser profile: chrome, firefox, safari, edge, or random (choose a browser per connection).
	Fingerprint string `toml:"fingerprint" json:"fingerprint,omitempty"`
	// Best-effort splitting of the first TLS/gRPC ClientHello write; does not guarantee packet boundaries.
	TLSFragment bool `toml:"tls_fragment" json:"tlsFragment,omitempty"`
	// gRPC service name, identical on both peers; empty uses Backpack.
	GRPCService string `toml:"grpc_service" json:"grpcService,omitempty"`
	// The remote sshd port used by an SSH client; zero uses 22.
	SSHPort int `toml:"ssh_port" json:"sshPort,omitempty"`
	// The account on the remote sshd used for TCP forwarding.
	SSHUser string `toml:"ssh_user" json:"sshUser,omitempty"`
	// Optional SSH password on the client; omitted from setup links and generic JSON responses.
	SSHPassword string `toml:"ssh_password" json:"-"`
	// Path to an unencrypted SSH private key on the client; not copied to another server.
	SSHKeyFile string `toml:"ssh_key" json:"sshKey,omitempty"`
	// Required SHA256 host-key fingerprint of the remote sshd.
	SSHHostKey string `toml:"ssh_host_key" json:"sshHostKey,omitempty"`
	// Loopback forwarding destination on the remote server; empty uses 127.0.0.1 and the tunnel port.
	SSHRemote string `toml:"ssh_remote" json:"sshRemote,omitempty"`
}

func (o CarrierOptions) Validate(tr TransportType, client bool) error {
	switch strings.ToLower(o.Fingerprint) {
	case "", "chrome", "firefox", "safari", "edge", "random":
	default:
		return fmt.Errorf("fingerprint must be chrome, firefox, safari, edge or random")
	}
	for _, r := range o.GRPCService {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_' || r == '.') {
			return fmt.Errorf("grpc_service must contain only letters, digits, _ and .")
		}
	}

	if o.SSHPort < 0 || o.SSHPort > 65535 {
		return fmt.Errorf("ssh_port must be between 1 and 65535")
	}
	if tr == SSH && client {
		if o.SSHUser == "" || (o.SSHPassword == "" && o.SSHKeyFile == "") {
			return fmt.Errorf("SSH requires ssh_user and ssh_password or ssh_key")
		}
		if !strings.HasPrefix(o.SSHHostKey, "SHA256:") {
			return fmt.Errorf("SSH requires the server's SHA256 ssh_host_key fingerprint")
		}
	}
	return nil
}
