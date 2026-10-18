package naive

import (
	"bytes"
	"context"
	"encoding/json"
	"github.com/backpack/backpack/config"
	"github.com/sirupsen/logrus"
	"golang.org/x/net/proxy"
	"io"
	"net"
	"os"
	"testing"
	"time"
)

func TestSingAdvancedRefusesIncompatibleOrPartialOptions(t *testing.T) {
	base := config.HysteriaRealm{}
	for _, tc := range []struct {
		protocol, obfs, password string
		min, max                 int
		realm                    config.HysteriaRealm
	}{
		{"anytls", "gecko", "a-valid-obfuscation-secret", 0, 0, base},
		{"hysteria2", "gecko", "", 0, 0, base},
		{"hysteria2", "gecko", "a-valid-obfuscation-secret", 1200, 512, base},
		{"hysteria2", "salamander", "a-valid-obfuscation-secret", 512, 1200, base},
		{"hysteria2", "", "", 0, 0, config.HysteriaRealm{ServerURL: "http://realm.example", Token: "a-realm-account-secret", RealmID: "pair", STUNServers: "stun.example:3478"}},
	} {
		if validateSingAdvanced(tc.protocol, tc.obfs, tc.password, tc.min, tc.max, "", "", "", tc.realm, false) == nil {
			t.Fatalf("accepted invalid advanced configuration: %s/%s", tc.protocol, tc.obfs)
		}
	}
}

func TestSingAdvancedOfficialGeckoAndECHTransfers(t *testing.T) {
	bin := os.Getenv("BP_SINGBOX_BINARY")
	if bin == "" {
		t.Skip("set BP_SINGBOX_BINARY to pinned sing-box")
	}
	for _, mode := range []string{"gecko", "gecko-ech", "anytls-ech", "tuic-ech"} {
		t.Run(mode, func(t *testing.T) {
			protocol := "hysteria2"
			if mode == "anytls-ech" {
				protocol = "anytls"
			}
			if mode == "tuic-ech" {
				protocol = "tuic"
			}
			s, c := singFixture(t, protocol)
			s.Sing.Binary, c.Sing.Binary = bin, bin
			s.Sing.Listen = singFreeAddr(t, config.SingDatagram(protocol))
			c.Sing.Server = s.Sing.Listen
			if protocol == "hysteria2" {
				s.Sing.ObfsType, c.Sing.ObfsType = "gecko", "gecko"
				s.Sing.GeckoMin, c.Sing.GeckoMin = 512, 512
				s.Sing.GeckoMax, c.Sing.GeckoMax = 1200, 1200
			}
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			if mode != "gecko" {
				public, private, err := GenerateECH(ctx, bin, "public.example")
				if err != nil {
					t.Fatal(err)
				}
				s.Sing.ECHConfig, s.Sing.ECHKey, c.Sing.ECHConfig = public, private, public
			}
			backend, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			defer backend.Close()
			s.BindAddr, c.RemoteAddr = backend.Addr().String(), backend.Addr().String()
			go func() {
				for {
					conn, err := backend.Accept()
					if err != nil {
						return
					}
					go func() { defer conn.Close(); io.Copy(conn, conn) }()
				}
			}()
			log := logrus.New()
			log.SetOutput(io.Discard)
			sh, err := StartSingServer(ctx, &s, log)
			if err != nil {
				t.Fatal(err)
			}
			defer sh.Close()
			ch, err := StartSingClient(ctx, &c, log)
			if err != nil {
				t.Fatal(err)
			}
			defer ch.Close()
			d, err := proxy.SOCKS5("tcp", ch.addr, nil, proxy.Direct)
			if err != nil {
				t.Fatal(err)
			}
			conn, err := d.(proxy.ContextDialer).DialContext(ctx, "tcp", c.RemoteAddr)
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			conn.SetDeadline(time.Now().Add(10 * time.Second))
			want := bytes.Repeat([]byte("verified-gecko-ech"), 1024)
			if _, err := conn.Write(want); err != nil {
				t.Fatal(err)
			}
			got := make([]byte, len(want))
			if _, err := io.ReadFull(conn, got); err != nil || !bytes.Equal(got, want) {
				t.Fatalf("payload: %v", err)
			}
			body, err := SingClientJSON(c.Sing, "127.0.0.1:1080", c.RemoteAddr, "tcp")
			if err != nil {
				t.Fatal(err)
			}
			if s.Sing.ECHKey != "" && bytes.Contains(body, []byte(s.Sing.ECHKey)) {
				t.Fatal("server ECH key escaped into client")
			}
		})
	}
}

func TestSingRealmGeneratedContractKeepsTargetRestricted(t *testing.T) {
	s, c := singFixture(t, "hysteria2")
	r := config.HysteriaRealm{ServerURL: "https://realm.example:443", RealmID: "paired-test", Token: "separate-realm-account-token", STUNServers: "stun.example:3478", IPVersion: 4}
	s.Sing.Realm, c.Sing.Realm = r, r
	body, err := SingClientJSON(c.Sing, "127.0.0.1:1080", c.RemoteAddr, "both")
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(body, &doc); err != nil {
		t.Fatal(err)
	}
	out := doc["outbounds"].([]any)[0].(map[string]any)
	if out["server"] != nil || out["realm"] == nil {
		t.Fatal("Realm must discover its endpoint")
	}
	if !bytes.Contains(body, []byte("reject")) {
		t.Fatal("removed restricted destination policy")
	}
}
