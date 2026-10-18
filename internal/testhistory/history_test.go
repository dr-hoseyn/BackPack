package testhistory

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func sample() Run {
	return Run{Version: 1, At: time.Now().UTC(), Side: "iran", Preset: "turbo", Complete: true, Paths: []Path{{Host: "192.0.2.1", Peer: "198.51.100.1", Family: "IPv4", State: "done", Rows: []Measurement{{Kind: "reverse", Transport: "tcp", Status: "ok", Sent: 10, Received: 10, RTTms: 20, JitterMs: 2, Download: 100, Upload: 50}}}}}
}
func TestConcurrentRunsRemainDistinctAndHistoryRetentionIsBounded(t *testing.T) {
	store := Store{Dir: filepath.Join(t.TempDir(), "history")}
	var wg sync.WaitGroup
	for range 12 {
		wg.Go(func() {
			if err := store.Save(sample()); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	runs, err := store.Load()
	if err != nil || len(runs) != 12 {
		t.Fatalf("overlapping saves lost samples: %d %v", len(runs), err)
	}
	for i := 0; i < Limit; i++ {
		r := sample()
		r.At = r.At.Add(time.Duration(i+1) * time.Second)
		if err := store.Save(r); err != nil {
			t.Fatal(err)
		}
	}
	runs, err = store.Load()
	if err != nil || len(runs) != Limit {
		t.Fatalf("retention: %d %v", len(runs), err)
	}
	for i := 1; i < len(runs); i++ {
		if runs[i].At.After(runs[i-1].At) {
			t.Fatal("not newest first")
		}
	}
	files, err := os.ReadDir(store.Dir)
	if err != nil || len(files) != Limit {
		t.Fatal("temporary files or old runs leaked")
	}
}
func TestCorruptOrOversizedSamplesDoNotHideValidRuns(t *testing.T) {
	store := Store{Dir: t.TempDir()}
	if err := store.Save(sample()); err != nil {
		t.Fatal(err)
	}
	name := "20990101T000000.000000000Z-0123456789abcdef.json"
	for _, body := range [][]byte{[]byte("{"), make([]byte, maxFile+1)} {
		if err := os.WriteFile(filepath.Join(store.Dir, name), body, 0600); err != nil {
			t.Fatal(err)
		}
		runs, err := store.Load()
		if len(runs) != 1 || err == nil {
			t.Fatalf("corruption hidden or valid sample lost: %d %v", len(runs), err)
		}
	}
	r := sample()
	r.Paths[0].Rows[0].SNI = "bad\nterminal-control"
	if err := store.Save(r); err == nil {
		t.Fatal("control characters accepted")
	}
	r = sample()
	r.Paths[0].Rows[0].Received = 11
	if err := store.Save(r); err == nil {
		t.Fatal("impossible echo counts accepted")
	}
	r = sample()
	r.Paths[0].Family = "IPv6"
	if err := store.Save(r); err == nil {
		t.Fatal("IP families mixed")
	}
}
func TestComparisonKeepsPathsPresetsPeersAndSNICandidatesSeparate(t *testing.T) {
	a := sample()
	b := sample()
	b.Paths[0].Rows[0].Status = "unstable"
	b.Paths[0].Rows[0].Sent = 30
	b.Paths[0].Rows[0].Received = 15
	b.Paths[0].Rows[0].RTTms = 40
	partial := sample()
	partial.Complete = false
	separate := sample()
	separate.Preset = "balance"
	otherPeer := sample()
	otherPeer.Paths[0].Peer = "198.51.100.2"
	sni := sample()
	sni.Paths[0].Rows[0].SNI = "one.example"
	sni2 := sample()
	sni2.Paths[0].Rows[0].SNI = "two.example"
	out := Compare([]Run{a, b, partial, separate, otherPeer, sni, sni2})
	if len(out) != 5 {
		t.Fatalf("mixed independent cases: %+v", out)
	}
	var original Summary
	for _, s := range out {
		if s.Preset == "turbo" && s.Peer == "198.51.100.1" && s.SNI == "" {
			original = s
		}
	}
	if original.Runs != 2 || original.Steady != 1 || original.Loss != 37.5 || original.RTTms != 32 || original.Download != 100 || original.SpeedSamples != 1 {
		t.Fatalf("incorrect weighted measurements: %+v", original)
	}
	for i, n := range []float64{100, 20, 60} {
		r := sample()
		r.Paths[0].Rows[0].Download = n
		if i == 0 {
			a = r
		} else if i == 1 {
			b = r
		} else {
			partial = r
		}
	}
	out = Compare([]Run{a, b, partial})
	if len(out) != 1 || out[0].Download != 60 {
		t.Fatal("throughput was not the median")
	}
}
func TestEmptyStoreAndCredentialFreeSchema(t *testing.T) {
	store := Store{Dir: filepath.Join(t.TempDir(), "missing")}
	runs, err := store.Load()
	if err != nil || len(runs) != 0 {
		t.Fatal(err)
	}
	if err := store.Save(sample()); err != nil {
		t.Fatal(err)
	}
	names, _ := store.files()
	body, err := os.ReadFile(filepath.Join(store.Dir, names[0]))
	if err != nil {
		t.Fatal(err)
	}
	var schema map[string]json.RawMessage
	if err := json.Unmarshal(body, &schema); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"token", "password", "link", "error", "detail", "config"} {
		if strings.Contains(string(body), fmt.Sprintf("%q:", name)) {
			t.Fatalf("unexpected sensitive field %s", name)
		}
	}
}
