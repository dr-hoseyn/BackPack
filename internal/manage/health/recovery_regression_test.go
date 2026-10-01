package health

import (
	"encoding/json"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/backpack/backpack/internal/app"
	"github.com/backpack/backpack/internal/manage/core"
	"github.com/backpack/backpack/internal/metrics"
)

func TestRecoveryRegressionDirectSnapshotOverridesSocketTable(t *testing.T) {
	name := fmt.Sprintf("recovery-regression-%d", time.Now().UnixNano())
	path := metrics.Path(app.ConfigDir, name)
	t.Cleanup(func() { os.Remove(path) })
	if err := os.MkdirAll(app.ConfigDir, 0755); err != nil {
		t.Fatal(err)
	}
	for _, role := range []string{"iran", "kharej"} {
		for _, state := range []bool{false, true} {
			data, err := json.Marshal(metrics.Snapshot{Name: name, Taken: time.Now(), Connected: &state})
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, data, 0600); err != nil {
				t.Fatal(err)
			}
			tunnel := core.Tunnel{Name: name, Role: role, Transport: "direct/tcp", Addr: "203.0.113.9:9000"}
			var pairs [][2]string
			if !state {
				pairs = [][2]string{{"198.51.100.1:1234", "203.0.113.9:9000"}, {"203.0.113.9:9000", "198.51.100.1:1234"}}
			}
			if got, known := directHealthy(tunnel, pairs); !known || got != state {
				t.Errorf("role=%s snapshot=%v: got healthy=%v known=%v", role, state, got, known)
			}
		}
	}
}
