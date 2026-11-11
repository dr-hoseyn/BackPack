package quota

import "testing"

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
