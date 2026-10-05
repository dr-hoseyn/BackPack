//go:build linux

package core

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/backpack/backpack/internal/app"
)

func TestPckDeletionCleansStoppedTunnelAndPreservesFailedStop(t *testing.T) {
	if os.Getenv("BACKPACK_PCK_NAMESPACE") != "1" || os.Geteuid() != 0 {
		t.Skip("requires isolated root network namespace")
	}
	for _, failedStop := range []bool{false, true} {
		t.Run(map[bool]string{false: "stopped", true: "stop-failed"}[failedStop], func(t *testing.T) {
			oldRun := runSystemctl
			if err := os.MkdirAll(app.ConfigDir, 0755); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				runSystemctl = oldRun
				unitCache.Forget()
			})
			unitCache.Forget()
			runSystemctl = func(args ...string) (string, error) {
				if args[0] == "is-active" {
					return "active", nil
				}
				if args[0] == "is-enabled" {
					return "enabled", nil
				}
				if args[0] == "disable" && failedStop {
					return "", errors.New("stop failed")
				}
				return "", nil
			}
			const token = "pck-deletion-regression-token"
			sum := sha256.Sum256([]byte("backpack-pck-v1:" + token))
			tag := "backpack-pck-" + hex.EncodeToString(sum[:4]) + "-44000:44127"
			rule := []string{"OUTPUT", "-p", "tcp", "--sport", "44000:44127", "--tcp-flags", "RST", "RST", "-m", "comment", "--comment", tag, "-j", "DROP"}
			if out, err := exec.Command("iptables", append([]string{"-I"}, rule...)...).CombinedOutput(); err != nil {
				t.Fatalf("fixture: %v %s", err, out)
			}
			t.Cleanup(func() { exec.Command("iptables", append([]string{"-D"}, rule...)...).Run() })
			const name = "pck-delete-regression-fixture"
			path := app.ConfigPath(name)
			if _, err := os.Stat(path); !os.IsNotExist(err) {
				t.Fatal("fixture already exists")
			}
			t.Cleanup(func() { os.Remove(path) })
			if err := os.WriteFile(path, []byte("[server]\nbind_addr=\"192.0.2.1:53565\"\ntransport=\"tcpmux\"\ntoken=\""+token+"\"\n"), 0600); err != nil {
				t.Fatal(err)
			}
			err := Delete(name)
			out, listErr := exec.Command("iptables", "-S", "OUTPUT").Output()
			if listErr != nil {
				t.Fatal(listErr)
			}
			_, statErr := os.Stat(path)
			if failedStop {
				if err == nil || statErr != nil || !strings.Contains(string(out), tag) {
					t.Fatal("failed stop removed config or live rules")
				}
			} else if err != nil || !os.IsNotExist(statErr) || strings.Contains(string(out), tag) {
				t.Fatalf("stopped tunnel was not removed cleanly: %v %v %s", err, statErr, out)
			}
		})
	}
}
