package schedule

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestConcurrentScheduleChangesPreserveAllJobs(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("BP_TEST_CRONTAB", filepath.Join(dir, "jobs"))
	fake := `#!/bin/sh
if [ "$1" = "-l" ]; then
  if [ -f "$BP_TEST_CRONTAB" ]; then
    cat "$BP_TEST_CRONTAB"
  else
    echo "no crontab for test" >&2
    exit 1
  fi
else
  cat > "$BP_TEST_CRONTAB.new"
  mv "$BP_TEST_CRONTAB.new" "$BP_TEST_CRONTAB"
fi
`
	if err := os.WriteFile(filepath.Join(dir, "crontab"), []byte(fake), 0700); err != nil {
		t.Fatal(err)
	}
	const count = 8
	start := make(chan struct{})
	results := make(chan error, count)
	for i := range count {
		go func() {
			<-start
			results <- SetCron(fmt.Sprintf("job-%d", i), "0 * * * *", fmt.Sprintf("command-%d", i))
		}()
	}
	close(start)
	for range count {
		if err := <-results; err != nil {
			t.Error(err)
		}
	}
	lines, err := readCrontab()
	if err != nil {
		t.Fatal(err)
	}
	if len(lines) != count {
		t.Fatalf("kept %d jobs, want %d: %v", len(lines), count, lines)
	}
	for i := range count {
		want := fmt.Sprintf("0 * * * * command-%d # job-%d", i, i)
		if !strings.Contains(strings.Join(lines, "\n"), want) {
			t.Errorf("missing scheduled job %q", want)
		}
	}
}
