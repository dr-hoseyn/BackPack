package traffic

import (
	"context"
	"github.com/sirupsen/logrus"
	"os"
	"path/filepath"
	"testing"
)

func TestLegacySnifferSettingsDoNotWriteUsageFiles(t *testing.T) {
	path := filepath.Join(t.TempDir(), "usage.json")
	u := NewDataStore(context.Background(), path, true, logrus.New())
	u.AddOrUpdatePort(443, 1024)
	u.Run()
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("legacy sniffer created a usage file: %v", err)
	}
}
