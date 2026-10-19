package updatehealth

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestPolicyRefusesUnsafeOrAmbiguousConfiguration(t *testing.T) {
	good := Check{Tunnel: "edge", URL: "http://127.0.0.1:19080/health", ExpectedBody: "healthy\n"}
	for _, raw := range []string{
		"http://example.com:80/health", "http://localhost:80/health", "http://192.0.2.1:80/health",
		"http://127.0.0.1/health", "http://127.0.0.1:0/health", "http://127.0.0.1:99999/health",
		"http://user:secret@127.0.0.1:80/health", "http://127.0.0.1:80/health#fragment",
		"file:///etc/passwd", "://bad", "http:opaque",
	} {
		c := good
		c.URL = raw
		if err := (Policy{Checks: []Check{c}}).Validate(); err == nil {
			t.Errorf("accepted unsafe endpoint %q", raw)
		}
	}
	for _, p := range []Policy{
		{GraceSeconds: -1}, {GraceSeconds: 121},
		{Checks: []Check{good, good}}, {Checks: []Check{{URL: good.URL, ExpectedBody: "ok"}}},
		{Checks: []Check{{Tunnel: "edge", URL: good.URL}}},
		{Checks: []Check{{Tunnel: "edge", URL: good.URL, ExpectedBody: strings.Repeat("x", maxBody+1)}}},
		{Checks: make([]Check, 33)},
	} {
		if p.Validate() == nil {
			t.Errorf("accepted invalid policy")
		}
	}
	good.URL = "http://[::1]:19080/health"
	if err := (Policy{Checks: []Check{good}}).Validate(); err != nil {
		t.Fatal(err)
	}
	if (Policy{}).Grace() != 45*time.Second || (Policy{GraceSeconds: 10}).Grace() != 10*time.Second {
		t.Fatal("incorrect grace period")
	}
}

func TestPolicyLoadFailsClosedAndMissingPolicyIsOptional(t *testing.T) {
	path := filepath.Join(t.TempDir(), "update-health.json")
	if p, err := Load(path); err != nil || len(p.Checks) != 0 {
		t.Fatalf("absent policy: %+v, %v", p, err)
	}
	for _, body := range []string{
		"", "{", `{"check":[]}`, `{} {}`, `{} trailing`, `{"grace_seconds":999}`,
		strings.Repeat(" ", (256<<10)+1),
	} {
		if err := os.WriteFile(path, []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := Load(path); err == nil {
			t.Fatal("invalid policy was silently ignored")
		}
	}
	p := Policy{GraceSeconds: 10, Checks: []Check{{Tunnel: "edge", URL: "http://127.0.0.1:80/health", ExpectedBody: "ok\n"}}}
	b, _ := json.Marshal(p)
	if err := os.WriteFile(path, b, 0600); err != nil {
		t.Fatal(err)
	}
	loaded, err := Load(path)
	if err != nil || loaded.Checks[0] != p.Checks[0] {
		t.Fatalf("round trip: %+v %v", loaded, err)
	}
	if _, err := Load(filepath.Dir(path)); err == nil {
		t.Fatal("unreadable policy treated as absent")
	}
}

func TestPayloadProbeRejectsWrongPartialOversizedRedirectAndUntrustedTLS(t *testing.T) {
	for _, test := range []struct {
		name string
		h    http.HandlerFunc
	}{
		{"wrong", func(w http.ResponseWriter, _ *http.Request) { io.WriteString(w, "wrong") }},
		{"extra", func(w http.ResponseWriter, _ *http.Request) { io.WriteString(w, "healthy\nextra") }},
		{"short", func(w http.ResponseWriter, _ *http.Request) { io.WriteString(w, "health") }},
		{"truncated", func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Length", "100")
			io.WriteString(w, "healthy\n")
		}},
		{"unavailable", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusServiceUnavailable) }},
		{"redirect", func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, "/ok", http.StatusFound) }},
	} {
		t.Run(test.name, func(t *testing.T) {
			s := httptest.NewServer(test.h)
			defer s.Close()
			err := Probe(context.Background(), Check{Tunnel: "edge", URL: s.URL, ExpectedBody: "healthy\n"})
			if err == nil || strings.Contains(err.Error(), s.URL) {
				t.Fatalf("invalid response accepted or endpoint leaked: %v", err)
			}
		})
	}
	s := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { io.WriteString(w, "healthy\n") }))
	defer s.Close()
	if Probe(context.Background(), Check{URL: s.URL, ExpectedBody: "healthy\n"}) == nil {
		t.Fatal("accepted an untrusted TLS certificate")
	}
}

func TestPayloadProbeUsesFreshNoncesAndConnectionsWithoutAnEnvironmentProxy(t *testing.T) {
	var mu sync.Mutex
	nonces, connections := map[string]bool{}, map[string]bool{}
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		nonce := r.URL.Query().Get("_backpack_health")
		if len(nonce) != 32 || nonces[nonce] || connections[r.RemoteAddr] || r.Header.Get("Cache-Control") != "no-store, no-cache" {
			t.Error("probe reused a nonce/connection or omitted cache control")
		}
		nonces[nonce], connections[r.RemoteAddr] = true, true
		io.WriteString(w, "healthy\n")
	}))
	defer s.Close()
	proxy := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Error("used environment proxy") }))
	defer proxy.Close()
	t.Setenv("HTTP_PROXY", proxy.URL)
	t.Setenv("HTTPS_PROXY", proxy.URL)
	for i := 0; i < 2; i++ {
		if err := Probe(context.Background(), Check{URL: s.URL, ExpectedBody: "healthy\n"}); err != nil {
			t.Fatal(err)
		}
	}
}

func TestPayloadVerificationRetriesRecoveryAndDoesNotHideABrokenTunnel(t *testing.T) {
	var attempts atomic.Int32
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/broken" || attempts.Add(1) == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		io.WriteString(w, "ok")
	}))
	defer s.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	check := Check{Tunnel: "recovering", URL: s.URL, ExpectedBody: "ok"}
	if bad := Verify(ctx, []Check{check}, true); len(bad) != 0 || attempts.Load() < 2 {
		t.Fatalf("startup recovery failed: %+v", bad)
	}
	ctx2, cancel2 := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel2()
	broken := Check{Tunnel: "broken", URL: s.URL + "/broken", ExpectedBody: "ok"}
	bad := Verify(ctx2, []Check{check, broken}, true)
	if len(bad) != 1 || bad[0].Tunnel != "broken" {
		t.Fatalf("one healthy tunnel hid a failure: %+v", bad)
	}
}

func TestPayloadVerificationHasOneDeadlineAndBoundedConcurrency(t *testing.T) {
	var active, peak atomic.Int32
	s := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		n := active.Add(1)
		defer active.Add(-1)
		for p := peak.Load(); n > p && !peak.CompareAndSwap(p, n); p = peak.Load() {
		}
		<-r.Context().Done()
	}))
	defer s.Close()
	var checks []Check
	for i := 0; i < 12; i++ {
		checks = append(checks, Check{Tunnel: fmt.Sprint(i), URL: s.URL, ExpectedBody: "ok"})
	}
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	start := time.Now()
	bad := Verify(ctx, checks, true)
	if len(bad) != len(checks) || peak.Load() > 4 || time.Since(start) > 2*time.Second {
		t.Fatalf("deadline/concurrency not bounded: failures=%d peak=%d elapsed=%s", len(bad), peak.Load(), time.Since(start))
	}
}
