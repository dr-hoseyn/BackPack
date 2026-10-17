package webui

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/backpack/backpack/internal/manage"
)

func TestConnectionTestPanelCarriesEachPurposeAndItsIPPath(t *testing.T) {
	best := manage.ConnTestBest{Transport: "reverse xhttp", Recommendations: []manage.ConnTestRecommendation{
		{Purpose: "download", Transport: "reverse xhttp", Host: "192.0.2.1", Family: "IPv4", DownloadMbps: 500, UploadMbps: 20, SpeedMeasured: true},
		{Purpose: "realtime", Transport: "direct sni", Host: "2001:db8::1", Family: "IPv6", SNIDomain: "www.microsoft.com", Samples: 60, RTTms: 10, JitterMs: 1},
	}}
	s := &server{conntest: conntestRunner{v: conntestView{State: ctFinished, Best: &best, Paths: []conntestPath{{Host: "2001:db8::1", Family: "IPv6", State: "done", Best: best}}}}}
	w := httptest.NewRecorder()
	s.handleConnTest(w, httptest.NewRequest(http.MethodGet, "/api/conntest", nil))
	var view conntestView
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &view) != nil || view.Best == nil || len(view.Best.Recommendations) != 2 || len(view.Paths) != 1 || len(view.Paths[0].Best.Recommendations) != 2 {
		t.Fatalf("panel response lost purpose choices: %s", w.Body)
	}
	choice := view.Best.Recommendations[1]
	if choice.Purpose != "realtime" || choice.Host != "2001:db8::1" || choice.SNIDomain != "www.microsoft.com" || choice.RTTms != 10 || choice.JitterMs != 1 {
		t.Fatalf("panel changed the chosen path or evidence: %+v", choice)
	}
}
