package webui

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/backpack/backpack/internal/manage"
)

func TestConnectionTestPanelPreservesSeparateSNIDomains(t *testing.T) {
	rows := []conntestRow{
		rowOf(manage.ConnTestResult{Kind: "direct", Transport: "sni", SNIDomain: "www.microsoft.com", Status: "ok"}),
		rowOf(manage.ConnTestResult{Kind: "direct", Transport: "sni", SNIDomain: "www.apple.com", Status: "down"}),
	}
	s := &server{conntest: conntestRunner{v: conntestView{State: ctFinished, Rows: rows}}}
	w := httptest.NewRecorder()
	s.handleConnTest(w, httptest.NewRequest(http.MethodGet, "/api/conntest", nil))
	var view conntestView
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &view) != nil || len(view.Rows) != 2 ||
		view.Rows[0].SNIDomain != "www.microsoft.com" || view.Rows[1].SNIDomain != "www.apple.com" || view.Rows[1].Status != "down" {
		t.Fatalf("SNI candidates collapsed in the panel response: %s", w.Body)
	}
}
