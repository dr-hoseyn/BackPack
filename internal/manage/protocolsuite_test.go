package manage

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/backpack/backpack/config"
	"github.com/backpack/backpack/internal/amneziawg"
	"github.com/backpack/backpack/internal/app"
)

func TestSingSetupLinksPreservePairedSettingsAndNeverExportLocalSecrets(t *testing.T) {
	bin, cert, key := managedWizardFixture(t)
	for _, protocol := range []string{"hysteria2", "anytls", "shadowtls", "tuic"} {
		t.Run(protocol, func(t *testing.T) {
			x := config.SingServerConfig{Binary: bin, Protocol: protocol, Listen: "0.0.0.0:443", ServerName: "localhost", Password: "helper-secret-long-enough", Certificate: cert, Key: key}
			if protocol == "tuic" {
				x.UUID = "12345678-1234-4234-8234-123456789abc"
			}
			if protocol == "hysteria2" {
				x.ObfsPassword = "separate-salamander-secret"
			}
			if protocol == "shadowtls" {
				x.Certificate, x.Key = "", ""
				x.Handshake = "cover.example.com:443"
				x.ShadowsocksKey = base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{9}, 32))
			}
			spec := TunnelSpec{Role: "server", Name: "protocol-test", Transport: "tcp", BindAddr: "127.0.0.1:3080", Token: "separate-backpack-token", Ports: []string{"8080=127.0.0.1:8081"}, SingServer: x}
			if err := validateManagedSpec(spec); err != nil {
				t.Fatal(err)
			}
			cfg := decode(t, spec.Render())
			if cfg.Server.Sing != x {
				t.Fatal("render lost carrier fields")
			}
			raw, err := shareLinkOf(spec.Name, "203.0.113.1", cfg)
			if err != nil {
				t.Fatal(err)
			}
			link, err := DecodeShareLink(raw)
			if err != nil {
				t.Fatal(err)
			}
			peer := reverseClientFromLink(link, "203.0.113.1")
			if peer.SingClient.Protocol != protocol || peer.SingClient.Password != x.Password || peer.SingClient.UUID != x.UUID || peer.SingClient.ObfsPassword != x.ObfsPassword || peer.SingClient.ShadowsocksKey != x.ShadowsocksKey || peer.SingClient.ServerName != x.ServerName || peer.RemoteAddr != spec.BindAddr || peer.Token != spec.Token {
				t.Fatal("peer does not match listener")
			}
			if selectedTransport(peer) != protocol || managedEndpoint(peer) != "203.0.113.1:443" {
				t.Fatal("menu would display the private target")
			}
			public, _ := json.Marshal(spec)
			if strings.Contains(string(public), x.Password) || strings.Contains(string(public), bin) || strings.Contains(string(public), key) {
				t.Fatal("generic spec JSON leaked helper settings")
			}
			clearManagedTransport(&peer)
			if peer.SingClient.Enabled() {
				t.Fatal("switching away retained helper")
			}
		})
	}
}

func TestNativeDatagramSettingsSurviveEditsAndMirrorOnlyPublicTLSSettings(t *testing.T) {
	for _, protocol := range []string{"hysteria2", "tuic"} {
		s := l3Spec{Name: "native-test", Side: sideKharej, Carrier: protocol, Addr: "0.0.0.0:443", Token: "shared-token", LocalIP: "10.20.0.2/30", PeerIP: "10.20.0.1", Sing: config.SingL3Config{Binary: "/private/sing-box", Target: "127.0.0.1:62123", ServerName: "kharej.example.com", CertFile: "/private/server.crt", KeyFile: "/private/server.key"}}
		cfg := decode(t, s.Render())
		if !reflect.DeepEqual(cfg.L3.SingL3Config, s.Sing) {
			t.Fatal("native renderer lost helper identity")
		}
		for _, edit := range []l3Spec{l3SpecOf(Tunnel{Name: s.Name}, cfg.L3), directSpecFrom(s.Name, cfg.L3)} {
			if got := decode(t, edit.Render()).L3.SingL3Config; !reflect.DeepEqual(got, s.Sing) {
				t.Fatal("ordinary L3 edit lost helper settings")
			}
		}
		raw, err := shareLinkOf(s.Name, "203.0.113.1", cfg)
		if err != nil {
			t.Fatal(err)
		}
		link, err := DecodeShareLink(raw)
		if err != nil {
			t.Fatal(err)
		}
		peer := MirrorForPeer(link)
		if peer.Sing.Target != s.Sing.Target || peer.Sing.ServerName != s.Sing.ServerName || peer.Sing.CertFile != "" || peer.Sing.KeyFile != "" || peer.Sing.CAFile != "" {
			t.Fatal("native setup link leaked local TLS paths or lost paired settings")
		}
	}
}

func TestManagedAWGPairExportsOnlyTheReceivingIdentityAndPreservesItOnEdit(t *testing.T) {
	a, b, err := amneziawg.NewPair("203.0.113.1:25999", "10.254.248.1/32", "10.254.248.2/32")
	if err != nil {
		t.Fatal(err)
	}
	c := config.AWGConfig(a)
	c.PeerSetupKey = b.PrivateKey
	body, err := awgBody(c)
	if err != nil {
		t.Fatal(err)
	}
	cfg := decode(t, body)
	if !reflect.DeepEqual(cfg.AWG, c) {
		t.Fatal("AWG TOML roundtrip lost private pairing")
	}
	raw, err := awgLink("awg-test", "203.0.113.1", c)
	if err != nil {
		t.Fatal(err)
	}
	l, err := DecodeShareLink(raw)
	if err != nil {
		t.Fatal(err)
	}
	peer, err := awgFromLink(l)
	if err != nil {
		t.Fatal(err)
	}
	if peer.PrivateKey != b.PrivateKey || peer.PeerPublicKey != b.PeerPublicKey || peer.PrivateKey == c.PrivateKey || strings.Contains(l.AWGPeer, c.PrivateKey) || peer.PeerSetupKey != "" || peer.PresharedKey != c.PresharedKey {
		t.Fatal("setup link exported the wrong identity")
	}
	if InstallCommand(raw) != "" {
		t.Fatal("unreleased AWG link offered an older binary")
	}
	ports := "8080=10.254.248.2:8081"
	udp := true
	edit, err := applyAWGEdit(c, DirectEdit{Ports: &ports, AcceptUDP: &udp})
	if err != nil {
		t.Fatal(err)
	}
	edit.Ports, edit.AcceptUDP = nil, false
	if !reflect.DeepEqual(edit, c) {
		t.Fatal("port edit changed paired identities")
	}
	bad := l
	bad.Host = "203.0.113.2"
	if _, err := awgFromLink(bad); err == nil {
		t.Fatal("inconsistent peer endpoint accepted")
	}
	mtu := 1280
	if _, err := applyAWGEdit(c, DirectEdit{MTU: &mtu}); err == nil {
		t.Fatal("accepted an unsupported paired edit")
	}
}

func TestManagedAWGInstallationWritesAPrivateConfigAndReportsTheServiceState(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("managed Linux service fixture")
	}
	if err := os.MkdirAll(app.ConfigDir, 0700); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte(t.TempDir()))
	name := fmt.Sprintf("protocol-awg-%x", sum[:6])
	service := app.ServiceName(name)
	t.Cleanup(func() { os.Remove(app.ConfigPath(name)) })
	t.Cleanup(func() { os.Remove(app.ConfigPath(name + "-duplicate")) })
	a, b, err := amneziawg.NewPair("203.0.113.1:25999", "10.254.248.1/32", "10.254.248.2/32")
	if err != nil {
		t.Fatal(err)
	}
	c := config.AWGConfig(a)
	c.PeerSetupKey = b.PrivateKey
	c.Interface = fmt.Sprintf("bpawg%x", sum[:4])
	oldWrite, oldStart, oldReload, oldWait := writeUnit, StartService, DaemonReload, WaitServiceActive
	defer func() {
		writeUnit, StartService, DaemonReload, WaitServiceActive = oldWrite, oldStart, oldReload, oldWait
	}()
	written, started, waited := false, false, false
	writeUnit = func(n string) error {
		if n != name {
			t.Fatal("wrong unit name")
		}
		written = true
		return nil
	}
	StartService = func(s string) error {
		if s != service {
			t.Fatal("wrong service")
		}
		started = true
		return nil
	}
	DaemonReload = func() error { return nil }
	WaitServiceActive = func(s string, _ time.Duration) bool {
		if s != service {
			t.Fatal("wrong readiness target")
		}
		waited = true
		return true
	}
	result, err := installAWG(name, c)
	if err != nil {
		t.Fatal(err)
	}
	if !written || !started || !waited || !result.Active || result.Kind != "awg" || result.Service != service {
		t.Fatal("managed installation did not report its actual active service")
	}
	st, err := os.Stat(app.ConfigPath(name))
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0600 {
		t.Fatal("private identities were written with public permissions")
	}
	started = false
	if _, err := installAWG(name+"-duplicate", c); err == nil || started {
		t.Fatal("duplicate managed identity started a second service")
	}
}
