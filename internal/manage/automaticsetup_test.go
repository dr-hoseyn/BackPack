package manage

import (
	"fmt"
	"net"
	"os"
	"runtime"
	"strings"
	"testing"

	"github.com/backpack/backpack/config"
	"github.com/backpack/backpack/internal/tui"
	"github.com/backpack/backpack/internal/tunnel/naive"
)

func TestAutomaticBundlePairsEveryCompatibleCarrier(t *testing.T) {
	binary, cert, key := managedWizardFixture(t)
	carriers := automaticCarriers(true, true, true, "cover.example:443")
	var endpoints []string
	for i := range carriers {
		endpoints = append(endpoints, fmt.Sprintf("0.0.0.0:%d", 24000+i))
	}
	s, err := buildAutomaticSpec("automatic-test", "localhost", "127.0.0.1:23000", automaticTLSMaterial{Cert: cert, Key: key}, "cover.example:443", carriers, endpoints, []string{"25000=127.0.0.1:443"})
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

func TestAutomaticIPPairsXHTTPPrivateDNSWithoutRequiringADomain(t *testing.T) {
	const identity = "automatic-ip.backpack.invalid"
	binary, cert, key := managedWizardFixture(t, identity)
	var requests []string
	original := EnsureSelfSignedCert
	EnsureSelfSignedCert = func(name, host string) (string, string, error) {
		requests = append(requests, name+"="+host)
		return cert, key, nil
	}
	defer func() { EnsureSelfSignedCert = original }()
	material, err := automaticCertificates("automatic-ip", "127.0.0.1", true)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(requests, ",") != "automatic-ip-https=127.0.0.1,automatic-ip-xhttp="+identity {
		t.Fatal("IP and private DNS certificates were not generated independently", requests)
	}
	carriers := []string{"short-https", "xhttp"}
	endpoints := []string{"0.0.0.0:24000", "0.0.0.0:24001"}
	s, err := buildAutomaticSpec("automatic-ip", "127.0.0.1", "127.0.0.1:23000", material, "", carriers, endpoints, []string{"25000=127.0.0.1:443"})
	if err != nil {
		t.Fatal(err)
	}
	s.ServerPaths[1].Xray.Binary = binary
	if runtime.GOOS == "linux" {
		if err := validateManagedSpec(s); err != nil {
			t.Fatal(err)
		}
	}
	link, err := DecodeShareLink(pendingReverseLink(s, "127.0.0.1", linkExtras{}))
	if err != nil {
		t.Fatal(err)
	}
	client := reverseClientFromLink(link, "127.0.0.1")
	x := client.ClientPaths[1].Xray
	if x.Server != "127.0.0.1:24001" || x.ServerName != identity {
		t.Fatalf("peer lost IP endpoint or private verified name: %+v", x)
	}
	x.Binary, x.CAFile = binary, cert
	c := config.ClientConfig{Transport: config.TCP, RemoteAddr: s.BindAddr, Token: s.Token, Xray: x}
	if runtime.GOOS == "linux" {
		if err := naive.ValidateXrayClient(&c); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := buildAutomaticSpec("automatic-ip", "127.0.0.1", s.BindAddr, automaticTLSMaterial{Cert: cert, Key: key}, "", carriers, endpoints, s.Ports); err == nil {
		t.Fatal("IP XHTTP accepted without its verified DNS identity")
	}
}

func TestAutomaticAvailabilityAndPortReservations(t *testing.T) {
	if got := automaticCarriers(false, true, true, ""); strings.Join(got, ",") != "short-https,xhttp" {
		t.Fatal(got)
	}
	if got := automaticCarriers(true, false, true, ""); len(got) != 5 {
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
