package manage

import (
	"reflect"
	"testing"

	"github.com/backpack/backpack/config"
	"github.com/backpack/backpack/internal/tunnel/l3"
)

func TestGoldenCarrierFormsPreserveDNSSettingsAndNeverMirrorLocalBind(t *testing.T) {
	for _, carrier := range []string{"dns", "icmp-request", "icmp-reply"} {
		s := l3Spec{Name: "golden-test", Side: sideIran, Carrier: carrier, Addr: "192.0.2.2:30530", Token: "golden-private-test-token", LocalIP: "10.78.2.1/30", PeerIP: "10.78.2.2", MTU: 1280}
		if carrier == "dns" {
			s.DNS = config.DNSConfig{DNSBind: "192.0.2.1", DNSRotate: 6, DNSRate: 5000}
			s.Paths = 2
		}
		_, body, err := directBodyFromSpec(s)
		if err != nil {
			t.Fatal(err)
		}
		c := decode(t, body)
		engine, err := l3.ConfigFromTable(c.L3)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(engine.DNS, s.DNS) {
			t.Fatal("DNS options never reached the engine")
		}
		raw, err := shareLinkOf(s.Name, "192.0.2.1", c)
		if err != nil {
			t.Fatal(err)
		}
		link, err := DecodeShareLink(raw)
		if err != nil {
			t.Fatal(err)
		}
		form := MirrorForPeer(link)
		if form.DNS.DNSBind != "" || form.DNS.DNSRotate != s.DNS.DNSRotate || form.DNS.DNSRate != s.DNS.DNSRate {
			t.Fatal("setup link lost sending settings or copied the other server's local bind")
		}
		peer := form.ToNewDirectTunnel()
		peer.Name = "golden-peer"
		_, peerBody, err := directBody(peer)
		if err != nil {
			t.Fatal(err)
		}
		pc := decode(t, peerBody)
		if pc.L3.Carrier != carrier || pc.L3.DNSRotate != s.DNS.DNSRotate || pc.L3.DNSRate != s.DNS.DNSRate || pc.L3.Paths != s.Paths {
			t.Fatalf("peer configuration lost the carrier: %+v", pc.L3)
		}
		_, edited, err := directBodyFromSpec(directSpecFrom(s.Name, c.L3))
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(decode(t, edited).L3.DNSConfig, c.L3.DNSConfig) {
			t.Fatal("an ordinary edit lost DNS settings")
		}
	}
}

func TestGoldenCarriersEnterTheActualConnectionTestAndPortCoordination(t *testing.T) {
	for _, name := range []string{"dns", "icmp-request", "icmp-reply"} {
		seen := false
		for _, plan := range ctDirectPlans("") {
			if plan.carrier == name {
				seen = true
			}
		}
		if !seen || !l3.KnownCarrier(name) || !offeredCarrier(name) {
			t.Fatalf("%s is not offered and tested by the engine", name)
		}
	}
	if !ctPortCarrier("dns") || !udpCarriers["dns"] {
		t.Fatal("DNS does not reserve and coordinate its receive ports")
	}
}
