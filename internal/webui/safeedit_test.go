package webui

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestSafePreviewRejectsTraversalAndWrongMethods(t *testing.T) {
	s := &server{}
	for _, tc := range []struct {
		route   string
		handler http.HandlerFunc
	}{
		{"/api/tunnel/preview", s.handleTunnelPreview},
		{"/api/confhist/preview", s.handleConfPreview},
	} {
		w := httptest.NewRecorder()
		tc.handler(w, httptest.NewRequest(http.MethodGet, tc.route, nil))
		if w.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s accepts GET", tc.route)
		}
		w = httptest.NewRecorder()
		tc.handler(w, httptest.NewRequest(http.MethodPost, tc.route, strings.NewReader(`{"name":"../secrets","at":"123"}`)))
		if w.Code != http.StatusBadRequest {
			t.Errorf("%s traversal status=%d", tc.route, w.Code)
		}
	}
}

func TestSafePreviewRoutesRequireWriteAuth(t *testing.T) {
	source := readSourceFile(t, "server.go")
	for _, route := range []string{"/api/tunnel/preview", "/api/confhist/preview"} {
		start := strings.Index(source, `"`+route+`"`)
		if start < 0 {
			t.Fatalf("missing route %s", route)
		}
		line := strings.SplitN(source[start:], "\n", 2)[0]
		if !strings.Contains(line, "requireAuth") || strings.Contains(line, "requireReadAuth") {
			t.Errorf("%s is not writer-only", route)
		}
	}
}
