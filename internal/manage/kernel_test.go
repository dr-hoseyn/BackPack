package manage

import (
	"reflect"
	"strings"
	"testing"

	"github.com/backpack/backpack/config"
	"github.com/backpack/backpack/internal/tunnel/kernel"
)

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
