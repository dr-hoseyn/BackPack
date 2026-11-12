//go:build linux

package kernel

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// Opt in with BACKPACK_TEST_XGRE_NETNS=1. Both peers run inside disposable
// network namespaces; the opposite endpoint is configured with standard Linux
// GRE commands matching the observed unencrypted BPP profile, not BPP code.
func TestXGREKernelInterop(t *testing.T) {
	if os.Getenv("BACKPACK_TEST_XGRE_NETNS") != "1" {
		t.Skip("set BACKPACK_TEST_XGRE_NETNS=1 for isolated kernel interoperability test")
	}
	if os.Getenv("BP_XGRE_CHILD") == "1" {
		xgreKernelFixture(t)
		return
	}
	for _, tool := range []string{"unshare", "nsenter", "ip", "ping", "sysctl"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s is unavailable", tool)
		}
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "unshare", "-n", exe, "-test.run=^TestXGREKernelInterop$", "-test.v")
	cmd.Env = append(os.Environ(), "BP_XGRE_CHILD=1")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("isolated xGRE interop: %v\n%s", err, out)
	}
	t.Log(string(out))
}

func TestXGREKernelPeer(t *testing.T) {
	if os.Getenv("BP_XGRE_PEER") != "1" {
		t.Skip("namespace helper")
	}
	var start [1]byte
	if _, err := io.ReadFull(os.Stdin, start[:]); err != nil {
		t.Fatal(err)
	}
	ln, err := net.ListenTCP("tcp4", &net.TCPAddr{IP: net.ParseIP("10.77.80.2"), Port: 45992})
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	_ = ln.SetDeadline(time.Now().Add(20 * time.Second))
	fmt.Println("XGRE_PEER_READY")
	conn, err := ln.AcceptTCP()
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(20 * time.Second))
	if _, err := io.Copy(conn, conn); err != nil {
		t.Fatal(err)
	}
}

func xgreKernelFixture(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	peer := exec.CommandContext(ctx, "unshare", "-n", exe, "-test.run=^TestXGREKernelPeer$", "-test.v")
	peer.Env = append(os.Environ(), "BP_XGRE_PEER=1")
	input, err := peer.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	output, err := peer.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	var peerErrors bytes.Buffer
	peer.Stderr = &peerErrors
	if err := peer.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = input.Close(); _ = peer.Process.Kill(); _ = peer.Wait() }()
	selfNS, _ := os.Readlink("/proc/self/ns/net")
	deadline := time.Now().Add(3 * time.Second)
	for {
		ns, err := os.Readlink(fmt.Sprintf("/proc/%d/ns/net", peer.Process.Pid))
		if err == nil && ns != selfNS {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("peer did not enter its network namespace")
		}
		time.Sleep(10 * time.Millisecond)
	}
	run := func(inPeer bool, args ...string) string {
		t.Helper()
		name := args[0]
		if inPeer {
			args = append([]string{"-t", fmt.Sprint(peer.Process.Pid), "-n", name}, args[1:]...)
			name = "nsenter"
		} else {
			args = args[1:]
		}
		out, err := exec.CommandContext(ctx, name, args...).CombinedOutput()
		if err != nil {
			t.Fatalf("%s %v: %v\n%s", name, args, err, out)
		}
		return string(out)
	}
	run(false, "ip", "link", "set", "lo", "up")
	run(false, "ip", "link", "add", "xgva", "type", "veth", "peer", "name", "xgvb")
	run(false, "ip", "link", "set", "xgvb", "netns", fmt.Sprint(peer.Process.Pid))
	run(false, "ip", "address", "add", "192.0.2.1/30", "dev", "xgva")
	run(false, "ip", "link", "set", "xgva", "mtu", "1500", "up")
	run(true, "ip", "link", "set", "lo", "up")
	run(true, "ip", "address", "add", "192.0.2.2/30", "dev", "xgvb")
	run(true, "ip", "link", "set", "xgvb", "mtu", "1500", "up")
	run(true, "ip", "link", "add", "bppcompat", "type", "gre", "local", "192.0.2.2", "remote", "192.0.2.1", "key", "4045620583", "ttl", "255", "tos", "inherit", "nopmtudisc")
	run(true, "ip", "address", "add", "10.77.80.2/30", "dev", "bppcompat")
	run(true, "ip", "link", "set", "bppcompat", "mtu", "1472", "up")
	run(true, "sysctl", "-w", "net.ipv4.conf.bppcompat.rp_filter=2")
	c := example("xgre")
	c.Interface, c.LocalIP, c.PeerIP, c.ID = "bpkxglive", "10.77.80.1/30", "10.77.80.2", 0xf1234567
	ready := make(chan struct{}, 1)
	done := make(chan error, 1)
	completed := false
	go func() {
		done <- RunManaged(ctx, c, func(up bool) {
			if up {
				ready <- struct{}{}
			}
		})
	}()
	defer func() {
		cancel()
		if !completed {
			select {
			case err := <-done:
				if err != nil {
					t.Errorf("cleanup: %v", err)
				}
			case <-time.After(12 * time.Second):
				t.Error("xGRE teardown did not finish")
			}
		}
		if err := exec.Command("ip", "link", "show", "dev", c.Interface).Run(); err == nil {
			t.Error("owned xGRE interface survived teardown")
		}
	}()
	select {
	case <-ready:
	case err := <-done:
		completed = true
		t.Fatalf("xGRE start: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("xGRE did not report readiness")
	}
	for key, want := range map[string]string{"rp_filter": "2", "forwarding": "1"} {
		b, err := os.ReadFile("/proc/sys/net/ipv4/conf/" + c.Interface + "/" + key)
		if err != nil || strings.TrimSpace(string(b)) != want {
			t.Fatalf("interface policy %s = %q, %v", key, b, err)
		}
	}
	run(false, "ping", "-n", "-I", c.Interface, "-c", "3", "-W", "1", "-M", "do", "-s", "1444", c.PeerIP)
	if err := exec.CommandContext(ctx, "ping", "-n", "-I", c.Interface, "-c", "1", "-W", "1", "-M", "do", "-s", "1445", c.PeerIP).Run(); err == nil {
		t.Fatal("configured MTU was not enforced")
	}
	if _, err := input.Write([]byte{1}); err != nil {
		t.Fatal(err)
	}
	peerReady := make(chan bool, 1)
	go func() {
		scan := bufio.NewScanner(output)
		for scan.Scan() {
			if scan.Text() == "XGRE_PEER_READY" {
				peerReady <- true
				return
			}
		}
		peerReady <- false
	}()
	select {
	case ok := <-peerReady:
		if !ok {
			t.Fatalf("peer listener failed: %s", &peerErrors)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("peer listener was not ready")
	}
	conn, err := net.DialTimeout("tcp4", net.JoinHostPort(c.PeerIP, "45992"), 3*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(15 * time.Second))
	payload := bytes.Repeat([]byte("xgre-verified-payload"), 400000)
	written := make(chan error, 1)
	go func() {
		_, err := io.Copy(conn, bytes.NewReader(payload))
		if err == nil {
			err = conn.(*net.TCPConn).CloseWrite()
		}
		written <- err
	}()
	got, err := io.ReadAll(io.LimitReader(conn, int64(len(payload)+1)))
	if err != nil {
		t.Fatal(err)
	}
	if err := <-written; err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatalf("verified TCP round trip differs: %d/%d bytes", len(got), len(payload))
	}
	t.Logf("verified %d-byte TCP round trip and the 1472-byte MTU against a standard keyed GRE peer", len(payload))
}
