package manage

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"net"
	"os"
	"strconv"
	"time"

	"github.com/backpack/backpack/config"
)

// This deliberately uses an ordinary TLS client on a separate connection.
// It isolates basic reachability/trust/ALPN from the actual helper's fingerprint,
// authentication and sustained traffic. A successful probe never marks a tunnel OK.
func ctProbeHTTPS(parent context.Context, spec TunnelSpec) string {
	ctx, cancel := context.WithTimeout(parent, 5*time.Second)
	defer cancel()
	if spec.SingClient.Enabled() {
		return "not-applicable" // these protocols are not standalone HTTPS/H2 services
	}
	if spec.ConnectClient.Enabled() && (spec.ConnectClient.Protocol == "webtransport" || spec.ConnectClient.Mode == "h3") {
		return "not-applicable"
	}
	endpoint, serverName, caFile := spec.XrayClient.Server, spec.XrayClient.ServerName, spec.XrayClient.CAFile
	alpn, minimumTLS := "h2", uint16(tls.VersionTLS12)
	if spec.ConnectClient.Enabled() {
		endpoint, serverName, caFile = spec.ConnectClient.Server, spec.ConnectClient.ServerName, spec.ConnectClient.CAFile
		minimumTLS = tls.VersionTLS13
	}
	if spec.ShortClient.Enabled() {
		endpoint, serverName, caFile = spec.ShortClient.Server, spec.ShortClient.ServerName, spec.ShortClient.CAFile
		if serverName == "" {
			serverName, _, _ = net.SplitHostPort(endpoint)
		}
		alpn, minimumTLS = "http/1.1", tls.VersionTLS13
	}
	if spec.NaiveClient.Enabled() {
		endpoint, caFile = spec.NaiveClient.Server, spec.NaiveClient.CAFile
		serverName, _, _ = net.SplitHostPort(endpoint) // Naive uses the IP endpoint, without a DNS SNI
	}
	result := "tcp-failed"
	for ctx.Err() == nil {
		// The Iran engine starts asynchronously. Its port reservation can
		// briefly accept TCP before the TLS helper owns the listener. Retry
		// startup EOF/refusal under the same total budget, without skipping it.
		attempt, stopAttempt := context.WithTimeout(ctx, time.Second)
		conn, err := (&net.Dialer{}).DialContext(attempt, "tcp", endpoint)
		if err == nil {
			if config.XrayREALITY(spec.XrayClient.Mode) {
				conn.Close()
				stopAttempt()
				return "tcp-ok" // only authenticated tunnel traffic can prove REALITY
			}
			result = "tls-failed"
			var roots *x509.CertPool
			if caFile != "" {
				ca, caErr := os.ReadFile(caFile)
				roots = x509.NewCertPool()
				if caErr != nil || !roots.AppendCertsFromPEM(ca) {
					conn.Close()
					stopAttempt()
					return result
				}
			}
			stopClose := context.AfterFunc(attempt, func() { conn.Close() })
			client := tls.Client(conn, &tls.Config{ServerName: serverName, RootCAs: roots, NextProtos: []string{alpn}, MinVersion: minimumTLS})
			err = client.HandshakeContext(attempt)
			conn.Close()
			stopClose()
			stopAttempt()
			if err == nil {
				if client.ConnectionState().NegotiatedProtocol == alpn {
					return "tls-ok"
				}
				return result
			}
			var verification *tls.CertificateVerificationError
			if errors.As(err, &verification) {
				return result
			}
		} else {
			stopAttempt()
		}
		ctSleep(ctx, 100*time.Millisecond)
	}
	return result
}

func ctValidHTTPSProbe(tr, probe string) bool {
	if !managedTransport(tr) {
		return false
	}
	switch probe {
	case "not-applicable":
		return config.SingProtocol(tr) || tr == "webtransport"
	case "tcp-failed":
		return !config.SingProtocol(tr)
	case "tcp-ok":
		return config.XrayREALITY(tr)
	case "tls-ok", "tls-failed":
		return tr == "naive" || tr == "xhttp" || tr == "short-https" || tr == "trusttunnel"
	}
	return false
}

func ctHTTPSProbeLabel(probe string) string {
	switch probe {
	case "not-applicable":
		return "standalone HTTPS probe does not apply; see authenticated tunnel traffic"
	case "tcp-failed":
		return "public TCP connection failed"
	case "tcp-ok":
		return "public TCP reachable; REALITY authentication requires tunnel traffic"
	case "tls-failed":
		return "TCP reachable; certificate/TLS/ALPN probe failed"
	case "tls-ok":
		return "TCP + verified TLS/ALPN passed; this is not a tunnel traffic check"
	}
	return "not reported by peer"
}

func ctHTTPSResult(r ConnTestResult, c *connTestCase) ConnTestResult {
	if c.kind == "reverse" && managedTransport(c.tr) {
		r.HTTPSProbe = c.httpsProbe
		r.TestPort, _ = strconv.Atoi(c.link.Port)
	}
	return r
}
