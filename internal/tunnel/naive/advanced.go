package naive

import (
	"bytes"
	"context"
	"encoding/pem"
	"errors"
	"github.com/backpack/backpack/config"
	"net"
	"net/url"
	"os/exec"
	"regexp"
	"strings"
	"time"
)

var realmIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,63}$`)

func validateSingAdvanced(protocol, obfs, password string, min, max int, echKey, echConfig, bbr string, realm config.HysteriaRealm, server bool) error {
	if obfs != "" && obfs != "salamander" && obfs != "gecko" {
		return errors.New("obfs_type must be salamander or gecko")
	}
	if obfs != "" && password == "" {
		return errors.New("obfs_type requires obfs_password")
	}
	if (obfs != "" || min != 0 || max != 0 || bbr != "" || realm.Enabled()) && protocol != "hysteria2" {
		return errors.New("obfuscation, BBR profile and Realm require Hysteria2")
	}
	if bbr != "" && bbr != "standard" && bbr != "conservative" && bbr != "aggressive" {
		return errors.New("bbr_profile must be standard, conservative or aggressive")
	}
	if min != 0 || max != 0 {
		if obfs != "gecko" {
			return errors.New("gecko_min/max require Gecko")
		}
		if min == 0 {
			min = 512
		}
		if max == 0 {
			max = 1200
		}
		if min < 512 || max > 1400 || min > max {
			return errors.New("Gecko packet bounds must satisfy 512 <= min <= max <= 1400")
		}
	}
	if echKey != "" || echConfig != "" {
		if protocol != "hysteria2" && protocol != "anytls" && protocol != "tuic" {
			return errors.New("ECH requires Hysteria2, AnyTLS or TUIC")
		}
		if !validECHPEM(echConfig, "ECH CONFIGS") || (server && !validECHPEM(echKey, "ECH KEYS")) || (!server && echKey != "") {
			return errors.New("ECH requires public ECH CONFIGS and a server-only ECH KEYS PEM pair")
		}
	}
	if realm.Enabled() {
		u, err := url.Parse(realm.ServerURL)
		if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
			return errors.New("Realm server_url must be HTTPS without credentials, query or fragment")
		}
		if !realmIDPattern.MatchString(realm.RealmID) || len(realm.Token) < 16 || len(realm.Token) > 1024 || strings.ContainsAny(realm.Token, "\r\n\x00") {
			return errors.New("Realm requires a 1..64 ASCII realm_id and independent 16..1024 byte token")
		}
		if realm.IPVersion != 0 && realm.IPVersion != 4 && realm.IPVersion != 6 {
			return errors.New("Realm ip_version must be 0, 4 or 6")
		}
		servers := strings.Split(realm.STUNServers, ",")
		if len(servers) > 8 {
			return errors.New("Realm supports at most eight explicit STUN endpoints")
		}
		for _, endpoint := range servers {
			if _, _, err := hostPort(strings.TrimSpace(endpoint), false); err != nil {
				return errors.New("Realm stun_servers must list explicit host:port endpoints")
			}
		}
	}
	return nil
}

func validECHPEM(value, kind string) bool {
	if len(value) == 0 || len(value) > 16384 {
		return false
	}
	block, rest := pem.Decode([]byte(value))
	return block != nil && block.Type == kind && len(block.Bytes) >= 8 && len(bytes.TrimSpace(rest)) == 0 && len(block.Headers) == 0
}

// GenerateECH delegates key generation to the same pinned TLS implementation
// that consumes the keys. Output stays in memory and never enters logs/argv.
func GenerateECH(ctx context.Context, binary, publicName string) (public, private string, err error) {
	if !xrayDNSName(publicName) || net.ParseIP(publicName) != nil {
		return "", "", errors.New("ECH public name must be a DNS hostname")
	}
	if err = readableFile(binary); err != nil {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, binary, "generate", "ech-keypair", publicName)
	var output limitedECHOutput
	cmd.Stdout = &output
	if err = cmd.Run(); err != nil {
		return "", "", errors.New("pinned helper could not generate ECH keys")
	}
	remaining := output.Bytes()
	for len(remaining) > 0 {
		var b *pem.Block
		b, remaining = pem.Decode(remaining)
		if b == nil {
			return "", "", errors.New("invalid ECH key generator response")
		}
		switch b.Type {
		case "ECH CONFIGS":
			public = string(pem.EncodeToMemory(b))
		case "ECH KEYS":
			private = string(pem.EncodeToMemory(b))
		default:
			return "", "", errors.New("unknown ECH generator block")
		}
	}
	if !validECHPEM(public, "ECH CONFIGS") || !validECHPEM(private, "ECH KEYS") {
		return "", "", errors.New("incomplete ECH key generator response")
	}
	return
}

type limitedECHOutput struct{ bytes.Buffer }

func (w *limitedECHOutput) Write(p []byte) (int, error) {
	if w.Len()+len(p) > 32768 {
		return 0, errors.New("ECH generator output too large")
	}
	return w.Buffer.Write(p)
}

func singObfs(kind, password string, min, max int) map[string]any {
	if kind == "" {
		kind = "salamander"
	}
	v := map[string]any{"type": kind, "password": password}
	if kind == "gecko" {
		if min != 0 {
			v["min_packet_size"] = min
		}
		if max != 0 {
			v["max_packet_size"] = max
		}
	}
	return v
}
func singRealm(r config.HysteriaRealm) map[string]any {
	servers := strings.Split(r.STUNServers, ",")
	for i := range servers {
		servers[i] = strings.TrimSpace(servers[i])
	}
	return map[string]any{"server_url": r.ServerURL, "token": r.Token, "realm_id": r.RealmID, "stun_servers": servers, "ip_version": r.IPVersion}
}
