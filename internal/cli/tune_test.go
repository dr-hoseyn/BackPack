package cli

import (
	"errors"
	"reflect"
	"strings"
	"testing"
)

func TestTuneCapacityCLIAndErrors(t *testing.T) {
	root, enable, restore := isRoot, enableHighConcurrency, restoreHighConcurrency
	t.Cleanup(func() { isRoot, enableHighConcurrency, restoreHighConcurrency = root, enable, restore })
	isRoot = func() bool { return true }
	var got []int
	enableHighConcurrency = func(p []int) (string, error) { got = append([]int(nil), p...); return "62050-62051", nil }
	if !IsCommand("tune") {
		t.Fatal("main cannot dispatch tune")
	}
	r := Run([]string{"tune", "high-concurrency", "--reserve-ports", "62050-62051"})
	if r.Code != 0 || len(got) < 2 || !reflect.DeepEqual(got[:2], []int{62050, 62051}) || !strings.Contains(r.Out, "10240 65535") {
		t.Fatal(r, got)
	}
	for _, a := range [][]string{{"tune"}, {"tune", "high-concurrency", "--reserve-ports"}, {"tune", "high-concurrency", "--reserve-ports=bad"}, {"tune", "restore", "extra"}, {"tune", "path-mtu", "a", "1501"}, {"tune", "path-mtu", "a", "575"}} {
		if r := Run(a); r.Code != CodeUsage {
			t.Fatal(a, r)
		}
	}
	enableHighConcurrency = func([]int) (string, error) { return "", errors.New("refused") }
	if r := Run([]string{"tune", "high-concurrency"}); r.Code != CodeFailed {
		t.Fatal(r)
	}
	restoreHighConcurrency = func() error { return nil }
	if r := Run([]string{"tune", "restore"}); r.Code != 0 {
		t.Fatal(r)
	}
	isRoot = func() bool { return false }
	if r := Run([]string{"tune", "high-concurrency"}); r.Code != CodeFailed {
		t.Fatal(r)
	}
}

func TestTuneMeasuredPathUsesExistingMSSSetting(t *testing.T) {
	root, set := isRoot, tuneMSS
	t.Cleanup(func() { isRoot, tuneMSS = root, set })
	isRoot = func() bool { return true }
	var name string
	var mss int
	tuneMSS = func(n string, m int) error { name, mss = n, m; return nil }
	r := Run([]string{"tune", "path-mtu", "server-8443", "1492"})
	if r.Code != 0 || name != "server-8443" || mss != 1440 {
		t.Fatal(r, name, mss)
	}
	if !strings.Contains(r.Out, "restarted") {
		t.Fatal("restart side effect not reported")
	}
	r = Run([]string{"tune", "path-mtu", "server-8443", "0"})
	if r.Code != 0 || mss != 0 {
		t.Fatal(r, mss)
	}
	tuneMSS = func(string, int) error { return errors.New("UDP transport") }
	if r = Run([]string{"tune", "path-mtu", "kcp", "1492"}); r.Code != CodeFailed {
		t.Fatal(r)
	}
}
