package core

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Systemctl runs the binary that exists: systemctl, lower case.
//
// When the function was exported during the package split, the rename reached
// the string inside it too, and every start, stop, restart, enable and
// daemon-reload went to a program called "Systemctl" — which no Linux has, so
// every one of them failed with "executable file not found". Nothing noticed:
// no test ran a real command. This one runs a stand-in named exactly as the
// real one and checks it was reached.
func TestSystemctlRunsTheRealBinaryName(t *testing.T) {
	dir := t.TempDir()
	stub := filepath.Join(dir, "systemctl")
	if err := os.WriteFile(stub, []byte("#!/bin/sh\necho \"stub:$*\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	out, err := Systemctl("is-active", "backpack-x")
	if err != nil {
		t.Fatalf("Systemctl did not reach a binary named systemctl: %v", err)
	}
	if !strings.Contains(out, "stub:is-active backpack-x") {
		t.Fatalf("Systemctl output = %q", out)
	}
}

func TestSystemctlDoesNotInheritMonitorNotifySocket(t *testing.T) {
	dir := t.TempDir()
	stub := filepath.Join(dir, "systemctl")
	if err := os.WriteFile(stub, []byte("#!/bin/sh\nprintf '%s\\n' \"notify=${NOTIFY_SOCKET-unset}\" \"marker=$BP_TEST_MARKER\" \"args=$*\"\n"), 0755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	t.Setenv("NOTIFY_SOCKET", "/private/monitor-notify")
	t.Setenv("BP_TEST_MARKER", "preserved")
	out, err := Systemctl("show", "backpack-monitor.service")
	if err != nil {
		t.Fatal(err)
	}
	want := "notify=unset\nmarker=preserved\nargs=show backpack-monitor.service"
	if out != want {
		t.Fatalf("child environment or arguments changed: %q", out)
	}
	if os.Getenv("NOTIFY_SOCKET") != "/private/monitor-notify" {
		t.Fatal("monitor notification environment was modified")
	}
}
