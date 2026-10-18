package webui

import (
	"net/http/httptest"
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
	for _, path := range []string{"/api/automatic", "/api/carrier/diagnose"} {
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, req("POST", path, secret))
		if w.Code != 403 {
			t.Fatalf("%s: %d", path, w.Code)
		}
	}
}
