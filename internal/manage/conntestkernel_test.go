package manage

import (
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/backpack/backpack/config"
	"github.com/backpack/backpack/internal/testhistory"
	"github.com/backpack/backpack/internal/tunnel/kernel"
)

func TestKernelConnectionTestOffersEveryMethodAndDirection(t *testing.T) {
	s := &ConnTestIran{root: true, link: ConnTestLink{ID: "fixture", Host: "192.0.2.1", Tok: "fixture-token"}}
	s.addKernelCases(ConnTestOptions{Direct: true})
	if len(s.cases) != 2*len(kernel.Methods()) || !s.link.KernelTests {
		t.Fatal("incomplete kernel suite")
	}
	seen := map[string]bool{}
	for _, c := range s.cases {
		m, reverse, ok := ctKernelMethod(c.tr)
		if !ok || seen[c.tr] || !ctSetupSupported(c.kind, c.tr) || !validName(c.name) {
			t.Fatalf("invalid kernel case %s", c.tr)
		}
		seen[c.tr] = true
		if !reverse && m.IPv6 && c.skip == "" {
			t.Fatal("IPv6 native method scheduled on IPv4 underlay")
		}
		if reverse && c.skip != "" {
			t.Fatalf("reverse method omitted: %s", c.tr)
		}
		c.mode = ConnTestQuick
		if c.samples() != 4 {
			t.Fatal("kernel Quick observation is not bounded")
		}
		c.mode = ConnTestFull
		if c.samples() != connTestSoak {
			t.Fatal("kernel Full lost stability samples")
		}
		s.link.Cases = append(s.link.Cases, c.link)
	}
	main, _ := ctConfigParts(s.link)
	if len("config "+main) > 1200 {
		t.Fatal("kernel suite broke single-packet config")
	}
	raw, _ := unGzipB64(main)
	var wire ConnTestLink
	if err := json.Unmarshal(raw, &wire); err != nil || !wire.KernelTests || len(wire.Cases) != 0 {
		t.Fatal("kernel settings must be sent on demand")
	}
}

func TestKernelCoordinatorAuthenticatesAndRejectsStaleAcks(t *testing.T) {
	c := &ctCoordinator{tok: "secret"}
	generation := c.kernelSet("gre", "ct-fixture", &config.KernelConfig{Method: "gre", Role: "kharej"})
	if c.answer("kernel-next wrong", "192.0.2.2") != "" {
		t.Fatal("unauthenticated kernel command")
	}
	reply := c.answer("kernel-next secret", "192.0.2.2")
	if !strings.HasPrefix(reply, "kernel ") || len(reply) > 1200 || !c.kernelCapable {
		t.Fatal("kernel capability/settings missing")
	}
	if got := c.answer("kernel-ack secret "+ctSetupEncode(ctKernelAck{Generation: generation - 1}), ""); got != "" {
		t.Fatal("stale acknowledgement accepted")
	}
	if got := c.answer("kernel-ack secret "+ctSetupEncode(ctKernelAck{Generation: generation}), ""); got != "ok" {
		t.Fatal("valid acknowledgement refused")
	}
	if got := c.answer("kernel-ack secret "+ctSetupEncode(ctKernelAck{Generation: generation, Error: "replayed error"}), ""); got != "ok" || c.kernelAck.Error != "" {
		t.Fatal("replay changed a completed generation")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := c.kernelWait(ctx, generation); err != nil {
		t.Fatal(err)
	}
	c.kernelSet("", "", nil)
	if c.answer("kernel-ack secret "+ctSetupEncode(ctKernelAck{Generation: generation}), "") != "" {
		t.Fatal("old run acknowledgement stopped a newer generation")
	}
}

func TestKernelTestPromotionRetainsMethodAndRotatesPairedIdentity(t *testing.T) {
	for _, method := range kernel.Methods() {
		for _, reverse := range []bool{false, true} {
			tr := method.Name
			if reverse {
				tr += "-reverse"
			}
			a := config.KernelConfig{Method: method.Name, Role: "iran", LocalAddr: "192.0.2.1", RemoteAddr: "192.0.2.2", LocalIP: "10.77.21.1/30", PeerIP: "10.77.21.2", Reverse: reverse, Token: strings.Repeat("a", 48), MTU: 1280}
			if method.IPv6 && !reverse {
				a.LocalAddr, a.RemoteAddr = "2001:db8::1", "2001:db8::2"
			}
			if method.IPv6Only {
				a.LocalIP, a.PeerIP = "fd77:21::1/64", "fd77:21::2"
			}
			a, err := kernel.Resolve(a)
			if err != nil {
				t.Fatal(err)
			}
			b := kernel.Peer(a)
			link := ShareLink{Kind: "kernel", Tr: tr, Name: "ct-native"}
			setup := newCTSetup(ConnTestLink{Cases: []ShareLink{link}}, "192.0.2.2", nil)
			req := ConnTestSetupRequest{Kind: "kernel", Transport: tr, Ports: "34567=8080", AcceptUDP: true}
			if method.IPv6Only {
				req.Ports, req.AcceptUDP = "", false
			}
			fa, fb, err := setup.forms(req)
			if err != nil {
				t.Fatal(err)
			}
			ma := &ctSetupMaterial{root: filepath.Join(t.TempDir(), "assets"), cases: map[string]config.Config{ctSetupCaseID(link): {Kernel: a}}, assets: map[string][]byte{}}
			mb := &ctSetupMaterial{root: filepath.Join(t.TempDir(), "assets"), cases: map[string]config.Config{ctSetupCaseID(link): {Kernel: b}}, assets: map[string][]byte{}}
			fa, err = ma.prepare(fa)
			if err != nil {
				t.Fatalf("%s Iran: %v", tr, err)
			}
			fb, err = mb.prepare(fb)
			if err != nil {
				t.Fatalf("%s Kharej: %v", tr, err)
			}
			pa, pb := fa.SetupConfig.Kernel, fb.SetupConfig.Kernel
			if pa.Token == a.Token || pa.Token != pb.Token || pa.ID != pb.ID || pa.Interface == a.Interface || pa.Method != a.Method || pa.Reverse != reverse || pa.MTU != a.MTU || pa.LocalIP != a.LocalIP || pa.PeerIP != a.PeerIP || pb.LocalIP != b.LocalIP {
				t.Fatalf("%s promotion damaged paired settings", tr)
			}
			if len(pb.Ports) != 0 || !reflect.DeepEqual(pa.Ports, parsePorts(req.Ports)) || pa.AcceptUDP != req.AcceptUDP {
				t.Fatal("forwarded ports escaped to Kharej")
			}
		}
	}
}

func TestKernelResultsAppearInHistoryAndBestSettings(t *testing.T) {
	row := ConnTestResult{Kind: "kernel", Transport: "gre-reverse", Mode: ConnTestQuick, Status: ctOK, Tried: 4, OK: 4, Mbps: 100, UploadMbps: 60, SpeedStable: true}
	table := ConnTestTable([]ConnTestResult{row})
	if !strings.Contains(table, "Kernel") || !strings.Contains(table, "GRE Reverse") {
		t.Fatal("kernel missing from verdict")
	}
	c := &connTestCase{kind: "kernel", tr: row.Transport, kernel: config.KernelConfig{Method: "gre", Reverse: true, MTU: 1280}}
	best := ctComputeBest([]ConnTestResult{row}, []*connTestCase{c}, 1500, t.TempDir())
	if best.KernelMTU != 1280 || !best.KernelEncrypted || best.FECData != 0 || best.KeepAlive != 0 {
		t.Fatal("reverse tuning was applied to a kernel engine")
	}
	if text := ConnTestBestTable(best); !strings.Contains(text, "Kernel MTU") || strings.Contains(text, "Keepalive") {
		t.Fatal("misleading native tuning")
	}
	old := ConnTestHistoryRoot
	ConnTestHistoryRoot = t.TempDir()
	defer func() { ConnTestHistoryRoot = old }()
	if err := SaveConnTestHistory("iran", "turbo", []ConnTestPath{{Host: "192.0.2.1", Kharej: "192.0.2.2", Family: "IPv4", State: "done", Rows: []ConnTestResult{row}}}, true); err != nil {
		t.Fatal(err)
	}
	runs, err := LoadConnTestHistory()
	if err != nil || len(runs) != 1 || len(testhistory.Compare(runs)) != 1 {
		t.Fatalf("kernel history: %v", err)
	}
}

// Invoked only by the disposable namespace CI harness, never by normal tests.
func TestKernelConnectionTestLiveEndpoint(t *testing.T) {
	role := os.Getenv("BP_KERNEL_CT_ROLE")
	if role == "" {
		t.Skip("isolated kernel Connection Test fixture")
	}
	method := os.Getenv("BP_KERNEL_CT_METHOD")
	reverse := os.Getenv("BP_KERNEL_CT_REVERSE") == "1"
	tr := method
	if reverse {
		tr += "-reverse"
	}
	root := os.Getenv("BP_KERNEL_CT_DIR")
	if root == "" {
		t.Fatal("missing fixture directory")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	link := ConnTestLink{V: 1, Kind: connTestKind, Host: "192.0.2.1", Coord: 30191, Tok: "live-kernel-connection-test-secret", TCP: 30192, UDP: 30193, L3: 30194, Mode: ConnTestQuick, Until: time.Now().Add(2 * time.Minute).Unix(), KernelTests: true, AutoSetup: true}
	m, _, ok := ctKernelMethod(tr)
	if !ok {
		t.Fatal("bad fixture method")
	}
	if m.IPv6 && !reverse {
		link.Host = "2001:db8:1::1"
	}
	if role == "kharej" {
		port := net.JoinHostPort(link.Host, strconv.Itoa(link.Coord))
		for ctx.Err() == nil {
			c, e := net.DialTimeout("tcp", port, 100*time.Millisecond)
			if e == nil {
				c.Close()
				break
			}
			ctSleep(ctx, 100*time.Millisecond)
		}
		_, _, err := runConnTestKharejLink(ctx, link, os.Stdout, nil)
		if err != nil {
			t.Fatal(err)
		}
		material := ctGetSetupMaterial(connTestAddr{Host: link.Host, Coord: link.Coord, Tok: link.Tok})
		if material == nil || !material.cases["kernel/"+tr+"/"].Kernel.Enabled() {
			t.Fatal("Kharej could not retain the tested config")
		}
		return
	}
	coord, err := startCTCoordinator(link.Coord, link.Tok)
	if err != nil {
		t.Fatal(err)
	}
	defer coord.close()
	dir := t.TempDir()
	c := &connTestCase{kind: "kernel", tr: tr, name: "ct-live-kernel", mode: ConnTestQuick, link: ShareLink{Kind: "kernel", Tr: tr, Name: "ct-live-kernel"}}
	s := &ConnTestIran{root: true, dir: dir, link: link, coord: coord, cases: []*connTestCase{c}}
	select {
	case <-s.Joined():
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	r := s.runKernelCase(ctx, c, func(r ConnTestResult) {
		t.Logf("%s: %s %d/%d %.1f/%.1f %s", tr, r.Status, r.OK, r.Tried, r.Mbps, r.UploadMbps, r.Detail)
	})
	if r.Status != ctOK || r.Mbps <= 0 || r.UploadMbps <= 0 || r.OK != 4 {
		t.Fatalf("real kernel Connection Test failed: %+v", r)
	}
	if _, err := os.Stat(filepath.Join(dir, c.name+".toml")); err != nil {
		t.Fatal(err)
	}
	coord.publish([]ConnTestResult{r}, ctComputeBest([]ConnTestResult{r}, []*connTestCase{c}, 1500, dir))
	ctSleep(ctx, time.Second)
	if err := os.WriteFile(filepath.Join(root, "passed"), []byte(tr), 0600); err != nil {
		t.Fatal(err)
	}
}
