// Package carrierdiag reports observations without inferring filtering or
// authentication from a UDP timeout. Reports contain no configuration secrets.
package carrierdiag

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"os"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/backpack/backpack/config"
	"github.com/quic-go/quic-go"
)

type Stage struct {
	Name            string `json:"stage"`
	Status          string `json:"status"`
	Code            string `json:"code"`
	Detail          string `json:"detail"`
	Endpoint        string `json:"endpoint,omitempty"`
	SentPackets     uint64 `json:"sent_packets,omitempty"`
	ReceivedPackets uint64 `json:"received_packets,omitempty"`
}

type Report struct {
	Path     string  `json:"path"`
	Protocol string  `json:"protocol"`
	Verified bool    `json:"verified"`
	Stages   []Stage `json:"stages"`
}

type countingPacketConn struct {
	net.PacketConn
	tx, rx atomic.Uint64
}

func (c *countingPacketConn) ReadFrom(b []byte) (int, net.Addr, error) {
	n, a, err := c.PacketConn.ReadFrom(b)
	if n > 0 {
		c.rx.Add(1)
	}
	return n, a, err
}
func (c *countingPacketConn) WriteTo(b []byte, a net.Addr) (int, error) {
	n, err := c.PacketConn.WriteTo(b, a)
	if n > 0 {
		c.tx.Add(1)
	}
	return n, err
}
func (c *countingPacketConn) SetReadBuffer(n int) error {
	if s, ok := c.PacketConn.(interface{ SetReadBuffer(int) error }); ok {
		return s.SetReadBuffer(n)
	}
	return nil
}
func (c *countingPacketConn) SetWriteBuffer(n int) error {
	if s, ok := c.PacketConn.(interface{ SetWriteBuffer(int) error }); ok {
		return s.SetWriteBuffer(n)
	}
	return nil
}

func TLSConfig(x config.SingClientConfig) (*tls.Config, error) {
	t := &tls.Config{MinVersion: tls.VersionTLS13, ServerName: x.ServerName, NextProtos: []string{"h3"}}
	if x.CAFile != "" {
		roots, err := x509.SystemCertPool()
		if err != nil {
			roots = x509.NewCertPool()
		}
		pem, err := os.ReadFile(x.CAFile)
		if err != nil {
			return nil, err
		}
		if !roots.AppendCertsFromPEM(pem) {
			return nil, errors.New("CA file contains no certificates")
		}
		t.RootCAs = roots
	}
	return t, nil
}

func handshakeFailure(err error, sent, received uint64) (string, string) {
	var unknown x509.UnknownAuthorityError
	var hostname x509.HostnameError
	var invalid x509.CertificateInvalidError
	switch {
	case errors.As(err, &unknown):
		return "tls_untrusted_certificate", "Peer certificate is not trusted; check the paired public CA."
	case errors.As(err, &hostname):
		return "tls_name_mismatch", "Peer certificate does not match the configured TLS name."
	case errors.As(err, &invalid):
		return "tls_invalid_certificate", "Peer certificate failed validity checks."
	case sent == 0:
		return "no_udp_packet_sent", "No UDP packet was sent successfully by this probe."
	case received == 0:
		return "no_udp_reply_observed", "UDP packets were sent, but no reply reached this probe. This does not identify the blocking hop or prove filtering."
	default:
		return "quic_handshake_failed", "UDP replies arrived, but this independent QUIC/TLS handshake did not complete. Authentication has not been tested."
	}
}

func ProbeQUIC(ctx context.Context, endpoint string, t *tls.Config) Stage {
	s := Stage{Name: "quic-tls", Endpoint: endpoint, Status: "fail"}
	a, err := net.ResolveUDPAddr("udp", endpoint)
	if err != nil {
		s.Code, s.Detail = "invalid_endpoint", "Could not resolve the numeric probe endpoint."
		return s
	}
	network := "udp4"
	if a.IP.To4() == nil {
		network = "udp6"
	}
	pc, err := net.ListenPacket(network, ":0")
	if err != nil {
		s.Code, s.Detail = "local_udp_socket_failed", "Could not open a local UDP socket."
		return s
	}
	c := &countingPacketConn{PacketConn: pc}
	transport := &quic.Transport{Conn: c}
	conn, err := transport.Dial(ctx, a, t, &quic.Config{HandshakeIdleTimeout: 3 * time.Second, MaxIdleTimeout: 5 * time.Second})
	if conn != nil {
		conn.CloseWithError(0, "")
	}
	transport.Close()
	pc.Close()
	s.SentPackets, s.ReceivedPackets = c.tx.Load(), c.rx.Load()
	if err == nil {
		s.Status, s.Code, s.Detail = "pass", "verified_quic_tls", "Independent QUIC with verified TLS succeeded. TUIC credentials and tunnel payload are tested separately."
	} else {
		s.Code, s.Detail = handshakeFailure(err, s.SentPackets, s.ReceivedPackets)
	}
	return s
}

func NetworkStages(parent context.Context, x config.SingClientConfig) []Stage {
	if x.Realm.Enabled() {
		return []Stage{{Name: "quic-tls", Status: "skipped", Code: "realm_discovered_endpoint", Detail: "Realm discovers the remote UDP endpoint dynamically. Only the actual helper and authenticated payload checks measure this path."}}
	}
	host, port, err := net.SplitHostPort(x.Server)
	if err != nil {
		return []Stage{{Name: "dns", Status: "fail", Code: "invalid_endpoint", Detail: "Expected a host:port endpoint."}}
	}
	if p, e := strconv.Atoi(port); e != nil || p < 1 || p > 65535 {
		return []Stage{{Name: "dns", Status: "fail", Code: "invalid_port", Detail: "Invalid public endpoint port."}}
	}
	ctx, cancel := context.WithTimeout(parent, 3*time.Second)
	ips, err := net.DefaultResolver.LookupIPAddr(ctx, host)
	cancel()
	if err != nil || len(ips) == 0 {
		return []Stage{{Name: "dns", Status: "fail", Code: "dns_failed", Detail: "No usable address was resolved within the DNS budget."}}
	}
	stages := []Stage{{Name: "dns", Status: "pass", Code: "resolved", Detail: fmt.Sprintf("Resolved %d addresses; probing at most four independently.", len(ips))}}
	if x.Protocol == "hysteria2" && x.ObfsPassword != "" {
		return append(stages, Stage{Name: "quic-tls", Status: "skipped", Code: "salamander_requires_helper", Detail: "Plain QUIC cannot test a Salamander listener. The actual pinned helper is tested next."})
	}
	t, err := TLSConfig(x)
	if err != nil {
		return append(stages, Stage{Name: "quic-tls", Status: "fail", Code: "local_ca_invalid", Detail: "Could not load the configured private CA. TLS verification remains enabled."})
	}
	for _, ip := range ips[:min(len(ips), 4)] {
		ctx, cancel := context.WithTimeout(parent, 4*time.Second)
		stages = append(stages, ProbeQUIC(ctx, net.JoinHostPort(ip.String(), port), t))
		cancel()
		if parent.Err() != nil {
			break
		}
	}
	return stages
}
