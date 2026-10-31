package manage

import (
	"errors"
	"net/netip"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/backpack/backpack/config"
	"github.com/backpack/backpack/internal/tunnel/kernel"
)

func TestKernelSubnetSuggestionsAvoidBroaderAndExistingPrefixes(t *testing.T) {
	used := []netip.Prefix{netip.MustParsePrefix("10.77.0.0/24"), netip.MustParsePrefix("fd77:77::/48")}
	local, peer := nextKernelPair(false, used)
	if local != "10.77.1.1/30" || peer != "10.77.1.2" {
		t.Fatalf("suggested an occupied subnet: %s / %s", local, peer)
	}
	local, peer = nextKernelPair(true, used)
	if local != "fd77:77:1::1/64" || peer != "fd77:77:1::2" {
		t.Fatalf("IPv6-only methods got invalid defaults: %s / %s", local, peer)
	}
	a := config.KernelConfig{LocalIP: "10.77.1.1/30", PeerIP: "10.77.1.2"}
	b := config.KernelConfig{LocalIP: "10.77.1.2/30", PeerIP: "10.77.1.1"}
	if !kernelAddressConflict(a, b) {
		t.Fatal("different host strings hid the same /30")
	}
	if kernelPortNetwork(config.KernelConfig{Method: "wireguard", Port: 53, Reverse: true}) != "udp" {
		t.Fatal("reverse WireGuard was mistaken for a TCP carrier")
	}
}

func TestKernelInstallationCannotReplaceAnExistingOrConcurrentConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "one.toml")
	var wg sync.WaitGroup
	results := make(chan error, 2)
	for _, body := range []string{"first private config", "second private config"} {
		wg.Add(1)
		go func(body string) { defer wg.Done(); results <- writeNewKernelConfig(path, []byte(body)) }(body)
	}
	wg.Wait()
	close(results)
	created, refused := 0, 0
	for err := range results {
		if err == nil {
			created++
		} else if errors.Is(err, os.ErrExist) {
			refused++
		} else {
			t.Fatal(err)
		}
	}
	if created != 1 || refused != 1 {
		t.Fatalf("concurrent imports: %d created, %d refused", created, refused)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err = writeNewKernelConfig(path, []byte("replacement")); !errors.Is(err, os.ErrExist) {
		t.Fatal("existing config was replaced")
	}
	after, _ := os.ReadFile(path)
	if string(after) != string(before) {
		t.Fatal("refused import changed the current config")
	}
	files, _ := os.ReadDir(filepath.Dir(path))
	if len(files) != 1 {
		t.Fatal("temporary private configs survived installation")
	}
}

func TestKernelSetupLinksMirrorEveryPairedSetting(t *testing.T) {
	for _, method := range kernel.Methods() {
		if method.IPv6 || method.IPv6Only {
			continue
		}
		c, err := kernel.Resolve(config.KernelConfig{Method: method.Name, Role: "iran", LocalAddr: "192.0.2.1", RemoteAddr: "192.0.2.2", LocalIP: "10.77.1.1/30", PeerIP: "10.77.1.2", Token: strings.Repeat("a", 48), Reverse: true, Ports: []string{"8080=8081"}, AcceptUDP: true})
		if err != nil {
			t.Fatal(err)
		}
		body, err := kernelBody(c)
		if err != nil {
			t.Fatal(err)
		}
		cfg := decode(t, body)
		if !reflect.DeepEqual(cfg.Kernel, c) {
			t.Fatalf("%s configuration lost settings", method.Name)
		}
		raw, err := kernelLink("kernel-test", c)
		if err != nil {
			t.Fatal(err)
		}
		l, err := DecodeShareLink(raw)
		if err != nil {
			t.Fatal(err)
		}
		peer, err := kernelFromLink(l)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(peer, kernel.Peer(c)) {
			t.Fatalf("%s link lost paired settings", method.Name)
		}
		if InstallCommand(raw) != "" {
			t.Fatal("offered an older release for an unreleased link format")
		}
		l.Host = "192.0.2.3"
		if _, err = kernelFromLink(l); err == nil {
			t.Fatal("inconsistent link metadata was accepted")
		}
	}
}
