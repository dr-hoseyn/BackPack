package manage

import (
	"bytes"
	"io"
	"os"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/backpack/backpack/internal/tui"
)

func TestForwardPortEditorDraftLifecycle(t *testing.T) {
	for _, tc := range []struct {
		name, input   string
		current, want []string
		save          bool
	}{
		{"enter preserves all", "\n", []string{"443", "8080=80"}, nil, false},
		{"back preserves all", "0\n", []string{"443"}, nil, false},
		{"EOF preserves all", "", []string{"443"}, nil, false},
		{"invalid action preserves all", "x\n\n\n", []string{"443"}, nil, false},
		{"add separates source and target", "1\n443\n8443\n5\ny\n", nil, []string{"443=8443"}, true},
		{"edit one keeps other mappings", "2\n2\n\n2096\n5\ny\n", []string{"443", "8080=80"}, []string{"443", "8080=2096"}, true},
		{"edit source", "2\n1\n8443\n\n5\ny\n", []string{"443=2096"}, []string{"8443=2096"}, true},
		{"unchanged fields do not restart", "2\n1\n\n\n5\n", []string{"443"}, nil, false},
		{"cancel source", "1\n\n0\n", []string{"443"}, nil, false},
		{"cancel target", "1\n8443\n\n0\n", []string{"443"}, nil, false},
		{"EOF during source", "1\n", []string{"443"}, nil, false},
		{"EOF during target", "1\n8443\n", []string{"443"}, nil, false},
		{"EOF at apply", "1\n8443\n80\n5\n", []string{"443"}, nil, false},
		{"delete only selected", "3\n1\ny\n5\ny\n", []string{"443", "8080=80"}, []string{"8080=80"}, true},
		{"delete last explicitly clears", "3\n1\ny\n5\ny\n", []string{"443"}, []string{}, true},
		{"delete refused by default", "3\n1\n\n5\n", []string{"443"}, nil, false},
		{"EOF at deletion", "3\n1\n", []string{"443"}, nil, false},
		{"delete all then apply", "4\ny\n5\ny\n", []string{"443", "8080"}, []string{}, true},
		{"delete all refused", "4\n\n5\n", []string{"443"}, nil, false},
		{"back discards pending delete", "3\n1\ny\n\n", []string{"443", "8080"}, nil, false},
		{"back discards pending addition", "1\n8443\n80\n0\n", []string{"443"}, nil, false},
		{"apply refused by default", "1\n8443\n80\n5\n\n0\n", []string{"443"}, nil, false},
		{"invalid mapping keeps list", "1\n443\n65536\n\n5\n", []string{"443"}, nil, false},
		{"overlap keeps list", "1\n443\n8443\n\n5\n", []string{"443"}, nil, false},
		{"invalid selection keeps list", "2\n99\n\n5\n", []string{"443"}, nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before := slices.Clone(tc.current)
			restore := tui.SetInput(strings.NewReader(tc.input))
			defer restore()
			var got []string
			var saved bool
			captureForwardPortScreen(t, func() {
				got, saved = (forwardPortEditor{defaultHost: "10.10.0.2", allowEmpty: true}).edit(tc.current)
			})
			if !reflect.DeepEqual(got, tc.want) || saved != tc.save {
				t.Fatalf("got (%#v, %v), want (%#v, %v)", got, saved, tc.want, tc.save)
			}
			if !slices.Equal(before, tc.current) {
				t.Fatalf("editor mutated the live list: %v -> %v", before, tc.current)
			}
		})
	}
}

func TestForwardPortEditorValidation(t *testing.T) {
	for _, tc := range []struct {
		name   string
		editor forwardPortEditor
		ports  []string
		valid  bool
	}{
		{"single remap", forwardPortEditor{}, []string{"443=8443"}, true},
		{"explicit hosts", forwardPortEditor{}, []string{"127.0.0.1:443=10.0.0.5:8443"}, true},
		{"IPv6", forwardPortEditor{}, []string{"[::1]:443=[2001:db8::1]:8443"}, true},
		{"offset ranges", forwardPortEditor{}, []string{"10000-10009=20000-20009"}, true},
		{"local bound ranges", forwardPortEditor{}, []string{"127.0.0.1:10000-10009=80"}, true},
		{"backend pool", forwardPortEditor{}, []string{"443=127.0.0.1:80|[::1]:80"}, true},
		{"reverse range backend pool", forwardPortEditor{reverse: true}, []string{"10000-10009=80|81"}, true},
		{"different local addresses", forwardPortEditor{}, []string{"127.0.0.1:443=80", "127.0.0.2:443=80"}, true},
		{"empty L3", forwardPortEditor{allowEmpty: true}, []string{}, true},
		{"empty direct edge", forwardPortEditor{}, []string{}, false},
		{"bad target", forwardPortEditor{}, []string{"443=garbage"}, false},
		{"bad target host", forwardPortEditor{}, []string{"443=bad host:80"}, false},
		{"too high target", forwardPortEditor{}, []string{"443=65536"}, false},
		{"empty target", forwardPortEditor{}, []string{"443="}, false},
		{"multiple equals", forwardPortEditor{}, []string{"443=80=81"}, false},
		{"backwards range", forwardPortEditor{}, []string{"444-443=80"}, false},
		{"mismatched ranges", forwardPortEditor{}, []string{"443-445=80-81"}, false},
		{"reverse target range", forwardPortEditor{reverse: true}, []string{"443-445=80-82"}, false},
		{"reverse bad backend", forwardPortEditor{reverse: true}, []string{"443=80|bad"}, false},
		{"duplicate", forwardPortEditor{}, []string{"443", "443=80"}, false},
		{"range overlap", forwardPortEditor{}, []string{"443-445", "444=80"}, false},
		{"wildcard overlap", forwardPortEditor{}, []string{"443", "127.0.0.1:443=80"}, false},
		{"IPv6 alias overlap", forwardPortEditor{}, []string{"[::1]:443=80", "[0:0:0:0:0:0:0:1]:443=80"}, false},
		{"reserved listener", forwardPortEditor{reserved: []string{"0.0.0.0:9000=1"}}, []string{"9000=80"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tc.editor.defaultHost = "127.0.0.1"
			if err := tc.editor.validate(tc.ports); (err == nil) != tc.valid {
				t.Fatalf("validation = %v, want valid=%v", err, tc.valid)
			}
		})
	}
}

func TestForwardPortEditorTargetDisplay(t *testing.T) {
	for _, tc := range []struct{ host, spec, source, target string }{
		{"127.0.0.1", "443", "*:443", "127.0.0.1:443"},
		{"10.10.0.2", "443=8443", "*:443", "10.10.0.2:8443"},
		{"10.10.0.2", "443=:8443", "*:443", "10.10.0.2:8443"},
		{"10.10.0.2", "10000-10009", "*:10000-10009", "10.10.0.2:10000-10009"},
		{"10.10.0.2", "127.0.0.1:443", "127.0.0.1:443", "10.10.0.2:443"},
		{"::2", "443=80|[::1]:81", "*:443", "[::2]:80 | [::1]:81"},
	} {
		e := forwardPortEditor{defaultHost: tc.host}
		source, target := e.endpoints(tc.spec)
		if source != tc.source || target != tc.target {
			t.Errorf("%s: got %s -> %s, want %s -> %s", tc.spec, source, target, tc.source, tc.target)
		}
	}
}

func TestForwardPortGuidedSetup(t *testing.T) {
	e := forwardPortEditor{defaultHost: "10.10.0.2", allowEmpty: true}
	restore := tui.SetInput(strings.NewReader("1\n443\n8443\n5\ny\n"))
	defer restore()
	captureForwardPortScreen(t, func() {
		ports, err := e.setupPorts("A")
		if err != nil || !slices.Equal(ports, []string{"443=8443"}) {
			t.Fatalf("guided setup: %v, %v", ports, err)
		}
	})
}

func captureForwardPortScreen(t *testing.T, f func()) string {
	t.Helper()
	previous := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = w
	done := make(chan string, 1)
	go func() {
		var b bytes.Buffer
		_, _ = io.Copy(&b, r)
		done <- b.String()
	}()
	defer func() { os.Stdout = previous; r.Close() }()
	f()
	w.Close()
	return <-done
}
