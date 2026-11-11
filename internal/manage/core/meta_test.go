package core

import (
	"os"
	"path/filepath"
	"testing"
)

func TestMetadataRemainsWritableAfterDamagedOrNullState(t *testing.T) {
	for _, body := range []string{`null`, `{"tunnel":`} {
		dir := t.TempDir()
		path := filepath.Join(dir, "meta.json")
		if err := os.WriteFile(path, []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
		m := loadMetaFile(path)
		m["new"] = tunnelMeta{LinkHosts: []string{"192.0.2.1"}}
		if got := m["new"].LinkHosts; len(got) != 1 || got[0] != "192.0.2.1" {
			t.Fatalf("backup addresses were not recorded after loading %s", body)
		}
		if body != "null" {
			copies, err := filepath.Glob(path + ".unreadable-*")
			if err != nil || len(copies) != 1 {
				t.Fatalf("damaged state recovery copies = %v, error %v", copies, err)
			}
			saved, err := os.ReadFile(copies[0])
			if err != nil || string(saved) != body {
				t.Fatalf("damaged metadata was not preserved: %q, error %v", saved, err)
			}
		}
	}
}
