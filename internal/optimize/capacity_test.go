package optimize

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func capacityFixture(t *testing.T) (*capacityState, *[]string) {
	t.Helper()
	oldFile, oldBackup, oldNet, oldIPv4, oldSet := capacityFile, capacityBackup, procNetDir, procIPv4Dir, capacitySysctl
	t.Cleanup(func() {
		capacityFile, capacityBackup, procNetDir, procIPv4Dir, capacitySysctl = oldFile, oldBackup, oldNet, oldIPv4, oldSet
	})
	dir := t.TempDir()
	capacityFile, capacityBackup = filepath.Join(dir, "sysctl.d", "capacity.conf"), filepath.Join(dir, "config", "backup.json")
	procNetDir, procIPv4Dir = filepath.Join(dir, "net"), filepath.Join(dir, "ipv4")
	for _, d := range []string{procNetDir, procIPv4Dir} {
		if err := os.MkdirAll(d, 0755); err != nil {
			t.Fatal(err)
		}
	}
	s := &capacityState{Range: "32768 60999", Reserved: "9000,62050-62051"}
	write := func(name, value string) {
		t.Helper()
		if err := os.WriteFile(name, []byte(value), 0600); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join(procIPv4Dir, "ip_local_port_range"), s.Range)
	write(filepath.Join(procIPv4Dir, "ip_local_reserved_ports"), s.Reserved)
	write(filepath.Join(procNetDir, "tcp"), "  sl local_address rem_address st\n 0: 0100007F:4BB8 00000000:0000 0A\n 1: 0100007F:9C40 0100007F:01BB 01\n")              // backend 19384, connected socket 40000
	write(filepath.Join(procNetDir, "tcp6"), "  sl local_address rem_address st\n 0: 00000000000000000000000000000000:F262 00000000000000000000000000000000:0000 0A\n") // panel 62050
	calls := new([]string)
	capacitySysctl = func(key, value string) error {
		*calls = append(*calls, key+"="+value)
		switch key {
		case "net.ipv4.ip_local_port_range":
			s.Range = value
			write(filepath.Join(procIPv4Dir, "ip_local_port_range"), value)
		case "net.ipv4.ip_local_reserved_ports":
			s.Reserved = value
			write(filepath.Join(procIPv4Dir, "ip_local_reserved_ports"), value)
		default:
			t.Fatalf("unexpected kernel mutation %s", key)
		}
		return nil
	}
	return s, calls
}

func TestHighConcurrencyPreservesServicesAndOriginalRollback(t *testing.T) {
	s, calls := capacityFixture(t)
	reserved, err := enableHighConcurrency([]int{443, 20000, 20001})
	if err != nil {
		t.Fatal(err)
	}
	if reserved != "443,9000,19384,20000-20001,62050-62051" {
		t.Fatal(reserved)
	}
	if s.Range != HighConcurrencyRange {
		t.Fatal(s.Range)
	}
	if !strings.HasPrefix((*calls)[0], "net.ipv4.ip_local_reserved_ports=") {
		t.Fatal("range widened before services were reserved")
	}
	if strings.Contains(reserved, "40000") {
		t.Fatal("transient outbound socket was permanently reserved")
	}
	first, err := os.ReadFile(capacityBackup)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = enableHighConcurrency([]int{21000}); err != nil {
		t.Fatal(err)
	}
	again, _ := os.ReadFile(capacityBackup)
	if string(first) != string(again) {
		t.Fatal("second application lost original rollback")
	}
	config, err := os.ReadFile(capacityFile)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(config), "21000") || !HighConcurrencyEnabled() {
		t.Fatal("new reservation not persisted")
	}
	if err = restoreHighConcurrency(); err != nil {
		t.Fatal(err)
	}
	if s.Range != "32768 60999" || s.Reserved != "9000,62050-62051" {
		t.Fatalf("restore: %+v", s)
	}
	if HighConcurrencyEnabled() {
		t.Fatal("override survived restore")
	}
}

func TestHighConcurrencyRollsBackRejectedRange(t *testing.T) {
	s, _ := capacityFixture(t)
	set := capacitySysctl
	capacitySysctl = func(key, value string) error {
		if key == "net.ipv4.ip_local_port_range" && value == HighConcurrencyRange {
			return errors.New("kernel refused")
		}
		return set(key, value)
	}
	_, err := enableHighConcurrency([]int{443})
	if err == nil || !strings.Contains(err.Error(), "kernel refused") {
		t.Fatal(err)
	}
	if s.Range != "32768 60999" || s.Reserved != "9000,62050-62051" {
		t.Fatalf("rollback: %+v", s)
	}
	if HighConcurrencyEnabled() {
		t.Fatal("failed application persisted")
	}
	if _, err := os.Stat(capacityBackup); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("rollback snapshot not cleaned up after complete rollback")
	}
}

func TestHighConcurrencyFailsBeforeMutationsWhenDiscoveryFails(t *testing.T) {
	for _, broken := range []string{"reserved", "listeners", "unmanaged"} {
		t.Run(broken, func(t *testing.T) {
			_, calls := capacityFixture(t)
			switch broken {
			case "reserved":
				os.WriteFile(filepath.Join(procIPv4Dir, "ip_local_reserved_ports"), []byte("62050-bad"), 0600)
			case "listeners":
				os.Remove(filepath.Join(procNetDir, "tcp"))
			case "unmanaged":
				os.MkdirAll(filepath.Dir(capacityFile), 0755)
				os.WriteFile(capacityFile, []byte("# somebody else's settings"), 0644)
			}
			if _, err := enableHighConcurrency(nil); err == nil {
				t.Fatal("unsafe discovery accepted")
			}
			if len(*calls) > 0 {
				t.Fatal("kernel changed after failed discovery")
			}
		})
	}
}

func TestHighConcurrencyRetainsBackupWhenRollbackFails(t *testing.T) {
	_, _ = capacityFixture(t)
	capacitySysctl = func(key, value string) error { return errors.New("refused") }
	if _, err := enableHighConcurrency(nil); err == nil {
		t.Fatal("expected failure")
	}
	if _, err := os.Stat(capacityBackup); err != nil {
		t.Fatal("recovery information lost", err)
	}
}

func TestReservedPortSyntaxIsStrict(t *testing.T) {
	got, err := ParseReservedPorts("443,62050-62051,443")
	if err != nil || !reflect.DeepEqual(got, []int{443, 62050, 62051, 443}) {
		t.Fatal(got, err)
	}
	for _, bad := range []string{"0", "65536", "62051-62050", "1-2-3", "1,,2", "abc", "1-65536"} {
		if _, err := ParseReservedPorts(bad); err == nil {
			t.Errorf("accepted %q", bad)
		}
	}
}

func TestHighConcurrencySurvivesOptimizeWithoutBreakingBootRollback(t *testing.T) {
	_, _ = capacityFixture(t)
	if _, err := enableHighConcurrency([]int{443}); err != nil {
		t.Fatal(err)
	}
	live, persisted, err := capacityTuningRows([]int{25000})
	if err != nil {
		t.Fatal(err)
	}
	values := func(rows [][2]string) map[string]string {
		out := map[string]string{}
		for _, r := range rows {
			out[r[0]] = r[1]
		}
		return out
	}
	l, p := values(live), values(persisted)
	if l["net.ipv4.ip_local_port_range"] != HighConcurrencyRange || p["net.ipv4.ip_local_port_range"] != "32768 60999" {
		t.Fatal(l, p)
	}
	if !strings.Contains(l["net.ipv4.ip_local_reserved_ports"], "25000") || p["net.ipv4.ip_local_reserved_ports"] != "9000,62050-62051" {
		t.Fatal(l, p)
	}
	b, _ := os.ReadFile(capacityFile)
	if !strings.Contains(string(b), "25000") {
		t.Fatal("new service port lost on boot")
	}
	if err := restoreHighConcurrency(); err != nil {
		t.Fatal(err)
	}
	live, _, err = capacityTuningRows([]int{443})
	if err != nil || values(live)["net.ipv4.ip_local_port_range"] != "32768 60999" {
		t.Fatal(live, err)
	}
}

func TestHighConcurrencyOptimizeKeepsPolicyWhenDiscoveryFails(t *testing.T) {
	_, _ = capacityFixture(t)
	if _, err := enableHighConcurrency(nil); err != nil {
		t.Fatal(err)
	}
	os.Remove(filepath.Join(procNetDir, "tcp"))
	live, persisted, err := capacityTuningRows([]int{443})
	if err == nil {
		t.Fatal("missing listener table ignored")
	}
	for _, rows := range [][][2]string{live, persisted} {
		for _, r := range rows {
			if strings.Contains(r[0], "ip_local_") {
				t.Fatal("failed discovery still changed policy", r)
			}
		}
	}
}

func TestHighConcurrencyRollsBackPersistenceFailure(t *testing.T) {
	s, _ := capacityFixture(t)
	set := capacitySysctl
	capacitySysctl = func(key, value string) error {
		if err := set(key, value); err != nil {
			return err
		}
		if key == "net.ipv4.ip_local_port_range" && value == HighConcurrencyRange {
			if err := os.MkdirAll(capacityFile, 0755); err != nil {
				t.Fatal(err)
			}
		}
		return nil
	}
	if _, err := enableHighConcurrency(nil); err == nil {
		t.Fatal("expected persistence failure")
	}
	if s.Range != "32768 60999" || s.Reserved != "9000,62050-62051" {
		t.Fatal("live changes survived failed persistence", s)
	}
}
