//go:build linux

package manage

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// This exercises AWG through Connection Test itself, not just its standalone
// engine: identity handoff, the Iran forward, peer echo, verdict and speed.
func TestAmneziaWGConnectionTestOverRealIPv4AndIPv6TUN(t *testing.T) {
	ctTestNativeConnectionTestBothFamilies(t, "awg", "awg")
}

func TestConnectIPConnectionTestOverRealIPv4AndIPv6TUN(t *testing.T) {
	ctTestNativeConnectionTestBothFamilies(t, "direct", "connect-ip")
}

func ctTestNativeConnectionTestBothFamilies(t *testing.T, kind, transport string) {
	t.Helper()
	if os.Getenv("BP_L3_LIVE") == "" {
		t.Skip("set BP_L3_LIVE=1 for isolated root network namespaces")
	}
	bin := filepath.Join(t.TempDir(), "backpack")
	build := exec.Command("go", "build", "-o", bin, "../..")
	build.Stderr = os.Stderr
	if err := build.Run(); err != nil {
		t.Fatal(err)
	}
	script, _ := filepath.Abs(filepath.Join("testdata", "conntestlive.sh"))
	self, _ := os.Executable()
	for _, family := range []string{"IPv4", "IPv6"} {
		t.Run(family, func(t *testing.T) {
			w := t.TempDir()
			ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
			defer cancel()
			args := []string{"--net", "--mount", "--fork", "--", script}
			if os.Geteuid() != 0 {
				args = append([]string{"--map-auto", "--map-root-user"}, args...)
			}
			cmd := exec.CommandContext(ctx, "unshare", args...)
			cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
			cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
			v6 := ""
			if family == "IPv6" {
				v6 = "1"
			}
			cmd.Env = append(os.Environ(), "BIN="+bin, "TESTBIN="+self, "W="+w, "SOAK=3", "ONLY="+kind+"/"+transport, "FILTER=", "CT_IPV6="+v6, "PATHMTU=1400")
			out, err := cmd.CombinedOutput()
			if err != nil || !strings.Contains(string(out), "RESULT DONE") {
				t.Fatalf("%v\n%s", err, out)
			}
			raw, err := os.ReadFile(filepath.Join(w, "iran.json"))
			if err != nil {
				t.Fatal(err)
			}
			var rows []ConnTestResult
			if err := json.Unmarshal(raw, &rows); err != nil {
				t.Fatal(err)
			}
			found := false
			for _, r := range rows {
				if r.Kind == kind && r.Transport == transport {
					found = true
					if r.Status != ctOK || r.Mbps <= 0 || r.UploadMbps <= 0 {
						t.Fatalf("%s did not carry bidirectional traffic: %+v\n%s", transport, r, out)
					}
				}
			}
			if !found {
				t.Fatalf("%s was missing from Connection Test", transport)
			}
		})
	}
}
