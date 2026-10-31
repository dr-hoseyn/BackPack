package kernel

import (
	"context"
	"crypto/ecdh"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/backpack/backpack/config"
)

func example(method string) config.KernelConfig {
	c := config.KernelConfig{Method: method, Role: "iran", LocalAddr: "192.0.2.1", RemoteAddr: "192.0.2.2", LocalIP: "10.77.1.1/30", PeerIP: "10.77.1.2", Token: strings.Repeat("s", 48)}
	m, _ := Lookup(method)
	if m.IPv6 {
		c.LocalAddr, c.RemoteAddr = "2001:db8::1", "2001:db8::2"
	}
	if m.IPv6Only {
		c.LocalIP, c.PeerIP = "fd77::1/64", "fd77::2"
	}
	return c
}

func TestEveryStandardMethodBuildsAPairedPlan(t *testing.T) {
	for _, m := range Methods() {
		for _, rev := range []bool{false, true} {
			t.Run(m.Name+map[bool]string{false: "/direct", true: "/reverse"}[rev], func(t *testing.T) {
				c := example(m.Name)
				c.Reverse = rev
				p, err := Build(c)
				if err != nil {
					t.Fatal(err)
				}
				peer, err := Build(Peer(p.Config))
				if err != nil {
					t.Fatal(err)
				}
				if p.Config.ID != peer.Config.ID || p.Config.Port != peer.Config.Port || p.Config.MTU != peer.Config.MTU {
					t.Fatal("paired settings diverged")
				}
				if p.Local != peer.Remote || p.Remote != peer.Local {
					t.Fatalf("underlays do not mirror: %s/%s, %s/%s", p.Local, p.Remote, peer.Local, peer.Remote)
				}
				if len(p.Interfaces) == 0 || len(p.Steps) == 0 {
					t.Fatal("method has no data plane")
				}
				if rev && m.Name != "wireguard" && len(p.Interfaces) < 2 {
					t.Fatal("reverse omitted its carrier device")
				}
			})
		}
	}
}

func TestInvalidNativeSettingsCannotReachNetworkCommands(t *testing.T) {
	for _, change := range []func(*config.KernelConfig){
		func(c *config.KernelConfig) { c.Method = "unknown" }, func(c *config.KernelConfig) { c.LocalAddr = "0.0.0.0" }, func(c *config.KernelConfig) { c.RemoteAddr = "bad;reboot" }, func(c *config.KernelConfig) { c.Interface = "../other" }, func(c *config.KernelConfig) { c.Token = "short" }, func(c *config.KernelConfig) { c.MTU = 1 }, func(c *config.KernelConfig) { c.Lanes = 9 }, func(c *config.KernelConfig) { c.PeerIP = "::1" }, func(c *config.KernelConfig) { c.Port = 500 },
	} {
		c := example("gre")
		change(&c)
		if _, err := Build(c); err == nil {
			t.Fatalf("accepted %+v", c)
		}
	}
}

func TestNativeCreationFailureOnlyUnwindsCompletedResources(t *testing.T) {
	p, err := Build(example("6to4"))
	if err != nil {
		t.Fatal(err)
	}
	var expected [][]string
	fail := len(p.Steps) - 1
	for _, s := range p.Steps[:fail] {
		if len(s.Undo) > 0 {
			expected = append([][]string{s.Undo}, expected...)
		}
	}
	var undone [][]string
	position := 0
	run := func(_ context.Context, _ string, args []string, _ io.Reader) ([]byte, error) {
		if args[0] == "-j" {
			return nil, errors.New("absent")
		}
		if position == fail {
			position++
			return nil, errors.New("injected creation failure")
		}
		if position > fail {
			undone = append(undone, args)
		}
		position++
		return nil, nil
	}
	_, err = execute(context.Background(), p, run)
	if err == nil {
		t.Fatal("failure was hidden")
	}
	if !reflect.DeepEqual(undone, expected) {
		t.Fatalf("cleanup %v, want %v", undone, expected)
	}
}

func TestAnOperatorsInterfaceIsNeverDeleted(t *testing.T) {
	p, _ := Build(example("gre"))
	calls := 0
	_, err := execute(context.Background(), p, func(_ context.Context, _ string, args []string, _ io.Reader) ([]byte, error) {
		calls++
		if args[0] != "-j" {
			t.Fatal("mutated an unowned interface")
		}
		return []byte(`[{"ifname":"operator","ifalias":"operator-owned"}]`), nil
	})
	if err == nil || calls != 1 {
		t.Fatal("existing interface was not refused")
	}
}

func TestParallelGREAndNativeWireGuardKeepTheirOwnRoutes(t *testing.T) {
	c := example("ecmp-lanes")
	p, err := Build(c)
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Interfaces) != 4 {
		t.Fatalf("got %d lanes", len(p.Interfaces))
	}
	for _, s := range p.Steps {
		if strings.Contains(strings.Join(s.Args, " "), "default") {
			t.Fatal("changed the host default route")
		}
	}
	a, _ := Resolve(example("wireguard"))
	b := Peer(a)
	get := func(text, key string) string {
		for _, line := range strings.Split(text, "\n") {
			if strings.HasPrefix(line, key+" = ") {
				return strings.TrimPrefix(line, key+" = ")
			}
		}
		return ""
	}
	private, err := base64.StdEncoding.DecodeString(get(wireguardConfig(a), "PrivateKey"))
	if err != nil {
		t.Fatal(err)
	}
	key, err := ecdh.X25519().NewPrivateKey(private)
	if err != nil {
		t.Fatal(err)
	}
	if base64.StdEncoding.EncodeToString(key.PublicKey().Bytes()) != get(wireguardConfig(b), "PublicKey") {
		t.Fatal("WireGuard peers cannot authenticate each other")
	}
	if get(wireguardConfig(a), "PresharedKey") != get(wireguardConfig(b), "PresharedKey") {
		t.Fatal("WireGuard PSKs do not match")
	}
}

func ipPacket(port uint16, seq byte) []byte {
	p := make([]byte, 64)
	p[0] = 0x45
	p[9] = 6
	copy(p[12:20], []byte{10, 0, 0, 1, 10, 0, 0, 2})
	binary.BigEndian.PutUint16(p[20:22], port)
	binary.BigEndian.PutUint16(p[22:24], 443)
	p[27] = seq
	return p
}
func TestReverseLanesPinFlowsRatherThanPackets(t *testing.T) {
	a, b := ipPacket(1000, 1), ipPacket(1000, 2)
	if flowHash(a) != flowHash(b) {
		t.Fatal("one TCP flow changes lanes")
	}
	if flowHash(a) == flowHash(ipPacket(1001, 1)) {
		t.Fatal("different flows cannot use different lanes")
	}
	gre := append(make([]byte, 24), a...)
	gre[0] = 0x45
	gre[9] = 47
	gre[22] = 8
	if flowHash(gre) != flowHash(a) {
		t.Fatal("GRE header hides the inner flow")
	}
}

type memoryDevice struct {
	input, output chan []byte
	done          chan struct{}
	once          sync.Once
}

func device() *memoryDevice {
	return &memoryDevice{input: make(chan []byte, 16), output: make(chan []byte, 16), done: make(chan struct{})}
}
func (d *memoryDevice) Read(p []byte) (int, error) {
	select {
	case b := <-d.input:
		return copy(p, b), nil
	case <-d.done:
		return 0, io.EOF
	}
}
func (d *memoryDevice) Write(p []byte) (int, error) {
	select {
	case d.output <- append([]byte(nil), p...):
		return len(p), nil
	case <-d.done:
		return 0, io.ErrClosedPipe
	}
}
func (d *memoryDevice) Close() error { d.once.Do(func() { close(d.done) }); return nil }

func TestReverseCarrierTransfersAuthenticatedPacketsAndJoinsOnCancellation(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	l.Close()
	c := example("gre")
	c.LocalAddr, c.RemoteAddr, c.Port, c.Reverse = "127.0.0.1", "127.0.0.2", port, true
	c, err = Resolve(c)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	a, b := device(), device()
	results := make(chan error, 2)
	go func() { results <- runReverse(ctx, c, a) }()
	go func() { results <- runReverse(ctx, Peer(c), b) }()
	deadline := time.NewTimer(8 * time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(50 * time.Millisecond)
	defer tick.Stop()
	packet := ipPacket(1234, 4)
	for {
		select {
		case <-tick.C:
			a.input <- packet
		case got := <-b.output:
			if !reflect.DeepEqual(got, packet) {
				t.Fatal("payload changed")
			}
			cancel()
			for i := 0; i < 2; i++ {
				select {
				case <-results:
				case <-time.After(time.Second):
					t.Fatal("reverse workers survived cancellation")
				}
			}
			return
		case err := <-results:
			t.Fatalf("reverse exited before traffic: %v", err)
		case <-deadline.C:
			t.Fatal("no authenticated traffic crossed the reverse carrier")
		}
	}
}
