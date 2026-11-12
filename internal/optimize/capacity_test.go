package optimize

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"testing/iotest"
)

type oneShotSysctl struct {
	value    string
	consumed bool
}

func (r *oneShotSysctl) Read(p []byte) (int, error) {
	if r.consumed {
		return 0, io.EOF
	}
	r.consumed = true
	return copy(p, r.value), nil
}

func TestCapacityReadsLongOneShotSysctl(t *testing.T) {
	value := strings.Repeat("12345,", 200) + "62050\n"
	got, err := readCapacityValue(&oneShotSysctl{value: value})
	if err != nil || got != strings.TrimSpace(value) {
		t.Fatalf("reservation list truncated: got %d bytes, error %v", len(got), err)
	}
	if _, err := ParseReservedPorts(got); err != nil {
		t.Fatal(err)
	}
}

func TestCapacityRejectsOversizedSysctl(t *testing.T) {
	if _, err := readCapacityValue(strings.NewReader(strings.Repeat("1", 1<<20))); err == nil {
		t.Fatal("accepted a possibly truncated sysctl value")
	}
}

func TestCapacityCollectsShortReads(t *testing.T) {
	value := strings.Repeat("12345,", 200) + "62050\n"
	got, err := readCapacityValue(iotest.OneByteReader(strings.NewReader(value)))
	if err != nil || got != strings.TrimSpace(value) {
		t.Fatalf("short reads lost reservations: got %d bytes, error %v", len(got), err)
	}
}

func TestCapacityPropagatesReadFailure(t *testing.T) {
	want := errors.New("sysctl read failed")
	if _, err := readCapacityValue(iotest.ErrReader(want)); !errors.Is(err, want) {
		t.Fatalf("read error = %v, want %v", err, want)
	}
}

func capacityFixture(t *testing.T) (*capacityState, *[]string) {
	t.Helper()
	oldFile, oldBackup, oldNet, oldIPv4, oldCore, oldSet, oldLock := capacityFile, capacityBackup, procNetDir, procIPv4Dir, procCoreDir, capacitySysctl, capacityLockFile
	t.Cleanup(func() {
		capacityFile, capacityBackup, procNetDir, procIPv4Dir, capacitySysctl, capacityLockFile = oldFile, oldBackup, oldNet, oldIPv4, oldSet, oldLock
		procCoreDir = oldCore
	})
	dir := t.TempDir()
	capacityLockFile = filepath.Join(dir, "kernel-policy.lock")
	capacityFile, capacityBackup = filepath.Join(dir, "sysctl.d", "capacity.conf"), filepath.Join(dir, "config", "backup.json")
	procNetDir, procIPv4Dir = filepath.Join(dir, "net"), filepath.Join(dir, "ipv4")
	procCoreDir = filepath.Join(dir, "core")
	for _, d := range []string{procNetDir, procIPv4Dir, procCoreDir} {
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
		case "net.core.busy_read", "net.core.busy_poll":
			write(filepath.Join(procCoreDir, strings.TrimPrefix(key, "net.core.")), value)
		default:
			t.Fatalf("unexpected kernel mutation %s", key)
		}
		return nil
	}
	return s, calls
}

func TestCapacityDisablesBusyPollingAndRestoresTheOriginalCPUTradeoff(t *testing.T) {
	_, _ = capacityFixture(t)
	for _, key := range []string{"busy_read", "busy_poll"} {
		if err := os.WriteFile(filepath.Join(procCoreDir, key), []byte("50"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 2; i++ {
		if _, err := enableHighConcurrency(nil); err != nil {
			t.Fatal(err)
		}
	}
	current, err := currentCapacity()
	if err != nil || *current.BusyRead != 0 || *current.BusyPoll != 0 {
		t.Fatal(current, err)
	}
	live, _, err := capacityTuningRows(nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range live {
		if row[0] == "net.core.busy_poll" || row[0] == "net.core.busy_read" {
			t.Fatal("the capacity transaction already applied", row)
		}
	}
	profile, _ := os.ReadFile(capacityFile)
	if !strings.Contains(string(profile), "net.core.busy_read = 0") || !strings.Contains(string(profile), "net.core.busy_poll = 0") {
		t.Fatal(string(profile))
	}
	if err := restoreHighConcurrency(); err != nil {
		t.Fatal(err)
	}
	current, err = currentCapacity()
	if err != nil || *current.BusyRead != 50 || *current.BusyPoll != 50 {
		t.Fatal(current, err)
	}
}

func TestOldCapacityBackupsGainCPURollbackBeforeNewSettingsChange(t *testing.T) {
	_, _ = capacityFixture(t)
	if _, err := enableHighConcurrency(nil); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(procCoreDir, "busy_read"), []byte("75"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := enableHighConcurrency(nil); err != nil {
		t.Fatal(err)
	}
	if err := restoreHighConcurrency(); err != nil {
		t.Fatal(err)
	}
	s, err := currentCapacity()
	if err != nil || s.BusyRead == nil || *s.BusyRead != 75 || s.BusyPoll != nil {
		t.Fatal(s, err)
	}
}

func TestFailedCPUTuningRestoresPortsAndDoesNotPublishAnActiveProfile(t *testing.T) {
	s, _ := capacityFixture(t)
	for _, key := range []string{"busy_read", "busy_poll"} {
		if err := os.WriteFile(filepath.Join(procCoreDir, key), []byte("50"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	set := capacitySysctl
	capacitySysctl = func(key, value string) error {
		if key == "net.core.busy_poll" && value == "0" {
			return errors.New("busy poll refused")
		}
		return set(key, value)
	}
	if _, err := enableHighConcurrency(nil); err == nil {
		t.Fatal("expected failure")
	}
	current, err := currentCapacity()
	if err != nil || *current.BusyRead != 50 || *current.BusyPoll != 50 || s.Range != "32768 60999" || HighConcurrencyEnabled() {
		t.Fatal(current, err)
	}
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
	state, _ := capacityFixture(t)
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
	if state.Range != HighConcurrencyRange || l["net.ipv4.ip_local_port_range"] != "" || p["net.ipv4.ip_local_port_range"] != "32768 60999" {
		t.Fatal(l, p)
	}
	if !strings.Contains(state.Reserved, "25000") || l["net.ipv4.ip_local_reserved_ports"] != "" || p["net.ipv4.ip_local_reserved_ports"] != "9000,62050-62051" {
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

func TestCapacityReclaimsOnlyOwnedListenerReservations(t *testing.T) {
	state, _ := capacityFixture(t)
	if _, err := enableHighConcurrency([]int{41000}); err != nil {
		t.Fatal(err)
	}
	boot, err := os.ReadFile(capacityFile)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(boot), capacityAutoPrefix+"19384\n") ||
		!strings.Contains(string(boot), capacityReservedPrefix+"9000,41000,62050-62051\n") {
		t.Fatalf("boot and live ownership were mixed: %s", boot)
	}
	// Simulate an operator adding a manual reservation while the profile is
	// active and the temporary listener exiting.
	if err := capacitySysctl("net.ipv4.ip_local_reserved_ports", state.Reserved+",42000"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(procNetDir, "tcp"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := capacityTuningRows(nil); err != nil {
		t.Fatal(err)
	}
	if state.Reserved != "9000,41000,42000,62050-62051" {
		t.Fatal(state.Reserved)
	}
	// A stopped listener can come back and be discovered again.
	if err := os.WriteFile(filepath.Join(procNetDir, "tcp"), []byte("0: 0100007F:4BB8 00000000:0000 0A\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := enableHighConcurrency(nil); err != nil {
		t.Fatal(err)
	}
	if state.Reserved != "9000,19384,41000,42000,62050-62051" {
		t.Fatal(state.Reserved)
	}
}

func TestCapacityRetainsLegacyReservationsWithUnknownOwnership(t *testing.T) {
	state, _ := capacityFixture(t)
	if _, err := enableHighConcurrency(nil); err != nil {
		t.Fatal(err)
	}
	legacy := capacityHeader + capacityReservedPrefix + state.Reserved + ",41000\n"
	if err := os.WriteFile(capacityFile, []byte(legacy), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(procNetDir, "tcp"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := enableHighConcurrency(nil); err != nil {
		t.Fatal(err)
	}
	if state.Reserved != "9000,19384,41000,62050-62051" {
		t.Fatal(state.Reserved)
	}
}

func TestCapacityRefreshFailureKeepsLivePortsAndOwnership(t *testing.T) {
	state, _ := capacityFixture(t)
	if _, err := enableHighConcurrency(nil); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(capacityFile)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(procNetDir, "tcp"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	set := capacitySysctl
	capacitySysctl = func(key, value string) error {
		if key == "net.ipv4.ip_local_port_range" && state.Reserved == "9000,62050-62051" {
			capacitySysctl = set // fail only the refresh, not the rollback
			return errors.New("cannot apply range")
		}
		return set(key, value)
	}
	if _, _, err := capacityTuningRows(nil); err == nil {
		t.Fatal("failed refresh reported success")
	}
	after, err := os.ReadFile(capacityFile)
	if err != nil || string(after) != string(before) || state.Reserved != "9000,19384,62050-62051" {
		t.Fatalf("failed refresh changed ownership or live ports: %q, %s, %v", after, state.Reserved, err)
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
