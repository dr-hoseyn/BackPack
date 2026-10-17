package l3

import (
	"bytes"
	"context"
	"net"
	"os"
	"testing"
	"time"

	"github.com/backpack/backpack/config"
)

func TestNativeSingCarriersCarryIPOverAuthenticatedNoise(t *testing.T) {
	bin := os.Getenv("BP_SINGBOX_BINARY")
	if bin == "" {
		t.Skip("set BP_SINGBOX_BINARY to the pinned executable")
	}
	for _, protocol := range []string{"hysteria2", "tuic"} {
		t.Run(protocol, func(t *testing.T) {
			tls := masqueTestConfig(t)
			port, err := net.ListenPacket("udp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			endpoint := port.LocalAddr().String()
			port.Close()
			base := Config{Mode: ModeListen, Addr: endpoint, Carrier: protocol, Token: "a-distinct-native-noise-secret", LocalIP: "10.10.0.2/30", PeerIP: "10.10.0.1", MTU: 1300, Sing: config.SingL3Config{Binary: bin, Target: tls.Target, ServerName: tls.ServerName, CertFile: tls.CertFile, KeyFile: tls.KeyFile}}
			dialDev, listenDev := newFakeDevice(1400), newFakeDevice(1400)
			ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
			defer cancel()
			listener, err := New(base, quietLogger())
			if err != nil {
				t.Fatal(err)
			}
			listener.openDevice = func(deviceSpec) (packetDevice, error) { return listenDev, nil }
			start(t, ctx, cancel, listener, listenDev)
			base.Mode = ModeDial
			base.LocalIP, base.PeerIP = "10.10.0.1/30", "10.10.0.2"
			base.Sing.CertFile, base.Sing.KeyFile, base.Sing.CAFile = "", "", tls.CertFile
			dialer, err := New(base, quietLogger())
			if err != nil {
				t.Fatal(err)
			}
			dialer.openDevice = func(deviceSpec) (packetDevice, error) { return dialDev, nil }
			start(t, ctx, cancel, dialer, dialDev)
			awaitSession(t, dialer, 15*time.Second)
			for i := 0; i < 8; i++ {
				across(t, dialDev, listenDev, ipv4Packet(bytes.Repeat([]byte{byte(i)}, 1100)...))
				across(t, listenDev, dialDev, ipv4Packet(4, 3, 2, byte(i)))
			}
		})
	}
}
