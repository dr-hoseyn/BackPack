package amneziawg

import (
	"encoding/json"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestManagedTelemetryRequiresARecentHandshakeAndNeverCarriesPrivateIPC(t *testing.T) {
	if (RuntimeStats{}).Connected() {
		t.Fatal("interface readiness counted as connectivity")
	}
	raw := "private_key=never-export-this\npublic_key=not-a-metric\npreshared_key=also-private\nrx_bytes=1234\ntx_bytes=5678\nendpoint=203.0.113.1:25999\nlast_handshake_time_sec="
	s := runtimeStats(raw + time.Now().Format("not-a-number"))
	if s.Connected() {
		t.Fatal("malformed handshake accepted")
	}
	s = runtimeStats(raw + strconv.FormatInt(time.Now().Unix(), 10))
	if !s.Connected() || s.BytesIn != 1234 || s.BytesOut != 5678 || s.Endpoint != "203.0.113.1:25999" {
		t.Fatal("valid public runtime state was lost")
	}
	b, _ := json.Marshal(s)
	if strings.Contains(string(b), "private") || strings.Contains(string(b), "not-a-metric") {
		t.Fatal("IPC secret reached telemetry")
	}
	s.LastHandshake = time.Now().Add(-4 * time.Minute)
	if s.Connected() {
		t.Fatal("stale handshake counted as current")
	}
}
