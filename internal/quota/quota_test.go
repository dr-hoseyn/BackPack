package quota

import (
	"os"
	"testing"
)

func TestAQuotaSurvivesAndClears(t *testing.T) {
	dir := t.TempDir()
	if q, err := Load(dir, "t1"); err != nil || q.Limit != 0 {
		t.Fatalf("no file should be no limit: %+v %v", q, err)
	}
	if err := Save(dir, "t1", 5<<40); err != nil {
		t.Fatal(err)
	}
	q, err := Load(dir, "t1")
	if err != nil || q.Limit != 5<<40 || q.Set.IsZero() {
		t.Fatalf("read back %+v %v", q, err)
	}
	if q.Reached(5<<40-1) || !q.Reached(5<<40) {
		t.Error("the limit is reached at the limit, not a byte before")
	}
	if err := Save(dir, "t1", 0); err != nil {
		t.Fatal(err)
	}
	if q, _ := Load(dir, "t1"); q.Limit != 0 || q.Reached(1<<50) {
		t.Error("zero is no limit")
	}
}

func TestQuotaSaveIgnoresAnAbandonedTemporaryPath(t *testing.T) {
	dir := t.TempDir()
	if err := os.Mkdir(Path(dir, "t1")+".tmp", 0700); err != nil {
		t.Fatal(err)
	}
	if err := Save(dir, "t1", 1024); err != nil {
		t.Fatal(err)
	}
	if q, err := Load(dir, "t1"); err != nil || q.Limit != 1024 {
		t.Fatalf("loaded quota = %+v, error %v", q, err)
	}
}

func TestQuotaWithoutAnExplicitLimitIsRejected(t *testing.T) {
	dir := t.TempDir()
	for _, body := range []string{`null`, `{}`, `{"limit":null}`} {
		if err := os.WriteFile(Path(dir, "t1"), []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := Load(dir, "t1"); err == nil {
			t.Errorf("%s was treated as an unlimited quota", body)
		}
	}
	if err := os.WriteFile(Path(dir, "t1"), []byte(`{"limit":0}`), 0600); err != nil {
		t.Fatal(err)
	}
	if q, err := Load(dir, "t1"); err != nil || q.Limit != 0 {
		t.Fatalf("explicit unlimited quota = %+v, error %v", q, err)
	}
}
