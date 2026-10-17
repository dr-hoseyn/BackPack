package l3

import (
	"bytes"
	"context"
	"testing"
	"time"
)

func TestMasqueHTTP2TunnelCarriesVerifiedIPOverNoise(t *testing.T) {
	masqueCfg := masqueTestConfig(t)
	dialDev, listenDev := newFakeDevice(1400), newFakeDevice(1400)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	base := Config{Mode: ModeListen, Addr: "127.0.0.1:0", Token: "masque-h2-noise-token", Carrier: CarrierMasqueH2,
		Masque: masqueCfg, LocalIP: "10.10.0.2/30", PeerIP: "10.10.0.1", MTU: 1300}
	listener, err := New(base, quietLogger())
	if err != nil {
		t.Fatal(err)
	}
	listener.openDevice = func(deviceSpec) (packetDevice, error) { return listenDev, nil }
	start(t, ctx, cancel, listener, listenDev)
	base.Mode, base.Addr = ModeDial, awaitBind(t, listener).String()
	base.LocalIP, base.PeerIP = "10.10.0.1/30", "10.10.0.2"
	dialer, err := New(base, quietLogger())
	if err != nil {
		t.Fatal(err)
	}
	dialer.openDevice = func(deviceSpec) (packetDevice, error) { return dialDev, nil }
	start(t, ctx, cancel, dialer, dialDev)
	awaitSession(t, dialer, 10*time.Second)
	across(t, dialDev, listenDev, ipv4Packet(bytes.Repeat([]byte{0xAB}, 1100)...))
	across(t, listenDev, dialDev, ipv4Packet(4, 3, 2, 1))
}
