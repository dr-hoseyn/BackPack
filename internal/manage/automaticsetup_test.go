package manage

import (
	"fmt"
	"net"
	"os"
	"runtime"
	"strings"
	"testing"

	"github.com/backpack/backpack/internal/tui"
)

func TestAutomaticBundlePairsEveryCompatibleCarrier(t *testing.T) {
	binary, cert, key := managedWizardFixture(t)
	carriers := automaticCarriers(true, true, "cover.example:443")
	var endpoints []string
	for i := range carriers {
		endpoints = append(endpoints, fmt.Sprintf("0.0.0.0:%d", 24000+i))
	}
	s, err := buildAutomaticSpec("automatic-test", "localhost", "127.0.0.1:23000", cert, key, "cover.example:443", carriers, endpoints, []string{"25000=127.0.0.1:443"})
	if err != nil {
		t.Fatal(err)
	}
	if len(s.ServerPaths) != 8 || !s.AcceptUDP {
		t.Fatal("incomplete automatic bundle")
	}
	secrets := map[string]bool{}
	for i := range s.ServerPaths {
		p := &s.ServerPaths[i]
		if p.Naive.Enabled() {
			p.Naive.Binary = binary
		}
		if p.Xray.Enabled() {
			p.Xray.Binary = binary
		}
		if p.Sing.Enabled() {
			p.Sing.Binary = binary
			if p.Sing.Password == "" || secrets[p.Sing.Password] {
				t.Fatal("helper credentials reused")
			}
			secrets[p.Sing.Password] = true
			if (p.Sing.Protocol == "tuic" || p.Sing.Protocol == "hysteria2") != p.Sing.NativeUDP {
				t.Fatal("incorrect native UDP capability")
			}
		}
	}
	if runtime.GOOS == "linux" {
		if err := validateManagedSpec(s); err != nil {
			t.Fatal(err)
		}
	}
	raw := pendingReverseLink(s, "localhost", linkExtras{})
	if raw == "" {
		t.Fatal("no pairing link")
	}
	link, err := DecodeShareLink(raw)
	if err != nil {
		t.Fatal(err)
	}
	if link.Tr != "managed-paths" || len(link.PathLinks) != 8 {
		t.Fatal("automatic link lost carriers")
	}
	client := reverseClientFromLink(link, "localhost")
	if len(client.ClientPaths) != 8 || client.RemoteAddr != s.BindAddr || client.Token != s.Token {
		t.Fatal("peer cannot infer complete automatic configuration")
	}
	private, _ := os.ReadFile(key)
	decoded, err := decodeManagedPaths(link)
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range decoded {
		if strings.Contains(path.HelperCA, "PRIVATE") || strings.Contains(path.HelperCA, string(private)) {
			t.Fatal("private TLS key exported")
		}
	}
}

func TestAutomaticAvailabilityAndPortReservations(t *testing.T) {
	if got := automaticCarriers(false, true, ""); strings.Join(got, ",") != "short-https,xhttp" {
		t.Fatal(got)
	}
	if got := automaticCarriers(true, false, ""); len(got) != 5 {
		t.Fatal(got)
	}
	for _, tc := range []struct {
		spec string
		port int
		want bool
	}{
		{"443=127.0.0.1:2096", 2096, false}, {"443=127.0.0.1:2096", 443, true},
		{"20000-20010=30000-30010", 20005, true}, {"[::1]:8443=443", 8443, true},
	} {
		if got := automaticForwardPort([]string{tc.spec}, tc.port); got != tc.want {
			t.Fatalf("%s port %d: %v", tc.spec, tc.port, got)
		}
	}
	used := map[int]bool{}
	addr, close, err := reserveAutomaticPort("127.0.0.1", []string{"443"}, used)
	if err != nil {
		t.Fatal(err)
	}
	defer close()
	if l, err := net.Listen("tcp4", addr); err == nil {
		l.Close()
		t.Fatal("TCP reservation not held")
	}
	if l, err := net.ListenPacket("udp4", addr); err == nil {
		l.Close()
		t.Fatal("UDP reservation not held")
	}
	second, release, err := reserveAutomaticPort("127.0.0.1", nil, used)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	if second == addr {
		t.Fatal("independent carriers share an endpoint")
	}
	if got := automaticUDPConflict([]string{addr + "=443"}); got == "" {
		t.Fatal("occupied UDP forward was not detected")
	}
}

func TestKharejLinkNeverAsksForAProtocol(t *testing.T) {
	// A malformed link stops before any persistence. Even this path must not
	// ask the operator to choose a protocol or a direction first.
	restore := tui.SetInput(strings.NewReader("1\ninvalid-link\n\n"))
	defer restore()
	out := capture(t, SetupKharej)
	if !strings.Contains(out, "Setup Link") {
		t.Fatal("link entry is missing")
	}
	for _, unwanted := range []string{"Select Transport", "Direction", "Same Carrier"} {
		if strings.Contains(out, unwanted) {
			t.Fatalf("link setup asks redundant question: %s", unwanted)
		}
	}
}
