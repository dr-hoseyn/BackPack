package manage

import (
	"reflect"
	"strings"
	"testing"

	"github.com/backpack/backpack/config"
	"github.com/backpack/backpack/internal/tunnel/l3"
)

func TestMasqueSettingsSurviveRenderEditAndSetupLink(t *testing.T) {
	m := config.MasqueConfig{Target: "127.0.0.1:62110", ServerName: "kharej.example.com",
		CertFile: "/private/server.crt", KeyFile: "/private/server.key", CAFile: "/private/ca.crt"}
	spec := l3Spec{Name: "masque-pair", Side: sideKharej, Carrier: "masque", Masque: m,
		Addr: "0.0.0.0:443", Token: "long-masque-token", LocalIP: "10.20.0.2/30", PeerIP: "10.20.0.1", MTU: 1200}
	cfg := decode(t, spec.Render())
	if !reflect.DeepEqual(cfg.L3.MasqueConfig, m) {
		t.Fatalf("lost settings: %+v", cfg.L3.MasqueConfig)
	}
	for _, edit := range []l3Spec{directSpecFrom(spec.Name, cfg.L3), l3SpecOf(Tunnel{Name: spec.Name}, cfg.L3)} {
		edit.MTU = 1250
		if got := decode(t, edit.Render()).L3.MasqueConfig; !reflect.DeepEqual(got, m) {
			t.Fatalf("edit lost settings: %+v", got)
		}
	}
	engine, err := l3.ConfigFromTable(cfg.L3)
	if err != nil || !reflect.DeepEqual(engine.Masque, m) {
		t.Fatalf("engine config: %+v, %v", engine.Masque, err)
	}
	raw, err := shareLinkOf(spec.Name, "kharej.example.com", cfg)
	if err != nil {
		t.Fatal(err)
	}
	link, err := DecodeShareLink(raw)
	if err != nil {
		t.Fatal(err)
	}
	peer := MirrorForPeer(link)
	if peer.Masque.Target != m.Target || peer.Masque.ServerName != m.ServerName {
		t.Fatalf("peer: %+v", peer.Masque)
	}
	if peer.Masque.KeyFile != "" || peer.Masque.CertFile != "" || peer.Masque.CAFile != "" {
		t.Fatalf("link leaked local paths: %+v", peer.Masque)
	}
	n := peer.ToNewDirectTunnel()
	if n.Masque.Target != m.Target {
		t.Fatal("managed peer form lost target")
	}
}

func TestMasquePanelRequiresListenerTLSAndRejectsPublicTargets(t *testing.T) {
	base := NewDirectTunnel{Side: "kharej", Carrier: "masque", Name: "masque", Token: "token", TunnelPort: "443"}
	if _, err := base.spec(); err == nil || !strings.Contains(err.Error(), "masque_cert") {
		t.Fatalf("missing certificate: %v", err)
	}
	base.Masque = config.MasqueConfig{CertFile: "/cert.pem", KeyFile: "/key.pem"}
	if _, err := base.spec(); err != nil {
		t.Fatal(err)
	}
	base.Masque.Target = "8.8.8.8:53"
	if _, err := base.spec(); err == nil {
		t.Fatal("public target accepted")
	}
}

func TestMasqueLinkApplyExplainsMissingCertificateFiles(t *testing.T) {
	_, err := applyDirectLink(ShareLink{Kind: "direct", From: "iran", Tr: "masque", Tok: "token"}, LinkApplyOptions{})
	if err == nil || !strings.Contains(err.Error(), "--masque-cert") {
		t.Fatalf("unhelpful error: %v", err)
	}
}
