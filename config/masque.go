package config

import (
	"fmt"
	"net"
	"strconv"
	"strings"
)

// MasqueConfig supplies verified TLS for MASQUE, CONNECT-IP and WebTransport.
// The private UDP target applies only to the MASQUE carriers.
// Target is a private loopback UDP socket on the listener, not a public proxy.
type MasqueConfig struct {
	// Target is the listener's private loopback UDP address, identical on both ends.
	// Empty takes 127.0.0.1:62101. Use a different port for each MASQUE tunnel.
	Target string `toml:"masque_target" json:"target,omitempty"`
	// CertFile is the PEM certificate chain on the listening side.
	CertFile string `toml:"masque_cert" json:"certFile,omitempty"`
	// KeyFile is the corresponding PEM private key on the listening side.
	KeyFile string `toml:"masque_key" json:"keyFile,omitempty"`
	// ServerName is the TLS hostname to verify when dialling. Empty uses addr's host.
	ServerName string `toml:"masque_server_name" json:"serverName,omitempty"`
	// CAFile is an optional PEM trust bundle for a private CA on the dialling side.
	// Empty uses the system trust store. Certificate verification is always enabled.
	CAFile string `toml:"masque_ca" json:"caFile,omitempty"`
}

func (m MasqueConfig) TargetAddr() string {
	if m.Target == "" {
		return "127.0.0.1:62101"
	}
	host, port, err := net.SplitHostPort(m.Target)
	ip := net.ParseIP(host)
	n, numErr := strconv.Atoi(port)
	if err == nil && ip != nil && numErr == nil {
		return net.JoinHostPort(ip.String(), strconv.Itoa(n))
	}
	return m.Target
}

// Validate rejects externally reachable proxy targets and incomplete TLS settings.
func (m MasqueConfig) Validate(listen bool) error {
	host, service, err := net.SplitHostPort(m.TargetAddr())
	ip := net.ParseIP(host)
	port, portErr := strconv.Atoi(service)
	if err != nil || ip == nil || !ip.IsLoopback() || portErr != nil || port < 1 || port > 65535 {
		return fmt.Errorf("masque_target must be a numeric loopback IP:port (1..65535)")
	}
	if listen && (strings.TrimSpace(m.CertFile) == "" || strings.TrimSpace(m.KeyFile) == "") {
		return fmt.Errorf("MASQUE listening requires masque_cert and masque_key; the dialler verifies the TLS certificate")
	}
	return nil
}
