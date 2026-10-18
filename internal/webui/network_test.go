package webui

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestNetworkReadTokensCannotStartHelpersOrCreateTunnels(t *testing.T) {
	isolateAccess(t)
	isolateAlerts(t)
	secret, _, err := IssueToken("network-reader", ScopeRead, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	mux := newServer().routes()
	for _, path := range []string{"/api/automatic", "/api/carrier/diagnose", "/api/managed/link"} {
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, req("POST", path, secret))
		if w.Code != 403 {
			t.Fatalf("%s: %d", path, w.Code)
		}
	}
}

func TestNetworkStalePairingCancellationKeepsCurrentPreview(t *testing.T) {
	s := &server{managedLink: &managedLinkPlan{id: "current", expires: time.Now().Add(time.Minute)}}
	for _, body := range []string{`{"action":"cancel","id":"previous"}`, `{"action":"cancel"}`, `{"action":"commit","id":"previous"}`} {
		w := httptest.NewRecorder()
		s.handleManagedLink(w, httptest.NewRequest(http.MethodPost, "/api/managed/link", strings.NewReader(body)))
		if w.Code != http.StatusConflict || s.managedLink == nil || s.managedLink.id != "current" {
			t.Fatalf("stale request changed the current preview: %d", w.Code)
		}
	}
	w := httptest.NewRecorder()
	s.handleManagedLink(w, httptest.NewRequest(http.MethodPost, "/api/managed/link", strings.NewReader(`{"action":"cancel","id":"current"}`)))
	if w.Code != http.StatusOK || s.managedLink != nil || s.managedLinkBusy {
		t.Fatal("matching cancellation did not consume only its own preview")
	}
}

func TestNetworkPairingRejectsConcurrentOperations(t *testing.T) {
	s := &server{managedLinkBusy: true, managedLink: &managedLinkPlan{id: "current", expires: time.Now().Add(time.Minute)}}
	for _, action := range []string{"preview", "commit", "cancel"} {
		w := httptest.NewRecorder()
		s.handleManagedLink(w, httptest.NewRequest(http.MethodPost, "/api/managed/link", strings.NewReader(`{"action":"`+action+`","id":"current"}`)))
		if w.Code != http.StatusConflict || s.managedLink == nil || !s.managedLinkBusy {
			t.Fatal("concurrent pairing operation disturbed the in-flight operation")
		}
	}
}
