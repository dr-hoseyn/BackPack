package manage

import (
	"fmt"
	"strings"
	"testing"

	"github.com/BurntSushi/toml"
	"github.com/backpack/backpack/config"
	"github.com/backpack/backpack/internal/tui"
)

func TestXDIReverseChoiceRendersBothRolesAndSharesThePeerSettings(t *testing.T) {
	var choice string
	for gi, g := range transportGroups {
		for ei, e := range g.entries {
			if e.value != "xdi" {
				continue
			}
			restore := tui.SetInput(strings.NewReader(fmt.Sprintf("%d\n%d\n", gi+1, ei+1)))
			capture(t, func() { choice = chooseTransport() })
			restore()
		}
	}
	if choice != "xdi" {
		t.Fatal("the Reverse wizard cannot select xDi")
	}
	for _, preset := range []string{PresetBalance, PresetTurbo, PresetAggressive, PresetThroughput} {
		for _, role := range []string{"server", "client"} {
			s := TunnelSpec{Name: "reverse-xdi", Role: role, Transport: choice,
				BindAddr: "0.0.0.0:49001", RemoteAddr: "192.0.2.1:49001",
				Token: "reverse-xdi-shared-token", Ports: []string{"49002=127.0.0.1:8080"}}
			ApplyPreset(&s, preset)
			var cfg config.Config
			if _, err := toml.Decode(s.Render(), &cfg); err != nil {
				t.Fatalf("%s/%s: %v", role, preset, err)
			}
			tr, kcp := cfg.Server.Transport, cfg.Server.KCPConfig
			if role == "client" {
				tr, kcp = cfg.Client.Transport, cfg.Client.KCPConfig
			}
			if tr != config.XDI || kcp.MTU <= 0 || kcp.SndWnd <= 0 || kcp.RcvWnd <= 0 {
				t.Fatalf("%s/%s: invalid xDi config: %s %+v", role, preset, tr, kcp)
			}
			link, err := DecodeShareLink(pendingReverseLink(s, "192.0.2.1", linkExtras{}))
			if err != nil || link.Kind != "reverse" || link.Tr != "xdi" || MirrorForPeer(link).Transport != "xdi" {
				t.Fatalf("%s/%s: peer setup lost Reverse xDi: %+v, %v", role, preset, link, err)
			}
		}
	}
	found := false
	for _, tr := range connTestReverse {
		found = found || tr == "xdi"
	}
	if !found || !connTestNeedsRoot("xdi") || !ctSetupSupported("reverse", "xdi") {
		t.Fatal("Connection Test must measure xDi with privilege checks and support setup from its result")
	}
}
