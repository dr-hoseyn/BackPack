package naive

import (
	"crypto/x509"
	"errors"
	"fmt"
	"os"

	"github.com/backpack/backpack/config"
)

func validateXHTTPOptions(x config.XrayClientConfig) error {
	if !config.XrayHTTP(x.Mode) {
		if x.MaxConcurrency != 0 || x.MaxConnections != 0 || x.MaxRequests != 0 || x.MaxReuseSeconds != 0 ||
			x.DownloadServer != "" || x.DownloadServerName != "" || x.DownloadHost != "" || x.DownloadPath != "" || x.DownloadCAFile != "" {
			return errors.New("XHTTP download and connection settings require mode = xhttp")
		}
		return nil
	}
	// A TLS download leg needs a different identity/trust contract. Keep the
	// combined REALITY carrier single-security until it is explicitly configured.
	if config.XrayREALITY(x.Mode) && x.DownloadServer != "" {
		return errors.New("a separate TLS download endpoint cannot be combined with XHTTP REALITY")
	}
	for _, v := range []struct {
		name       string
		value, max int
	}{
		{"max_concurrency", x.MaxConcurrency, 1024}, {"max_connections", x.MaxConnections, 64},
		{"max_requests", x.MaxRequests, 100000}, {"max_reuse_seconds", x.MaxReuseSeconds, 86400},
	} {
		if v.value < 0 || v.value > v.max {
			return fmt.Errorf("XHTTP %s must be between 0 and %d", v.name, v.max)
		}
	}
	if x.MaxConcurrency > 0 && x.MaxConnections > 0 {
		return errors.New("XHTTP max_concurrency and max_connections are mutually exclusive")
	}
	if x.DownloadServer == "" {
		if x.DownloadServerName != "" || x.DownloadHost != "" || x.DownloadPath != "" || x.DownloadCAFile != "" {
			return errors.New("XHTTP download settings require download_server")
		}
		return nil
	}
	if _, _, err := xrayEndpoint(x.DownloadServer); err != nil {
		return fmt.Errorf("XHTTP download_server: %w", err)
	}
	if !xrayDNSName(x.DownloadServerName) {
		return errors.New("XHTTP download_server_name must be an explicit valid DNS name")
	}
	path := x.DownloadPath
	if path == "" {
		path = x.Path
	}
	if err := xrayHTTP(path, x.DownloadHost); err != nil {
		return fmt.Errorf("XHTTP download: %w", err)
	}
	if x.DownloadCAFile != "" {
		if err := readableFile(x.DownloadCAFile); err != nil {
			return fmt.Errorf("XHTTP download_ca_file: %w", err)
		}
		b, err := os.ReadFile(x.DownloadCAFile)
		if err != nil {
			return err
		}
		if !x509.NewCertPool().AppendCertsFromPEM(b) {
			return errors.New("XHTTP download_ca_file contains no PEM certificates")
		}
	}
	return nil
}

// xhttpTLS always verifies the endpoint's own identity. A separate download
// endpoint never silently inherits the upload endpoint's private trust anchor.
func xhttpTLS(serverName, caFile string) (map[string]any, error) {
	tls := map[string]any{"serverName": serverName, "alpn": []string{"h2"}, "allowInsecure": false}
	if caFile != "" {
		pem, err := os.ReadFile(caFile)
		if err != nil {
			return nil, err
		}
		if !x509.NewCertPool().AppendCertsFromPEM(pem) {
			return nil, errors.New("XHTTP trust file contains no PEM certificates")
		}
		tls["certificates"] = []any{map[string]any{"certificate": []string{string(pem)}, "usage": "verify"}}
	}
	return tls, nil
}

func xhttpSettings(x config.XrayClientConfig, download bool) (map[string]any, error) {
	path, host := x.Path, x.Host
	if host == "" {
		host = x.ServerName
	}
	if download {
		if x.DownloadPath != "" {
			path = x.DownloadPath
		}
		host = x.DownloadHost
		if host == "" {
			host = x.DownloadServerName
		}
	}
	settings := map[string]any{"path": path, "host": host, "mode": "stream-up"}
	// Preserve all upstream defaults when introducing just one override. Xray
	// replaces its entire default xmux block when any field is explicitly set.
	if x.MaxConcurrency != 0 || x.MaxConnections != 0 || x.MaxRequests != 0 || x.MaxReuseSeconds != 0 {
		mux := map[string]any{"maxConcurrency": 1, "hMaxRequestTimes": "600-900", "hMaxReusableSecs": "1800-3000"}
		if x.MaxConnections > 0 {
			delete(mux, "maxConcurrency")
			mux["maxConnections"] = x.MaxConnections
		}
		if x.MaxConcurrency > 0 {
			mux["maxConcurrency"] = x.MaxConcurrency
		}
		if x.MaxRequests > 0 {
			mux["hMaxRequestTimes"] = x.MaxRequests
		}
		if x.MaxReuseSeconds > 0 {
			mux["hMaxReusableSecs"] = x.MaxReuseSeconds
		}
		settings["xmux"] = mux
	}
	if !download && x.DownloadServer != "" {
		host, port, err := xrayEndpoint(x.DownloadServer)
		if err != nil {
			return nil, err
		}
		tls, err := xhttpTLS(x.DownloadServerName, x.DownloadCAFile)
		if err != nil {
			return nil, err
		}
		down, err := xhttpSettings(x, true)
		if err != nil {
			return nil, err
		}
		settings["downloadSettings"] = map[string]any{"address": host, "port": port, "network": "xhttp", "security": "tls", "tlsSettings": tls, "xhttpSettings": down}
	}
	return settings, nil
}
