package e2e

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/backpack/backpack/config"
	"github.com/backpack/backpack/internal/metrics"
)

type controlledUDPPath struct {
	addr    string
	blocked atomic.Bool
}

func newControlledUDPPath(t *testing.T, target string) *controlledUDPPath {
	t.Helper()
	l, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	destination, err := net.ResolveUDPAddr("udp", target)
	if err != nil {
		l.Close()
		t.Fatal(err)
	}
	p := &controlledUDPPath{addr: l.LocalAddr().String()}
	ctx, cancel := context.WithCancel(t.Context())
	var wg sync.WaitGroup
	var mu sync.Mutex
	peers := map[string]*net.UDPConn{}
	wg.Add(1)
	go func() {
		defer wg.Done()
		b := make([]byte, 65535)
		for {
			n, from, err := l.ReadFromUDP(b)
			if err != nil {
				return
			}
			if p.blocked.Load() {
				continue
			}
			mu.Lock()
			remote := peers[from.String()]
			if remote == nil {
				remote, err = net.DialUDP("udp", nil, destination)
				if err == nil {
					peers[from.String()] = remote
					wg.Add(1)
					go func(remote *net.UDPConn, from *net.UDPAddr) {
						defer wg.Done()
						defer remote.Close()
						stop := context.AfterFunc(ctx, func() { remote.Close() })
						defer stop()
						reply := make([]byte, 65535)
						for {
							n, err := remote.Read(reply)
							if err != nil {
								return
							}
							if !p.blocked.Load() {
								l.WriteToUDP(reply[:n], from)
							}
						}
					}(remote, from)
				}
			}
			mu.Unlock()
			if remote != nil {
				remote.Write(b[:n])
			}
		}
	}()
	t.Cleanup(func() {
		cancel()
		l.Close()
		mu.Lock()
		for _, v := range peers {
			v.Close()
		}
		mu.Unlock()
		wg.Wait()
	})
	return p
}

func TestAdaptiveNativeUDPRecoversTheSameFlowAcrossHysteria2AndTUIC(t *testing.T) {
	bin := os.Getenv("BP_SINGBOX_BINARY")
	if bin == "" {
		t.Skip("set BP_SINGBOX_BINARY for real native path failover")
	}
	cert, key := testCert(t)
	backend := startEchoBackend(t)
	udpBackend := startUDPEcho(t)
	internal, entry, udpEntry := freePort(t), freePort(t), freePort(t)
	s := baseServerConfig("tcp", internal, entry, backend.addr, "native-adaptive-secret-0123456789abcdef")
	c := baseClientConfig("tcp", s.BindAddr, s.Token, nil)
	on := true
	s.AcceptUDP = &on
	s.Heartbeat = 1
	c.Keepalive = 4
	s.Ports = append(s.Ports, fmt.Sprintf("127.0.0.1:%d=%s", udpEntry, udpBackend))
	fronts := map[string]*controlledUDPPath{}
	for _, protocol := range []string{"hysteria2", "tuic"} {
		listen := fmt.Sprintf("127.0.0.1:%d", freePort(t))
		sp := config.SingServerConfig{Binary: bin, Protocol: protocol, Listen: listen, ServerName: "127.0.0.1", Certificate: cert, Key: key, Password: "independent-helper-secret-0123456789", NativeUDP: true}
		if protocol == "tuic" {
			sp.UUID = "12345678-1234-4234-8234-123456789abc"
		}
		front := newControlledUDPPath(t, listen)
		fronts[protocol] = front
		cp := config.SingClientConfig{Binary: bin, Protocol: protocol, Server: front.addr, ServerName: sp.ServerName, CAFile: cert, Password: sp.Password, UUID: sp.UUID, NativeUDP: true}
		s.Paths = append(s.Paths, config.ManagedServerPath{Name: protocol, Sing: sp})
		c.Paths = append(c.Paths, config.ManagedClientPath{Name: protocol, Sing: cp})
	}
	c.PathPolicy = config.PathPolicy{ProbeInterval: 1, ProbeTimeout: 3, ProbeBytes: 1024, Failures: 1, Recoveries: 1, MinDwell: 30, Improvement: 90}
	tun := runPair(t, s, c, entry, internal)
	if err := tun.waitReady(30 * time.Second); err != nil {
		t.Fatal(err)
	}
	collector := metrics.NewCollector(t.TempDir(), "native-adaptive", "tcp", "client", nil, nil)
	deadline := time.Now().Add(20 * time.Second)
	var selected string
	for time.Now().Before(deadline) {
		snap := collector.Snapshot().ManagedPaths
		if snap != nil && snap.NativeUDPActive != "" {
			selected = snap.NativeUDPActive
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if selected == "" {
		t.Fatal("no verified native UDP candidate")
	}
	u, err := net.Dial("udp", fmt.Sprintf("127.0.0.1:%d", udpEntry))
	if err != nil {
		t.Fatal(err)
	}
	defer u.Close()
	check := func(label string, timeout time.Duration) {
		t.Helper()
		end := time.Now().Add(timeout)
		for time.Now().Before(end) {
			want := []byte(fmt.Sprintf("%s-%d", label, time.Now().UnixNano()))
			u.SetDeadline(time.Now().Add(500 * time.Millisecond))
			if _, err := u.Write(want); err != nil {
				continue
			}
			b := make([]byte, 200)
			n, err := u.Read(b)
			if err == nil && bytes.Equal(want, b[:n]) {
				return
			}
		}
		t.Fatalf("same UDP flow did not carry %s", label)
	}
	check("before-outage", 8*time.Second)
	fronts[selected].blocked.Store(true)
	other := "hysteria2"
	if selected == other {
		other = "tuic"
	}
	deadline = time.Now().Add(25 * time.Second)
	for {
		snap := collector.Snapshot().ManagedPaths
		if snap != nil && snap.NativeUDPActive == other {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("native path did not withdraw %s: %+v", selected, snap)
		}
		time.Sleep(100 * time.Millisecond)
	}
	check("after-native-path-switch", 10*time.Second)
	deadline = time.Now().Add(25 * time.Second)
	for {
		err = tun.roundTrip([]byte("new TCP forward on surviving native carrier"))
		if err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal(err)
		}
		time.Sleep(200 * time.Millisecond)
	}
	tun.Stop()
}
