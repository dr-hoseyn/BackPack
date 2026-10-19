//go:build linux

package manage

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/backpack/backpack/internal/app"
	"github.com/backpack/backpack/internal/updatehealth"
)

// This exercises ApplyLocalUpdate, TakeSnapshot and RestoreSnapshot together,
// including actual binary/config replacement. Run only inside the dedicated
// mount namespace prepared by .github/scripts/test-update-health-install.sh.
// The normal suite uses the real-tunnel decision tests without touching an install.
func TestUpdateHealthLocalUpdateRestoresBinaryAndConfiguration(t *testing.T) {
	if os.Getenv("BP_UPDATE_HEALTH_INSTALL") != "1" {
		t.Skip("requires the isolated update-install test wrapper")
	}
	selfNS, err := os.Readlink("/proc/self/ns/mnt")
	if err != nil {
		t.Fatal(err)
	}
	initNS, err := os.Readlink("/proc/1/ns/mnt")
	if err != nil || selfNS == initNS || os.Geteuid() != 0 {
		t.Fatal("refusing install test outside a private root mount namespace")
	}
	selfNet, err := os.Readlink("/proc/self/ns/net")
	if err != nil {
		t.Fatal(err)
	}
	initNet, err := os.Readlink("/proc/1/ns/net")
	if err != nil || selfNet == initNet {
		t.Fatal("refusing install test outside a private network namespace")
	}
	// Each destination must be a bind mount prepared by the wrapper, not just
	// a path inherited from the runner or the developer's machine.
	mounts, err := os.ReadFile("/proc/self/mountinfo")
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{app.ConfigDir, app.ServiceDir, app.InstallDir, filepath.Dir(app.BinPath)} {
		if !strings.Contains(string(mounts), " "+path+" ") {
			t.Fatalf("%s is not a private bind mount", path)
		}
	}
	oldBinary := "#!/bin/sh\necho previous-test-version\n"
	newBinary := "#!/bin/sh\necho new-test-version\n"
	if err := os.WriteFile(app.BinPath, []byte(oldBinary), 0755); err != nil {
		t.Fatal(err)
	}
	const name = "update-health-install"
	const payload = "backpack-health-install-ok\n"
	var baselineBroken atomic.Bool
	var configChanged atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		installed, _ := os.ReadFile(app.BinPath)
		if string(installed) == newBinary {
			if configChanged.CompareAndSwap(false, true) {
				before, err := os.ReadFile(app.ConfigPath(name))
				if err != nil {
					t.Error(err)
				}
				if err := os.WriteFile(app.ConfigPath(name), append(before, []byte("\n# changed by failed upgrade\n")...), 0600); err != nil {
					t.Error(err)
				}
			}
			io.WriteString(w, "new version cannot carry expected data")
			return
		}
		if baselineBroken.Load() {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		io.WriteString(w, payload)
	}))
	defer server.Close()
	_, port, _ := net.SplitHostPort(strings.TrimPrefix(server.URL, "http://"))
	originalConfig := "[server]\nbind_addr = \"127.0.0.1:19001\"\ntransport = \"tcp\"\ntoken = \"install-test-token\"\nports = [\"" + port + "=8080\"]\n"
	if err := os.WriteFile(app.ConfigPath(name), []byte(originalConfig), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(app.ServiceDir, app.ServiceName(name)), []byte("[Service]\n"), 0600); err != nil {
		t.Fatal(err)
	}
	policy := updatehealth.Policy{GraceSeconds: 1, Checks: []updatehealth.Check{{Tunnel: name, URL: server.URL, ExpectedBody: payload}}}
	policyJSON, _ := json.Marshal(policy)
	if err := os.WriteFile(filepath.Join(app.ConfigDir, "update-health.json"), policyJSON, 0600); err != nil {
		t.Fatal(err)
	}
	fakeBin := t.TempDir()
	if err := os.WriteFile(filepath.Join(fakeBin, "systemctl"), []byte("#!/bin/sh\necho active\nexit 0\n"), 0755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", fakeBin+":"+os.Getenv("PATH"))
	archive := filepath.Join(t.TempDir(), LocalAssetName())
	f, err := os.Create(archive)
	if err != nil {
		t.Fatal(err)
	}
	gz := gzip.NewWriter(f)
	tw := tar.NewWriter(gz)
	if err := tw.WriteHeader(&tar.Header{Name: "backpack", Mode: 0755, Size: int64(len(newBinary)), Typeflag: tar.TypeReg}); err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(tw, newBinary); err != nil {
		t.Fatal(err)
	}
	for _, closer := range []io.Closer{tw, gz, f} {
		if err := closer.Close(); err != nil {
			t.Fatal(err)
		}
	}
	u := LocalUpdate{Path: archive, Version: "new-test-version"}
	baselineBroken.Store(true)
	err = ApplyLocalUpdate(u, func(s string) { t.Log(s) })
	if err == nil || !strings.Contains(err.Error(), "update not installed") {
		t.Fatalf("bad baseline did not stop installation: %v", err)
	}
	if installed, _ := os.ReadFile(app.BinPath); string(installed) != oldBinary {
		t.Fatal("binary replaced despite failing baseline")
	}
	if snapshots, _ := filepath.Glob(app.InstallDir + "/snapshots/*"); len(snapshots) != 0 {
		t.Fatal("baseline failure reached the installation/snapshot phase")
	}
	baselineBroken.Store(false)
	err = ApplyLocalUpdate(u, func(s string) { t.Log(s) })
	if err == nil || !strings.Contains(err.Error(), "rolled back") || !configChanged.Load() {
		t.Fatalf("did not install, fail the payload check, and roll back: %v", err)
	}
	if installed, _ := os.ReadFile(app.BinPath); string(installed) != oldBinary {
		t.Fatal("previous binary was not restored byte for byte")
	}
	if restored, _ := os.ReadFile(app.ConfigPath(name)); string(restored) != originalConfig {
		t.Fatalf("previous configuration was not restored: %q", restored)
	}
	if _, err := os.Stat(archive); err != nil {
		t.Fatal("failed update archive was removed")
	}
	if err := updatehealth.Probe(context.Background(), policy.Checks[0]); err != nil {
		t.Fatalf("restored version did not pass its original check: %v", err)
	}
}
