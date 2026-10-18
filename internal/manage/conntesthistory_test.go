package manage

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestConnectionTestHistoryStoresMeasurementsWithoutPrivateDiagnostics(t *testing.T) {
	previous := ConnTestHistoryRoot
	ConnTestHistoryRoot = t.TempDir()
	defer func() { ConnTestHistoryRoot = previous }()
	ctSaveHistory("iran", "turbo", []ConnTestPath{{Host: "192.0.2.1", Family: "IPv4", State: "done", Error: "private-path-error", Rows: []ConnTestResult{{Kind: "reverse", Transport: "tcp", Status: ctOK, Tried: 10, OK: 10, RTTms: 10, JitterMs: 1, Mbps: 100, UploadMbps: 50, Detail: "secret-token-in-diagnostic", HTTPSProbe: "private-probe"}}}}, true)
	runs, err := LoadConnTestHistory()
	if err != nil || len(runs) != 1 {
		t.Fatalf("history: %v %v", runs, err)
	}
	files, _ := os.ReadDir(ConnTestHistoryRoot)
	body, _ := os.ReadFile(filepath.Join(ConnTestHistoryRoot, files[0].Name()))
	for _, secret := range []string{"private-path-error", "secret-token-in-diagnostic", "private-probe"} {
		if strings.Contains(string(body), secret) {
			t.Fatal("private diagnostic persisted")
		}
	}
	table := ConnTestHistoryTable(runs, true)
	for _, want := range []string{"steady 1/1", "RTT 10.00", "jitter 1.00", "down 100.00 / up 50.00", "not an uptime percentage"} {
		if !strings.Contains(table, want) {
			t.Fatalf("missing %s: %s", want, table)
		}
	}
}
