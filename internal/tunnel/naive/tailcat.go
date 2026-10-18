package naive

import (
	"context"
	"crypto/ecdh"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os/exec"
	"regexp"
	"strconv"
	"time"

	"github.com/backpack/backpack/config"
)

func NewTailcatKey() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	b[0] &= 248
	b[31] = (b[31] & 127) | 64
	return base64.StdEncoding.EncodeToString(b), nil
}
func tailcatKey(s string) ([]byte, error) {
	b, err := base64.StdEncoding.DecodeString(s)
	if err != nil || len(b) != 32 || base64.StdEncoding.EncodeToString(b) != s {
		return nil, errors.New("Tailcat keys must be canonical base64 32-byte values")
	}
	return b, nil
}
func TailcatPublic(private string) (public, disco string, err error) {
	b, err := tailcatKey(private)
	if err != nil {
		return "", "", err
	}
	k, err := ecdh.X25519().NewPrivateKey(b)
	if err != nil {
		return "", "", err
	}
	mac := hmac.New(sha256.New, b)
	mac.Write([]byte("github.com/tailscale/tailcat disco key v1"))
	d := mac.Sum(nil)
	d[0] &= 248
	d[31] = (d[31] & 127) | 64
	dk, err := ecdh.X25519().NewPrivateKey(d)
	if err != nil {
		return "", "", err
	}
	return base64.StdEncoding.EncodeToString(k.PublicKey().Bytes()), base64.StdEncoding.EncodeToString(dk.PublicKey().Bytes()), nil
}

func validateTailcat(binary string, t config.TailcatConfig, server bool) error {
	if err := readableFile(binary); err != nil {
		return fmt.Errorf("Tailcat sing-box binary: %w", err)
	}
	if _, err := tailcatKey(t.PrivateKey); err != nil {
		return err
	}
	if _, err := tailcatKey(t.PreSharedKey); err != nil {
		return err
	}
	if server {
		if _, err := tailcatKey(t.ClientKey); err != nil {
			return fmt.Errorf("Tailcat client provisioning key: %w", err)
		}
		if t.PublicKey != "" || t.DiscoKey != "" {
			return errors.New("Tailcat server does not accept outbound peer keys")
		}
		if t.ClientKey == t.PrivateKey {
			return errors.New("Tailcat peers require distinct private keys")
		}
	} else {
		if t.ClientKey != "" {
			return errors.New("Tailcat client cannot contain a provisioning key")
		}
		if _, err := tailcatKey(t.PublicKey); err != nil {
			return err
		}
		if _, err := tailcatKey(t.DiscoKey); err != nil {
			return err
		}
	}
	u, err := url.Parse(t.DERPMapURL)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.Fragment != "" {
		return errors.New("Tailcat requires an explicit verified HTTPS DERP map URL")
	}
	if t.DERPRegion < 0 || t.DERPRegion > 65535 {
		return errors.New("Tailcat DERP region must be 0..65535")
	}
	return nil
}

var tailcatVersion = regexp.MustCompile(`(?i)sing-box version (\d+)\.(\d+)`)

func checkTailcatBinary(ctx context.Context, binary string) error {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, binary, "version").Output()
	if err != nil {
		return fmt.Errorf("read Tailcat helper capability: %w", err)
	}
	m := tailcatVersion.FindSubmatch(out)
	if len(m) != 3 {
		return errors.New("unrecognized sing-box version for Tailcat")
	}
	major, _ := strconv.Atoi(string(m[1]))
	minor, _ := strconv.Atoi(string(m[2]))
	if major < 1 || major == 1 && minor < 15 {
		return errors.New("Tailcat requires its separately installed sing-box 1.15 helper")
	}
	return nil
}

func tailcatClientJSON(x config.SingClientConfig, socks, target, network string) ([]byte, error) {
	if network != "tcp" {
		return nil, errors.New("Tailcat Reverse uses streams; native UDP is not enabled")
	}
	h, p, err := hostPort(socks, true)
	if err != nil {
		return nil, err
	}
	t := x.Tailcat
	out := map[string]any{"type": "tailcat", "tag": "tunnel", "private_key": t.PrivateKey, "server_public_key": t.PublicKey, "server_disco_key": t.DiscoKey, "pre_shared_key": t.PreSharedKey, "derp_map_url": t.DERPMapURL}
	if t.DERPRegion != 0 {
		out["derp_region"] = t.DERPRegion
	}
	route, err := singRoute(target, network, "tunnel")
	if err != nil {
		return nil, err
	}
	return json.Marshal(map[string]any{"log": map[string]any{"level": "warn"}, "inbounds": []any{map[string]any{"type": "socks", "tag": "private", "listen": h, "listen_port": p}}, "outbounds": []any{out}, "route": route})
}

func tailcatServerJSON(x config.SingServerConfig, ready, target, network string) ([]byte, error) {
	if network != "tcp" {
		return nil, errors.New("Tailcat Reverse uses streams; native UDP is not enabled")
	}
	t := x.Tailcat
	client, _, err := TailcatPublic(t.ClientKey)
	if err != nil {
		return nil, err
	}
	in := map[string]any{"type": "tailcat", "tag": "public", "private_key": t.PrivateKey, "pre_shared_key": t.PreSharedKey, "users": []any{map[string]any{"name": "backpack", "public_key": client}}, "derp_map_url": t.DERPMapURL}
	if t.DERPRegion != 0 {
		in["derp_region"] = t.DERPRegion
	}
	ins := []any{in}
	if ready != "" {
		h, p, err := hostPort(ready, true)
		if err != nil {
			return nil, err
		}
		ins = append(ins, map[string]any{"type": "socks", "tag": "readiness", "listen": h, "listen_port": p})
	}
	route, err := singRoute(target, network, "tunnel")
	if err != nil {
		return nil, err
	}
	return json.Marshal(map[string]any{"log": map[string]any{"level": "warn"}, "inbounds": ins, "outbounds": []any{map[string]any{"type": "direct", "tag": "tunnel"}}, "route": route})
}
