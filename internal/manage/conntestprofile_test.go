package manage

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestQuickTestShortlistsOnlyCompleteTrafficEvidence(t *testing.T) {
	rows := []ConnTestResult{
		{Transport: "slow", Status: ctTesting, Phase: "speed-queued", Total: 30, Tried: 30, OK: 30, RTTms: 80},
		{Transport: "jittery", Status: ctTesting, Phase: "speed-queued", Total: 30, Tried: 30, OK: 30, RTTms: 5, JitterMs: 50},
		{Transport: "fast", Status: ctTesting, Phase: "speed-queued", Total: 30, Tried: 30, OK: 30, RTTms: 20},
		{Transport: "tie-first", Status: ctTesting, Phase: "speed-queued", Total: 30, Tried: 30, OK: 30, RTTms: 30},
		{Transport: "tie-last", Status: ctTesting, Phase: "speed-queued", Total: 30, Tried: 30, OK: 30, RTTms: 30},
		{Transport: "lossy", Status: ctUnstable, Total: 30, Tried: 30, OK: 29},
		{Transport: "cancelled", Status: ctUnstable, Total: 30, Tried: 1, OK: 1},
		{Transport: "udp", Status: ctOK, Total: 30, Tried: 30, OK: 30},
	}
	ctShortlist(rows, 3, func(i int, r ConnTestResult) { rows[i] = r })
	for _, i := range []int{0, 1} {
		if rows[i].Status != ctScreened || rows[i].Phase != "" || !strings.Contains(rows[i].Detail, "speed not measured") {
			t.Fatalf("non-finalist lost its evidence or claims speed: %+v", rows[i])
		}
	}
	for _, i := range []int{2, 3, 4} {
		if rows[i].Phase != "speed-queued" {
			t.Fatalf("finalist was removed: %+v", rows[i])
		}
	}
	if rows[5].Status != ctUnstable || rows[6].Status != ctUnstable || rows[7].Status != ctOK {
		t.Fatal("screening changed a failed or UDP result")
	}
}

func TestQuickTestProfilesStayPerRunAndTravelWithTheLink(t *testing.T) {
	quick, full := &connTestCase{mode: ConnTestQuick}, &connTestCase{}
	if quick.initialResult().Total != 30 || full.initialResult().Total != connTestSoak {
		t.Fatal("profiles share mutable sample counts")
	}
	if connectWaitFor(quick) != 20*time.Second || connectWaitFor(full) != connTestConnectWait {
		t.Fatal("wrong connection budgets")
	}
	for _, input := range []string{"", "full", " QUICK "} {
		if _, err := ConnTestMode(input); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := ConnTestMode("invalid"); err == nil {
		t.Fatal("invalid mode accepted")
	}
	l := ConnTestLink{Mode: ConnTestQuick, Host: "127.0.0.1"}
	raw, err := unGzipB64(ctConfig(l))
	if err != nil {
		t.Fatal(err)
	}
	var decoded ConnTestLink
	if err := json.Unmarshal(raw, &decoded); err != nil || decoded.Mode != ConnTestQuick {
		t.Fatalf("mode lost in config: %s (%v)", raw, err)
	}
	if ConnTestRunTimeFor(ConnTestQuick) >= ConnTestRunTimeFor(ConnTestFull) {
		t.Fatal("quick estimate does not reflect shortlist")
	}
}

// The first echo succeeds; the next payload is blackholed. A one-second
// observation must not turn into a four-second socket timeout or a pass.
func TestTrafficObservationHasAWallClockLimit(t *testing.T) {
	prior := connTestSoak
	connTestSoak = 1
	defer func() { connTestSoak = prior }()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	go func() {
		cn, err := l.Accept()
		if err != nil {
			return
		}
		defer cn.Close()
		buf := make([]byte, 512)
		if _, err := io.ReadFull(cn, buf); err != nil {
			return
		}
		if _, err := cn.Write(buf); err != nil {
			return
		}
		io.Copy(io.Discard, cn)
	}()
	port := l.Addr().(*net.TCPAddr).Port
	started := time.Now()
	r := ctProbe(context.Background(), &connTestCase{kind: "reverse", tr: "tcp", entry: port}, 0, nil)
	if elapsed := time.Since(started); elapsed > 2500*time.Millisecond || elapsed < 900*time.Millisecond {
		t.Fatalf("observation lasted %s", elapsed)
	}
	if r.Status == ctOK || r.Phase == "speed-queued" || r.OK != 0 {
		t.Fatalf("blackhole passed: %+v", r)
	}
}

func TestTrafficDiscoveryBoundsAHandshakeWithoutPayload(t *testing.T) {
	prior := connTestConnectWait
	connTestConnectWait = 150 * time.Millisecond
	defer func() { connTestConnectWait = prior }()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	go func() {
		cn, err := l.Accept()
		if err != nil {
			return
		}
		defer cn.Close()
		io.Copy(io.Discard, cn)
	}()
	started := time.Now()
	reported := ConnTestResult{}
	r := ctProbe(context.Background(), &connTestCase{kind: "reverse", tr: "tcp", entry: l.Addr().(*net.TCPAddr).Port}, 0, func(r ConnTestResult) { reported = r })
	if time.Since(started) > time.Second {
		t.Fatal("discovery exceeded its deadline waiting for payload")
	}
	if r.Status != ctDown || reported.Status != ctDown || r.OK != 0 {
		t.Fatalf("handshake-only path passed or left a stale live row: %+v / %+v", r, reported)
	}
}

func TestCancellationCannotPassEvenAfterTheLastEcho(t *testing.T) {
	prior := connTestSoak
	connTestSoak = 1
	defer func() { connTestSoak = prior }()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	go func() {
		cn, err := l.Accept()
		if err != nil {
			return
		}
		defer cn.Close()
		io.Copy(cn, cn)
	}()
	_, port, _ := net.SplitHostPort(l.Addr().String())
	n, _ := strconv.Atoi(port)
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	r := ctProbe(ctx, &connTestCase{kind: "reverse", tr: "tcp", entry: n}, 0, nil)
	if r.Status != ctUnstable || r.Phase != "" {
		t.Fatalf("partial observation passed: %+v", r)
	}
}

func TestScreenedRowsCannotBeRecommendedOrInstalled(t *testing.T) {
	r := ConnTestResult{Kind: "reverse", Transport: "tcp", Mode: ConnTestQuick, Status: ctScreened, OK: 30, Tried: 30, Total: 30}
	if got := newCTSetup(ConnTestLink{}, "", []ConnTestResult{r}).View().Choices; len(got) != 0 {
		t.Fatalf("unmeasured row can be installed: %+v", got)
	}
	if got := ctPurposeRecommendations([]ConnTestResult{r}, nil); len(got) != 0 {
		t.Fatalf("unmeasured row recommended: %+v", got)
	}
	if table := ConnTestTable([]ConnTestResult{r}); !strings.Contains(table, "QUICK TEST") || !strings.Contains(table, "SCREENED") {
		t.Fatalf("short evidence not labelled: %s", table)
	}
}

func TestQuickTestRunsThreeSerialVerifiedSpeedFinalists(t *testing.T) {
	if testing.Short() {
		t.Skip("observes the real 30-second quick window and transfers real speed payloads")
	}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	owners := &ctEchoes{}
	defer owners.close()
	defer l.Close()
	go ctServeTCPEcho(l, owners)
	coord, err := startCTCoordinator(ctPickPort(map[int]bool{}, true), "quick-test")
	if err != nil {
		t.Fatal(err)
	}
	defer coord.close()
	coord.pmtu <- 1500
	s := &ConnTestIran{dir: t.TempDir(), coord: coord, link: ConnTestLink{Mode: ConnTestQuick, Host: "127.0.0.1"}}
	for _, tr := range []string{"tcp", "tcpmux", "ws", "quic", "kcp"} {
		s.cases = append(s.cases, &connTestCase{mode: ConnTestQuick, kind: "reverse", tr: tr, entry: l.Addr().(*net.TCPAddr).Port})
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	var mu sync.Mutex
	active, speeds := -1, 0
	rows := s.Run(ctx, func(i int, r ConnTestResult) {
		mu.Lock()
		defer mu.Unlock()
		if r.Phase == "speed" {
			if active >= 0 {
				t.Errorf("concurrent speed measurements: %d and %d", active, i)
			}
			active = i
			speeds++
		} else if active == i {
			active = -1
		}
	})
	if err := ctx.Err(); err != nil {
		t.Fatal(err)
	}
	ok, screened := 0, 0
	for _, r := range rows {
		if r.Total != 30 || r.OK != 30 || r.Tried != 30 || r.Mode != ConnTestQuick {
			t.Fatalf("incomplete quick evidence: %+v", r)
		}
		switch r.Status {
		case ctOK:
			ok++
			if r.Mbps <= 0 || r.UploadMbps <= 0 {
				t.Fatalf("unverified speed: %+v", r)
			}
		case ctScreened:
			screened++
		default:
			t.Fatalf("unexpected verdict: %+v", r)
		}
	}
	if ok != 3 || screened != 2 || speeds != 3 {
		t.Fatalf("speed phase: %d passed / %d screened / %d measurements", ok, screened, speeds)
	}
}
