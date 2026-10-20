//go:build linux

package optimize

import (
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Exhaust and recover backend source ports inside a new network namespace.
func TestHighConcurrencyRealLoopbackCapacity(t *testing.T) {
	if os.Getenv("BP_CAPACITY_NETNS_TEST") != "1" {
		t.Skip("set BP_CAPACITY_NETNS_TEST=1 as root to exercise an isolated kernel namespace")
	}
	if os.Getenv("BP_CAPACITY_NETNS_CHILD") != "1" {
		cmd := exec.Command("unshare", "--net", os.Args[0], "-test.run=^TestHighConcurrencyRealLoopbackCapacity$", "-test.v")
		cmd.Env = append(os.Environ(), "BP_CAPACITY_NETNS_CHILD=1")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("namespace test: %v\n%s", err, out)
		} else {
			t.Log(string(out))
		}
		return
	}
	if out, err := exec.Command("ip", "link", "set", "lo", "up").CombinedOutput(); err != nil {
		t.Fatalf("lo: %v %s", err, out)
	}
	dir := t.TempDir()
	capacityLockFile = filepath.Join(dir, "policy.lock")
	capacityFile = filepath.Join(dir, "capacity.conf")
	capacityBackup = filepath.Join(dir, "backup.json")
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	port := listener.Addr().(*net.TCPAddr).Port
	if err := capacitySysctl("net.ipv4.ip_local_port_range", "40000 40015"); err != nil {
		t.Fatal(err)
	}
	if err := capacitySysctl("net.ipv4.ip_local_reserved_ports", fmt.Sprint(port)); err != nil {
		t.Fatal(err)
	}
	var held []net.Conn
	defer func() {
		for _, c := range held {
			c.Close()
		}
	}()
	dial := func() error {
		client, err := net.Dial("tcp4", listener.Addr().String())
		if err != nil {
			return err
		}
		held = append(held, client)
		server, err := listener.Accept()
		if err != nil {
			return err
		}
		held = append(held, server)
		return nil
	}
	var exhausted error
	for i := 0; i < 20; i++ {
		if exhausted = dial(); exhausted != nil {
			break
		}
	}
	if exhausted == nil || !strings.Contains(exhausted.Error(), "cannot assign requested address") {
		t.Fatalf("expected source-port exhaustion, got %v", exhausted)
	}
	if _, err := EnableHighConcurrency(nil); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 32; i++ {
		if err := dial(); err != nil {
			t.Fatalf("profile failed to restore backend capacity: %v", err)
		}
	}
	if err := RestoreHighConcurrency(); err != nil {
		t.Fatal(err)
	}
	s, err := currentCapacity()
	if err != nil || s.Range != "40000\t40015" {
		t.Fatalf("original range not restored: %+v %v", s, err)
	}
}

func TestConcurrentKernelPolicyChangesLeaveSnapshotAndSysctlsAlone(t *testing.T) {
	_, calls := capacityFixture(t)
	unlock, err := lockKernelPolicy()
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	if _, err := EnableHighConcurrency(nil); err == nil {
		t.Fatal("conflicting enable accepted")
	}
	if err := RestoreHighConcurrency(); err == nil {
		t.Fatal("conflicting restore accepted")
	}
	var logs []string
	Apply(func(s string) { logs = append(logs, s) }, nil)
	if len(logs) != 1 || !strings.Contains(logs[0], "another kernel policy update") {
		t.Fatal(logs)
	}
	if len(*calls) != 0 {
		t.Fatal("conflicting operation changed sysctls", *calls)
	}
	if _, err := os.Stat(capacityBackup); !os.IsNotExist(err) {
		t.Fatal("conflicting operation changed rollback snapshot", err)
	}
}
