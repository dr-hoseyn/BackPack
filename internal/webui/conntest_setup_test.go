package webui

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestConnTestSetupRequiresAFinishedSession(t *testing.T) {
	s := newServer()
	r := httptest.NewRequest(http.MethodPost, "/api/conntest", strings.NewReader(
		"action=setup&id=test&kind=reverse&transport=tcp&ports=443"))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	s.handleConnTest(w, r)
	if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "finish the connection test") {
		t.Fatalf("setup without a test: %d %s", w.Code, w.Body.String())
	}
}
