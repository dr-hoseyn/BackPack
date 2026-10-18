package naive

import (
	"context"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"strings"

	"github.com/backpack/backpack/config"
	"github.com/sirupsen/logrus"
)

// ValidateSing keeps all managed carriers mutually exclusive and refuses
// settings on a role the engine would otherwise silently ignore.
func ValidateSing(c *config.Config) error {
	if !c.Server.Sing.Enabled() && !c.Client.Sing.Enabled() {
		return nil
	}
	if c.Server.Naive.Enabled() || c.Client.Naive.Enabled() || c.Server.Xray.Enabled() || c.Client.Xray.Enabled() || c.Direct.Enabled() || c.L3.Enabled() {
		return errors.New("sing-box reverse carriers require an exclusive reverse role without Naive, Xray, Direct or L3")
	}
	server, client := c.Server.BindAddr != "", c.Client.RemoteAddr != ""
	if server == client || server != c.Server.Sing.Enabled() || client != c.Client.Sing.Enabled() {
		return errors.New("sing-box settings must match exactly one active reverse role")
	}
	if server {
		return ValidateSingServer(&c.Server)
	}
	return ValidateSingClient(&c.Client)
}

func singIdentity(binary, protocol, name, password, uuid, obfs, ss string) error {
	if err := readableFile(binary); err != nil {
		return fmt.Errorf("sing-box binary: %w", err)
	}
	if !config.SingProtocol(protocol) {
		return errors.New("sing-box protocol must be hysteria2, anytls, shadowtls or tuic")
	}
	if name == "" || (net.ParseIP(name) == nil && !xrayDNSName(name)) {
		return errors.New("server_name must be an explicit TLS hostname or IP")
	}
	if len(password) < 16 || len(password) > 1024 || strings.ContainsAny(password, "\r\n\x00") {
		return errors.New("helper password must contain 16..1024 bytes without NUL or line breaks")
	}
	if protocol == "tuic" {
		if len(uuid) != 36 || uuid[8] != '-' || uuid[13] != '-' || uuid[18] != '-' || uuid[23] != '-' {
			return errors.New("TUIC requires a canonical UUID")
		}
		if b, err := hex.DecodeString(strings.ReplaceAll(uuid, "-", "")); err != nil || len(b) != 16 {
			return errors.New("TUIC requires a canonical UUID")
		}
	} else if uuid != "" {
		return errors.New("uuid is valid only for TUIC")
	}
	if obfs != "" && (protocol != "hysteria2" || len(obfs) < 16 || len(obfs) > 1024 || strings.ContainsAny(obfs, "\r\n\x00")) {
		return errors.New("obfs_password requires Hysteria2 and 16..1024 bytes without NUL or line breaks")
	}
	if protocol == "shadowtls" {
		if !xrayDNSName(name) {
			return errors.New("ShadowTLS server_name must be the explicit verified cover hostname")
		}
		b, err := base64.StdEncoding.DecodeString(ss)
		if err != nil || len(b) != 32 || base64.StdEncoding.EncodeToString(b) != ss {
			return errors.New("ShadowTLS requires a canonical base64 32-byte SS2022 key")
		}
	} else if ss != "" {
		return errors.New("shadowsocks_key is valid only for ShadowTLS")
	}
	return nil
}

func ValidateSingClient(c *config.ClientConfig) error {
	x := c.Sing
	if !x.Enabled() {
		return nil
	}
	if err := validateNativeUDP(x.Protocol, x.NativeUDP, x.UDPMaxFlows, x.UDPBandwidthMbps); err != nil {
		return err
	}
	if c.Naive.Enabled() || c.Xray.Enabled() || c.Transport != config.TCP || len(c.FallbackTransports) != 0 || c.Proxy != "" || c.LocalAddr != "" || c.Interface != "" || c.SOMark != 0 || len(c.FallbackAddrs) != 0 || c.HealthFailover || c.LoadBalance {
		return errors.New("sing-box requires TCP reverse without other helpers, routing overrides or fallbacks")
	}
	if _, _, err := hostPort(c.RemoteAddr, true); err != nil {
		return fmt.Errorf("reverse target: %w", err)
	}
	return validateSingClientIdentity(x)
}

func validateSingClientIdentity(x config.SingClientConfig) error {
	if err := validateSingAdvanced(x.Protocol, x.ObfsType, x.ObfsPassword, x.GeckoMin, x.GeckoMax, "", x.ECHConfig, x.BBRProfile, x.Realm, false); err != nil {
		return err
	}
	if _, _, err := xrayEndpoint(x.Server); err != nil {
		return fmt.Errorf("sing-box server: %w", err)
	}
	if err := singIdentity(x.Binary, x.Protocol, x.ServerName, x.Password, x.UUID, x.ObfsPassword, x.ShadowsocksKey); err != nil {
		return err
	}
	if x.CAFile != "" {
		if err := readableFile(x.CAFile); err != nil {
			return err
		}
		pem, err := os.ReadFile(x.CAFile)
		if err != nil {
			return err
		}
		if !x509.NewCertPool().AppendCertsFromPEM(pem) {
			return errors.New("sing-box ca_file contains no PEM certificates")
		}
	}
	return nil
}

func ValidateSingServer(c *config.ServerConfig) error {
	x := c.Sing
	if !x.Enabled() {
		return nil
	}
	if err := validateNativeUDP(x.Protocol, x.NativeUDP, x.UDPMaxFlows, x.UDPBandwidthMbps); err != nil {
		return err
	}
	if x.NativeUDP && (!c.ForwardsUDP() || c.MaxConnections != 0 || c.BandwidthMbps != 0) {
		return errors.New("native_udp requires accept_udp=true and its separate udp_max_flows/udp_bandwidth_mbps limits; remove the stream-wide max_connections/bandwidth_mbps limits")
	}
	if c.Naive.Enabled() || c.Xray.Enabled() || c.Transport != config.TCP || len(c.FallbackTransports) != 0 {
		return errors.New("sing-box requires TCP reverse without other helpers or transport fallbacks")
	}
	if _, _, err := hostPort(c.BindAddr, true); err != nil {
		return fmt.Errorf("reverse target: %w", err)
	}
	if x.Protocol == "shadowtls" {
		h, p, _ := net.SplitHostPort(x.Handshake)
		ip := net.ParseIP(h)
		_, inner, _ := net.SplitHostPort(c.BindAddr)
		if p == inner && (ip != nil && ip.IsLoopback() || strings.EqualFold(h, "localhost")) {
			return errors.New("ShadowTLS cover must differ from the private reverse target")
		}
	}
	if _, p, _ := net.SplitHostPort(c.BindAddr); p != "" {
		if _, outer, _ := net.SplitHostPort(x.Listen); outer == p {
			return errors.New("public helper and private reverse ports must differ")
		}
	}
	return validateSingServerIdentity(x)
}

func validateSingServerIdentity(x config.SingServerConfig) error {
	if err := validateSingAdvanced(x.Protocol, x.ObfsType, x.ObfsPassword, x.GeckoMin, x.GeckoMax, x.ECHKey, x.ECHConfig, x.BBRProfile, x.Realm, true); err != nil {
		return err
	}
	if _, _, err := xrayEndpoint(x.Listen); err != nil {
		return fmt.Errorf("sing-box listen: %w", err)
	}
	if err := singIdentity(x.Binary, x.Protocol, x.ServerName, x.Password, x.UUID, x.ObfsPassword, x.ShadowsocksKey); err != nil {
		return err
	}
	if x.Protocol == "shadowtls" {
		if x.Certificate != "" || x.Key != "" {
			return errors.New("ShadowTLS uses its cover, not a local certificate/key")
		}
		host, port, err := xrayEndpoint(x.Handshake)
		if err != nil {
			return fmt.Errorf("ShadowTLS handshake: %w", err)
		}
		listen, lp, _ := xrayEndpoint(x.Listen)
		if port == lp && (strings.EqualFold(host, listen) || (net.ParseIP(host) != nil && net.ParseIP(host).IsLoopback() && (listen == "0.0.0.0" || listen == "::"))) {
			return errors.New("ShadowTLS cover must differ from its own listener")
		}
		return nil
	}
	if x.Handshake != "" {
		return errors.New("handshake is valid only for ShadowTLS")
	}
	_, _, err := validatedTLSMaterial(x.Certificate, x.Key, x.ServerName, x.Protocol)
	return err
}

func singTLS(name, ca string) (map[string]any, error) {
	t := map[string]any{"enabled": true, "server_name": name, "insecure": false}
	if ca != "" {
		b, err := os.ReadFile(ca)
		if err != nil {
			return nil, err
		}
		t["certificate"] = []string{string(b)}
	}
	return t, nil
}

// These generators are shared with native datagram L3 carriers. Their route
// is an exact numeric loopback address AND port, followed by a reject rule.
// No untrusted SOCKS destination can turn a helper into an open proxy.
func singRoute(target, network, outbound string) (map[string]any, error) {
	host, port, err := hostPort(target, true)
	if err != nil {
		return nil, err
	}
	ip := net.ParseIP(host)
	bits := "/128"
	if ip.To4() != nil {
		bits = "/32"
	}
	var networks any = network
	if network == "both" {
		networks = []string{"tcp", "udp"}
	}
	return map[string]any{"rules": []any{map[string]any{"ip_cidr": []string{ip.String() + bits}, "port": []int{port}, "network": networks, "action": "route", "outbound": outbound}, map[string]any{"action": "reject"}}}, nil
}

func SingClientJSON(x config.SingClientConfig, socks, target, network string) ([]byte, error) {
	if err := validateSingClientIdentity(x); err != nil {
		return nil, err
	}
	if network != "tcp" && ((network != "udp" && network != "both") || !config.SingDatagram(x.Protocol)) {
		return nil, errors.New("native datagrams require Hysteria2 or TUIC")
	}
	h, p, err := hostPort(socks, true)
	if err != nil {
		return nil, err
	}
	server, port, err := xrayEndpoint(x.Server)
	if err != nil {
		return nil, err
	}
	tls, err := singTLS(x.ServerName, x.CAFile)
	if err != nil {
		return nil, err
	}
	out := map[string]any{"type": x.Protocol, "tag": "tunnel", "server": server, "server_port": port, "password": x.Password, "tls": tls}
	if x.ECHConfig != "" {
		tls["ech"] = map[string]any{"enabled": true, "config": []string{x.ECHConfig}}
	}
	if x.Realm.Enabled() {
		out["realm"] = singRealm(x.Realm)
		delete(out, "server")
		delete(out, "server_port")
	}
	if x.BBRProfile != "" {
		out["bbr_profile"] = x.BBRProfile
	}
	outs := []any{out}
	switch x.Protocol {
	case "hysteria2":
		if x.ObfsPassword != "" {
			out["obfs"] = singObfs(x.ObfsType, x.ObfsPassword, x.GeckoMin, x.GeckoMax)
		}
	case "tuic":
		out["uuid"], out["congestion_control"], out["udp_relay_mode"], out["zero_rtt_handshake"] = x.UUID, "bbr", "native", false
		tls["alpn"] = []string{"h3"}
	case "shadowtls":
		out["version"], out["tag"] = 3, "cover"
		tls["utls"] = map[string]any{"enabled": true, "fingerprint": "chrome"}
		outs = append(outs, map[string]any{"type": "shadowsocks", "tag": "tunnel", "server": server, "server_port": port, "method": "2022-blake3-aes-256-gcm", "password": x.ShadowsocksKey, "detour": "cover", "network": "tcp"})
	}
	route, err := singRoute(target, network, "tunnel")
	if err != nil {
		return nil, err
	}
	return json.Marshal(map[string]any{"log": map[string]any{"level": "warn"}, "inbounds": []any{map[string]any{"type": "socks", "listen": h, "listen_port": p, "tag": "private"}}, "outbounds": outs, "route": route})
}

func SingServerJSON(x config.SingServerConfig, ready, target, network string) ([]byte, error) {
	if err := validateSingServerIdentity(x); err != nil {
		return nil, err
	}
	if network != "tcp" && ((network != "udp" && network != "both") || !config.SingDatagram(x.Protocol)) {
		return nil, errors.New("native datagrams require Hysteria2 or TUIC")
	}
	h, p, err := xrayEndpoint(x.Listen)
	if err != nil {
		return nil, err
	}
	in := map[string]any{"type": x.Protocol, "tag": "public", "listen": h, "listen_port": p}
	ins := []any{in}
	if x.Protocol != "shadowtls" {
		cert, key, err := validatedTLSMaterial(x.Certificate, x.Key, x.ServerName, x.Protocol)
		if err != nil {
			return nil, err
		}
		in["tls"] = map[string]any{"enabled": true, "certificate": []string{string(cert)}, "key": []string{string(key)}}
		if x.ECHKey != "" {
			in["tls"].(map[string]any)["ech"] = map[string]any{"enabled": true, "key": []string{x.ECHKey}}
		}
		in["users"] = []any{map[string]any{"name": "backpack", "password": x.Password}}
	}
	switch x.Protocol {
	case "hysteria2":
		// Adaptive BBR avoids inventing a bandwidth ceiling for an unknown path.
		in["ignore_client_bandwidth"] = true
		if x.BBRProfile != "" {
			in["bbr_profile"] = x.BBRProfile
		}
		if x.Realm.Enabled() {
			in["realm"] = singRealm(x.Realm)
		}
		if x.ObfsPassword != "" {
			in["obfs"] = singObfs(x.ObfsType, x.ObfsPassword, x.GeckoMin, x.GeckoMax)
		}
	case "tuic":
		in["users"] = []any{map[string]any{"name": "backpack", "uuid": x.UUID, "password": x.Password}}
		in["congestion_control"], in["zero_rtt_handshake"] = "bbr", false
		in["tls"].(map[string]any)["alpn"] = []string{"h3"}
	case "shadowtls":
		cover, cp, _ := xrayEndpoint(x.Handshake)
		in["version"], in["strict_mode"], in["wildcard_sni"], in["detour"] = 3, true, "off", "ss"
		in["users"] = []any{map[string]any{"name": "backpack", "password": x.Password}}
		in["handshake"] = map[string]any{"server": cover, "server_port": cp}
		// A detour-only inbound has no public listener of its own.
		ins = append(ins, map[string]any{"type": "shadowsocks", "tag": "ss", "method": "2022-blake3-aes-256-gcm", "password": x.ShadowsocksKey, "network": "tcp"})
	}
	if ready != "" {
		rh, rp, err := hostPort(ready, true)
		if err != nil {
			return nil, err
		}
		ins = append(ins, map[string]any{"type": "socks", "tag": "readiness", "listen": rh, "listen_port": rp})
	}
	route, err := singRoute(target, network, "tunnel")
	if err != nil {
		return nil, err
	}
	return json.Marshal(map[string]any{"log": map[string]any{"level": "warn"}, "inbounds": ins, "outbounds": []any{map[string]any{"type": "direct", "tag": "tunnel"}}, "route": route})
}

// StartSingClient uses a private SOCKS listener. A real authenticated transfer
// is still needed after readiness; accepting a local socket is not health.
func StartSingClient(ctx context.Context, c *config.ClientConfig, log *logrus.Logger) (*Helper, error) {
	if err := ValidateSingClient(c); err != nil {
		return nil, err
	}
	network := "tcp"
	if c.Sing.NativeUDP {
		network = "both"
	}
	return StartSingProxy(ctx, c.Sing, c.RemoteAddr, network, log)
}

func StartSingProxy(ctx context.Context, x config.SingClientConfig, target, network string, log *logrus.Logger) (*Helper, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	defer l.Close()
	body, err := SingClientJSON(x, l.Addr().String(), target, network)
	if err != nil {
		return nil, err
	}
	return startManaged(ctx, x.Binary, "sing-client", body, l.Addr().String(), os.Environ(), log, l.Close)
}

func StartSingServer(ctx context.Context, c *config.ServerConfig, log *logrus.Logger) (*Helper, error) {
	if err := ValidateSingServer(c); err != nil {
		return nil, err
	}
	network := "tcp"
	if c.Sing.NativeUDP {
		network = "both"
	}
	return StartSingListener(ctx, c.Sing, c.BindAddr, network, log)
}

func StartSingListener(ctx context.Context, x config.SingServerConfig, target, network string, log *logrus.Logger) (*Helper, error) {
	ready, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	defer ready.Close()
	var reservation interface{ Close() error }
	if config.SingDatagram(x.Protocol) {
		reservation, err = net.ListenPacket("udp", x.Listen)
	} else {
		reservation, err = net.Listen("tcp", x.Listen)
	}
	if err != nil {
		return nil, fmt.Errorf("reserve public %s listener: %w", x.Protocol, err)
	}
	defer reservation.Close()
	body, err := SingServerJSON(x, ready.Addr().String(), target, network)
	if err != nil {
		return nil, err
	}
	return startManaged(ctx, x.Binary, "sing-server", body, ready.Addr().String(), os.Environ(), log, func() error {
		if err := reservation.Close(); err != nil {
			return err
		}
		return ready.Close()
	})
}
