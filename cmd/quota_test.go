package cmd

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"testing/synctest"
	"time"

	"github.com/backpack/backpack/internal/metrics"
	"github.com/backpack/backpack/internal/quota"
)

func freePort(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return strconv.Itoa(l.Addr().(*net.TCPAddr).Port)
}

func listening(port string) bool {
	c, err := net.DialTimeout("tcp", "127.0.0.1:"+port, 300*time.Millisecond)
	if err != nil {
		return false
	}
	c.Close()
	return true
}

func eventually(t *testing.T, what string, within time.Duration, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if ok() {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("%s did not happen within %s", what, within)
}

// A tunnel with a traffic limit goes offline the moment it has carried it, and
// stays offline until the limit is raised — with the service still running,
// so raising the limit is all it takes.
func TestATunnelStopsAtItsTrafficLimitAndComesBackWhenRaised(t *testing.T) {
	prevCheck, prevReread := quotaCheck.Load(), quotaReread.Load()
	quotaCheck.Store(int64(20 * time.Millisecond))
	quotaReread.Store(int64(100 * time.Millisecond))
	defer func() { quotaCheck.Store(prevCheck); quotaReread.Store(prevReread) }()

	dir := t.TempDir()
	path := filepath.Join(dir, "q1.toml")
	port := freePort(t)
	writeConfig(t, path, "\n[server]\nbind_addr = \"127.0.0.1:"+port+"\"\ntransport = \"tcp\"\ntoken = \"a-token\"\nskip_optz = true\nports = [\""+freePort(t)+"\"]\n")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stopped := make(chan struct{})
	go func() { Run(path, ctx); close(stopped) }()
	eventually(t, "the tunnel listening", 10*time.Second, func() bool { return listening(port) })

	if err := quota.Save(dir, "q1", 1000); err != nil {
		t.Fatal(err)
	}
	metrics.AddBytes(4000, 1000) // what the tunnel carried
	eventually(t, "the tunnel going offline at its limit", 10*time.Second, func() bool { return !listening(port) })

	// Still offline a while later: nothing restarts it behind the limit's back.
	time.Sleep(700 * time.Millisecond)
	if listening(port) {
		t.Fatal("a tunnel over its limit came back on its own")
	}
	if s, err := metrics.Read(dir, "q1"); err != nil || s.BytesIn+s.BytesOut < 1000 {
		t.Fatalf("what was used is not on disk for the next start to see: %+v %v", s, err)
	}

	if err := quota.Save(dir, "q1", 1<<30); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the tunnel coming back once the limit was raised", 10*time.Second, func() bool { return listening(port) })

	cancel()
	select {
	case <-stopped:
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not return on shutdown")
	}
}

func TestRunningTunnelStopsIfItsQuotaBecomesUnreadable(t *testing.T) {
	dir := t.TempDir()
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		stopped := make(chan struct{})
		go watchQuota(ctx, filepath.Join(dir, "q1.toml"), func() { close(stopped) })

		// The watcher has read the initial quota and is waiting on its ticks.
		synctest.Wait()
		if err := os.WriteFile(quota.Path(dir, "q1"), []byte(`{}`), 0600); err != nil {
			t.Fatal(err)
		}
		select {
		case <-stopped:
		case <-time.After(every(&quotaReread) + time.Second):
			t.Fatal("watcher left the tunnel online with an unreadable quota")
		}
	})
}

func TestQuotaStartupWaitsWhenPersistedTrafficCannotBeRead(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "q1.toml")
	if err := quota.Save(dir, "q1", 1000); err != nil {
		t.Fatal(err)
	}
	if quotaReached(configPath) {
		t.Fatal("a new tunnel without a snapshot should start")
	}
	snapshot := metrics.Path(dir, "q1")
	for _, body := range []string{`{"bytes_in":`, `null`, `{}`, `{"bytes_in":null,"bytes_out":0}`} {
		if err := os.WriteFile(snapshot, []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
		if !quotaReached(configPath) {
			t.Fatalf("the quota gate treated %s as zero traffic", body)
		}
	}
	if err := os.WriteFile(snapshot, []byte(`{"bytes_in":250,"bytes_out":100}`), 0600); err != nil {
		t.Fatal(err)
	}
	if quotaReached(configPath) {
		t.Fatal("the quota gate stayed closed after traffic was readable")
	}
}
