package manage

import (
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/backpack/backpack/config"
	"github.com/backpack/backpack/internal/tui"
)

func TestForwardPortEditorsCancelBeforeConfigWrite(t *testing.T) {
	// No config exists for this name: an accidental save would report an error.
	for _, layer := range []string{"direct", "l3"} {
		t.Run(layer, func(t *testing.T) {
			restore := tui.SetInput(strings.NewReader("1\n\n"))
			defer restore()
			out := captureForwardPortScreen(t, func() {
				cfg := config.Config{}
				if layer == "l3" {
					cfg.L3 = config.L3Config{Mode: "dial", PeerIP: "10.10.0.2/30", Ports: []string{"443"}}
					editL3Ports(Tunnel{Name: "missing-forward-editor-test"}, cfg)
				} else {
					cfg.Direct = config.DirectConfig{Role: "iran", Ports: []string{"443"}}
					editDirectPorts(Tunnel{Name: "missing-forward-editor-test"}, cfg)
				}
			})
			if !strings.Contains(out, "SOURCE (THIS SERVER)") || strings.Contains(out, "Failed:") || strings.Contains(out, "Saved") {
				t.Fatalf("cancelled editor unexpectedly saved, or did not open:\n%s", out)
			}
		})
	}
}

func TestForwardPortReverseEditPreservesHiddenRelay(t *testing.T) {
	previousLoad, previousApply := LoadSpec, applySpec
	defer func() { LoadSpec, applySpec = previousLoad, previousApply }()
	relay := "19000=127.0.0.1:1080"
	current := []string{"443", "8080=80", relay}
	for _, tc := range []struct {
		name        string
		ports, want []string
		applies     bool
	}{
		{"nil leaves ports unchanged", nil, nil, false},
		{"delete selected", []string{"8080=80"}, []string{"8080=80", relay}, true},
		{"clear visible ports", []string{}, []string{relay}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			LoadSpec = func(string) (TunnelSpec, error) {
				return TunnelSpec{Role: "server", Ports: slices.Clone(current)}, nil
			}
			called := false
			applySpec = func(s TunnelSpec) error {
				called = true
				if !slices.Equal(s.Ports, tc.want) {
					t.Errorf("saved %v, want %v", s.Ports, tc.want)
				}
				return nil
			}
			err := EditTunnel("forward-test", "", "", tc.ports)
			if called != tc.applies || (err == nil) != tc.applies {
				t.Fatalf("applied=%v err=%v, want applied=%v", called, err, tc.applies)
			}
		})
	}
}

func TestForwardPortClearKeepsL3Settings(t *testing.T) {
	before := config.L3Config{Mode: "dial", Carrier: "pck", PeerIP: "10.10.0.2/30",
		Ports: []string{"443"}, AcceptUDP: true, MaxConnections: 12, BandwidthMbps: 50,
		MTU: 1400, FECData: 10, FECParity: 3, Paths: 4, Qdisc: "fq_codel"}
	raw := ""
	got, err := applyDirectEdit(before, DirectEdit{Ports: &raw})
	want := before
	want.Ports = nil
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("clearing only ports changed other settings: %+v, %v", got, err)
	}
}
