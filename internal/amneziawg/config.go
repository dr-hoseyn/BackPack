// Package amneziawg provides an opt-in, standalone AmneziaWG point-to-point
// prototype. It does not wrap the existing Noise/GRE tunnel or alter its routes.
package amneziawg

import (
	"bytes"
	"crypto/ecdh"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/backpack/backpack/config"
	"io"
	"net"
	"net/netip"
	"os"
	"regexp"
	"runtime"
	"strconv"
	"strings"
)

const Version = "v3.1.20260828"

type Obfuscation = config.AWGObfuscation

type Config config.AWGConfig

var interfaceName = regexp.MustCompile(`^bpawg[a-zA-Z0-9_-]{0,10}$`)

func keyBytes(value, field string) ([]byte, error) {
	b, err := base64.StdEncoding.DecodeString(value)
	if err != nil || len(b) != 32 || base64.StdEncoding.EncodeToString(b) != value || bytes.Equal(b, make([]byte, 32)) {
		return nil, fmt.Errorf("%s must be a nonzero canonical base64 32-byte key", field)
	}
	return b, nil
}

func hostPrefix(value string) (netip.Prefix, error) {
	p, err := netip.ParsePrefix(value)
	if err != nil || p.Bits() != p.Addr().BitLen() || p.Addr().Is4In6() || !p.Addr().IsGlobalUnicast() {
		return netip.Prefix{}, errors.New("addresses must be unicast host prefixes (/32 or /128)")
	}
	return p, nil
}

func (c Config) Validate() error {
	if !interfaceName.MatchString(c.Interface) {
		return errors.New("interface must start with bpawg and contain at most 15 letters, digits, underscores or hyphens")
	}

	if c.MaxConnections < 0 || c.BandwidthMbps < 0 {
		return errors.New("forwarding limits must be nonnegative")
	}
	if c.PeerSetupKey != "" {
		raw, err := keyBytes(c.PeerSetupKey, "peer_setup_key")
		if err != nil {
			return err
		}
		key, err := ecdh.X25519().NewPrivateKey(raw)
		if err != nil || base64.StdEncoding.EncodeToString(key.PublicKey().Bytes()) != c.PeerPublicKey {
			return errors.New("peer_setup_key does not match peer_public_key")
		}
	}
	a, err := hostPrefix(c.Address)
	if err != nil {
		return err
	}
	p, err := hostPrefix(c.PeerAddress)
	if err != nil {
		return err
	}
	if a.Addr().BitLen() != p.Addr().BitLen() || a == p {
		return errors.New("local and peer addresses must differ and use the same IP family")
	}
	if c.ListenPort < 0 || c.ListenPort > 65535 || c.Keepalive < 0 || c.Keepalive > 120 {
		return errors.New("invalid listen port or keepalive (0..120 seconds)")
	}
	if c.ListenPort == 0 && c.Endpoint == "" {
		return errors.New("a listener requires a nonzero listen_port")
	}
	if c.MTU < 1280 || c.MTU > 1380 {
		return errors.New("prototype MTU must be between 1280 and 1380")
	}
	if c.Endpoint != "" {
		host, port, err := net.SplitHostPort(c.Endpoint)
		ip, ipErr := netip.ParseAddr(host)
		pn, portErr := strconv.Atoi(port)
		if err != nil || ipErr != nil || portErr != nil || pn < 1 || pn > 65535 || ip.IsUnspecified() || ip.IsMulticast() || ip.Is4In6() || ip.Zone() != "" {
			return errors.New("endpoint must be a literal unicast IP:port")
		}
		if ip == p.Addr() || ip == a.Addr() {
			return errors.New("outer endpoint must differ from tunnel addresses")
		}
	}
	private, err := keyBytes(c.PrivateKey, "private_key")
	if err != nil {
		return err
	}
	public, err := keyBytes(c.PeerPublicKey, "peer_public_key")
	if err != nil {
		return err
	}
	pk, _ := ecdh.X25519().NewPrivateKey(private)
	peer, _ := ecdh.X25519().NewPublicKey(public)
	if bytes.Equal(pk.PublicKey().Bytes(), public) {
		return errors.New("peer_public_key must not be this endpoint's own identity")
	}
	if _, err := pk.ECDH(peer); err != nil {
		return errors.New("peer_public_key has invalid order")
	}
	if _, err := keyBytes(c.PresharedKey, "preshared_key"); err != nil {
		return err
	}
	o := c.Obfuscation
	if o.JunkCount < 0 || o.JunkCount > 12 || o.JunkMin < 0 || o.JunkMax < o.JunkMin || o.JunkMax > 1200 {
		return errors.New("invalid bounded junk packet parameters")
	}
	if o.ContentPadding < 0 || o.ContentPadding > 32 {
		return errors.New("content_padding must be between 0 and 32")
	}
	for _, n := range []int{o.S1, o.S2, o.S3, o.S4} {
		if n < 12 || n > 64 {
			return errors.New("s1..s4 must be between 12 and 64 for header protection")
		}
	}
	if _, err := keyBytes(o.HeaderProtectionKey, "header_protection_key"); err != nil {
		return err
	}
	var ranges [][2]uint64
	for _, value := range []string{o.H1, o.H2, o.H3, o.H4} {
		parts := strings.Split(value, "-")
		if len(parts) > 2 {
			return errors.New("invalid packet header range")
		}
		lo, err := strconv.ParseUint(parts[0], 10, 32)
		if err != nil || lo < 5 {
			return errors.New("packet header ranges must exclude standard WireGuard headers 0..4")
		}
		hi := lo
		if len(parts) == 2 {
			hi, err = strconv.ParseUint(parts[1], 10, 32)
			if err != nil {
				return errors.New("invalid packet header range")
			}
		}
		if hi < lo {
			return errors.New("invalid packet header range order")
		}
		for _, prev := range ranges {
			if lo <= prev[1] && hi >= prev[0] {
				return errors.New("packet header ranges must not overlap")
			}
		}
		ranges = append(ranges, [2]uint64{lo, hi})
	}
	// Ensure even worst-case IPv6 encapsulation fits an ordinary 1500-byte path.
	if c.MTU+40+8+32+o.S4+o.ContentPadding+15 > 1500 {
		return errors.New("MTU and padding exceed the prototype's 1500-byte outer packet budget")
	}
	return nil
}

func Load(path string) (Config, error) {
	var c Config
	f, err := os.Open(path)
	if err != nil {
		return c, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return c, err
	}
	if !st.Mode().IsRegular() || st.Size() > 16<<10 {
		return c, errors.New("configuration must be a regular file of at most 16 KiB")
	}
	if runtime.GOOS != "windows" && st.Mode().Perm()&0077 != 0 {
		return c, errors.New("configuration contains private keys; use mode 0600")
	}
	d := json.NewDecoder(io.LimitReader(f, (16<<10)+1))
	d.DisallowUnknownFields()
	if err := d.Decode(&c); err != nil {
		return Config{}, errors.New("invalid AmneziaWG JSON configuration")
	}
	if err := d.Decode(new(any)); err != io.EOF {
		return Config{}, errors.New("unexpected trailing configuration data")
	}
	if err := c.Validate(); err != nil {
		return Config{}, err
	}
	return c, nil
}

// ipcConfig is never logged, printed, or written to a world-readable file.
func (c Config) ipcConfig() (string, error) {
	if err := c.Validate(); err != nil {
		return "", err
	}
	hexKey := func(s string) string { b, _ := base64.StdEncoding.DecodeString(s); return hex.EncodeToString(b) }
	var b strings.Builder
	fmt.Fprintf(&b, "private_key=%s\nlisten_port=%d\nreplace_peers=true\n", hexKey(c.PrivateKey), c.ListenPort)
	o := c.Obfuscation
	fmt.Fprintf(&b, "jc=%d\njmin=%d\njmax=%d\ns1=%d\ns2=%d\ns3=%d\ns4=%d\nh1=%s\nh2=%s\nh3=%s\nh4=%s\nheader_protection_key=%s\ncontent_padding_addition=%d\n", o.JunkCount, o.JunkMin, o.JunkMax, o.S1, o.S2, o.S3, o.S4, o.H1, o.H2, o.H3, o.H4, hexKey(o.HeaderProtectionKey), o.ContentPadding)
	fmt.Fprintf(&b, "public_key=%s\npreshared_key=%s\nreplace_allowed_ips=true\nallowed_ip=%s\npersistent_keepalive_interval=%d\n", hexKey(c.PeerPublicKey), hexKey(c.PresharedKey), c.PeerAddress, c.Keepalive)
	if c.Endpoint != "" {
		fmt.Fprintf(&b, "endpoint=%s\n", c.Endpoint)
	}
	return b.String(), nil
}

func freshKey() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(b), nil
}
