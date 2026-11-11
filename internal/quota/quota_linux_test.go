package quota

import (
	"os"
	"path/filepath"
	"testing"
)

func TestConcurrentQuotaSavesPublishACompleteLimit(t *testing.T) {
	dir := t.TempDir()
	const writers = 12
	start := make(chan struct{})
	results := make(chan error, writers)
	for i := 1; i <= writers; i++ {
		go func() {
			<-start
			results <- Save(dir, "t1", uint64(i))
		}()
	}
	close(start)
	for range writers {
		if err := <-results; err != nil {
			t.Error(err)
		}
	}
	if q, err := Load(dir, "t1"); err != nil || q.Limit == 0 || q.Limit > writers || q.Set.IsZero() {
		t.Fatalf("loaded quota = %+v, error %v", q, err)
	}
}

func TestAnUnavailableQuotaSymlinkTargetDoesNotLiftTheLimit(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "limit.json")
	if err := os.Symlink(target, Path(dir, "t1")); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(dir, "t1"); err == nil {
		t.Fatal("a dangling quota symlink was treated as unlimited")
	}
	if err := os.WriteFile(target, []byte(`{"limit":1024}`), 0600); err != nil {
		t.Fatal(err)
	}
	if got, err := Load(dir, "t1"); err != nil || got.Limit != 1024 {
		t.Fatalf("recovered quota = %+v, error %v", got, err)
	}
}
