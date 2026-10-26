package config

import (
	"fmt"
	"net"
	"net/netip"
	"strconv"
	"strings"
)

// NormalizeFallbackAddrs accepts hosts, IPv4 and bracketed or bare IPv6,
// inherits the primary port, and removes duplicates including the primary.
func NormalizeFallbackAddrs(primary string, backups []string) ([]string, error) {
	_, port, err := net.SplitHostPort(strings.TrimSpace(primary))
	if err != nil {
		return nil, fmt.Errorf("primary address needs host:port: %w", err)
	}
	main, err := normalizeEndpoint(primary, port)
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{main: true}
	var out []string
	for _, addr := range backups {
		if strings.TrimSpace(addr) == "" {
			continue
		}
		full, err := normalizeEndpoint(addr, port)
		if err != nil {
			return nil, err
		}
		if !seen[full] {
			seen[full] = true
			out = append(out, full)
		}
	}
	return out, nil
}

func normalizeEndpoint(addr, defaultPort string) (string, error) {
	raw := strings.TrimSpace(addr)
	host, port, err := net.SplitHostPort(raw)
	if err != nil {
		host, port = raw, defaultPort
		if strings.HasPrefix(host, "[") && strings.HasSuffix(host, "]") {
			host = host[1 : len(host)-1]
		}
	}
	n, err := strconv.Atoi(port)
	if err != nil || n < 1 || n > 65535 {
		return "", fmt.Errorf("invalid port in address %q", raw)
	}
	if ip, err := netip.ParseAddr(host); err == nil {
		host = ip.Unmap().String()
	} else {
		if host == "" || strings.ContainsAny(host, " \t\r\n:/[]\\") {
			return "", fmt.Errorf("invalid server address %q (IPv6 with a port must use [IPv6]:port)", raw)
		}
		host = strings.ToLower(host)
	}
	return net.JoinHostPort(host, strconv.Itoa(n)), nil
}
