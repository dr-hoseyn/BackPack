//go:build linux

package network

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

func insertIncidentRules(t *testing.T, token string, base uint16) {
	t.Helper()
	for _, rule := range pckRules(pckTunnelID(token), base, base+127) {
		if out, err := exec.Command("iptables", append([]string{"-t", rule[0], "-I"}, rule[1:]...)...).CombinedOutput(); err != nil {
			t.Fatalf("insert fixture: %v %s", err, out)
		}
	}
	t.Cleanup(func() { CleanupPckGuards(token) })
}

func TestPckLifecycleOrphansAndLivePool(t *testing.T) {
	incidentNamespace(t)
	const token, other = "lifecycle-pool-token", "lifecycle-unrelated-token"
	insertIncidentRules(t, token, 41000)
	insertIncidentRules(t, token, 42000)
	insertIncidentRules(t, other, 43000)
	CleanupPckGuards(token)
	CleanupPckGuards(token)
	if incidentRules(t, pckTunnelID(token)) != 0 || incidentRules(t, pckTunnelID(other)) != 3 {
		t.Fatal("cleanup did not isolate the requested tunnel")
	}
	c1 := incidentCarrier(t, false, token, 52779)
	c2 := incidentCarrier(t, false, token, 52779)
	CleanupPckGuards(token)
	if c1.local == c2.local || incidentRules(t, pckTunnelID(token)) != 3 {
		t.Fatal("live pool lost its shared guard or ports collided")
	}
	c1.Close()
	c1.Close()
	if incidentRules(t, pckTunnelID(token)) != 3 {
		t.Fatal("closing one carrier removed another carrier's guard")
	}
	c2.Close()
	CleanupPckGuards(token)
	if incidentRules(t, pckTunnelID(token)) != 0 {
		t.Fatal("last carrier left rules behind")
	}
}

func TestPckLifecycleProcessHelper(t *testing.T) {
	mode := os.Getenv("BACKPACK_PCK_HELPER")
	if mode == "" {
		return
	}
	const token = "lifecycle-external-owner-token"
	if mode == "legacy" {
		fd, err := unix.Socket(unix.AF_PACKET, unix.SOCK_RAW|unix.SOCK_CLOEXEC, int(htons(unix.ETH_P_IP)))
		if err != nil {
			t.Fatal(err)
		}
		defer unix.Close(fd)
		insertIncidentRules(t, token, 46000)
	} else {
		incidentCarrier(t, true, token, 52780)
	}
	fmt.Println("READY")
	io.Copy(io.Discard, os.Stdin)
}

func externalIncidentOwner(t *testing.T, mode string) (*exec.Cmd, io.WriteCloser) {
	t.Helper()
	binary := os.Args[0]
	if mode == "legacy" {
		binary = filepath.Join(t.TempDir(), "renamed-carrier")
		if err := os.Link(os.Args[0], binary); err != nil {
			t.Fatal(err)
		}
	}
	cmd := exec.Command(binary, "-test.run=^TestPckLifecycleProcessHelper$")
	cmd.Env = append(os.Environ(), "BACKPACK_PCK_HELPER="+mode)
	in, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { in.Close(); cmd.Process.Kill(); cmd.Wait() })
	scan := bufio.NewScanner(out)
	for scan.Scan() {
		if scan.Text() == "READY" {
			return cmd, in
		}
	}
	t.Fatal("owner did not start")
	return nil, nil
}

func TestPckLifecyclePreservesAnotherProcessAndRecoversAfterKill(t *testing.T) {
	incidentNamespace(t)
	const token = "lifecycle-external-owner-token"
	for _, mode := range []string{"updated", "legacy"} {
		t.Run(mode, func(t *testing.T) {
			cmd, _ := externalIncidentOwner(t, mode)
			CleanupPckGuards(token)
			if incidentRules(t, pckTunnelID(token)) != 3 {
				t.Fatal("cleanup removed the live external owner's rules")
			}
			if mode == "updated" {
				// A different live tunnel remains usable while the orphan sweep
				// and a competing attempt with the same token are refused.
				other := incidentCarrier(t, false, "lifecycle-parallel-token", 52781)
				other.Close()
				pc, err := newPckConn(true, 52782, PcapCarrier{Port: 52782, Token: token, Interface: "bp-test", GatewayMAC: "02:00:00:00:00:02"})
				if err == nil {
					pc.Close()
					t.Fatal("a second process acquired the live owner's rules")
				}
			}
			cmd.Process.Kill()
			cmd.Wait()
			if incidentRules(t, pckTunnelID(token)) != 3 {
				t.Fatal("kill fixture did not leave orphan rules")
			}
			CleanupPckGuards(token)
			if incidentRules(t, pckTunnelID(token)) != 0 {
				t.Fatal("crashed owner's rules were not recovered")
			}
		})
	}
}

func TestPckLifecycleSkipsOccupiedPortAndReleasesReservations(t *testing.T) {
	incidentNamespace(t)
	const token = "lifecycle-occupied-token"
	base := pckClientPortBase(token)
	ip := net.ParseIP("192.0.2.1")
	ln, err := net.ListenTCP("tcp4", &net.TCPAddr{IP: ip, Port: int(base)})
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	c := incidentCarrier(t, false, token, 52783)
	if c.local == base {
		t.Fatal("PCK reused an occupied kernel port")
	}
	port := c.local
	c.Close()
	reused, err := net.ListenTCP("tcp4", &net.TCPAddr{IP: ip, Port: int(port)})
	if err != nil {
		t.Fatalf("closed carrier leaked reservation: %v", err)
	}
	reused.Close()
	pc, err := newPckConn(true, base, PcapCarrier{Port: base, Token: token, Interface: "bp-test", GatewayMAC: "02:00:00:00:00:02"})
	if err == nil {
		pc.Close()
		t.Fatal("server stole an existing TCP listener")
	}
	if incidentRules(t, pckTunnelID(token)) != 0 {
		t.Fatal("failed initialization installed rules")
	}
}

func TestPckLifecyclePartialGuardInstallationIsRemoved(t *testing.T) {
	incidentNamespace(t)
	real, err := exec.LookPath("iptables")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	wrapper := "#!/bin/sh\ncase \"$*\" in *\"-t raw -I OUTPUT\"*) exit 1;; esac\nexec " + real + " \"$@\"\n"
	if err := os.WriteFile(filepath.Join(dir, "iptables"), []byte(wrapper), 0755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))
	const token = "lifecycle-partial-guard-token"
	pc, err := newPckConn(true, 52784, PcapCarrier{Port: 52784, Token: token, Interface: "bp-test", GatewayMAC: "02:00:00:00:00:02"})
	if err != nil {
		t.Fatal(err)
	}
	c := pc.(*pckConn)
	if c.GuardInstalled() || incidentRules(t, pckTunnelID(token)) != 2 {
		t.Fatal("partial installation reported success or wrong rule count")
	}
	c.Close()
	if incidentRules(t, pckTunnelID(token)) != 0 {
		t.Fatal("partial guard leaked rules")
	}
	// Verify exact client scope and ensure protocol parsing precedes port flags.
	r := scopedPckRules("12345678", 40000, 40127, "bp-test", "192.0.2.1", "192.0.2.2", 52784)
	if !strings.Contains(strings.Join(r[0], " "), "-p tcp -o bp-test -s 192.0.2.1 -d 192.0.2.2 --dport 52784") {
		t.Fatal("wire scope was not preserved")
	}
}

func TestPckLifecycleRawCarrierStillStartsWithoutIptables(t *testing.T) {
	incidentNamespace(t)
	t.Setenv("PATH", t.TempDir())
	const token = "lifecycle-raw-only-token"
	pc, err := newPckConn(true, 52785, PcapCarrier{Port: 52785, Token: token, Interface: "bp-test", GatewayMAC: "02:00:00:00:00:02"})
	if err != nil {
		t.Fatal(err)
	}
	c := pc.(*pckConn)
	if c.GuardInstalled() {
		t.Fatal("missing iptables was reported as a complete guard")
	}
	c.Close()
	guardMu.Lock()
	defer guardMu.Unlock()
	if pckOwners[pckTunnelID(token)] != nil {
		t.Fatal("raw-only carrier retained firewall ownership")
	}
}
