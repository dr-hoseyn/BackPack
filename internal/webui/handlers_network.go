package webui

import (
	"context"
	"github.com/backpack/backpack/internal/app"
	"github.com/backpack/backpack/internal/manage"
	"github.com/backpack/backpack/internal/testhistory"
	"net/http"
	"time"
)

type managedLinkPlan struct {
	id, raw, name string
	expires       time.Time
}

func (s *server) handleManagedLink(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var request struct {
		Action string `json:"action"`
		ID     string `json:"id"`
		Link   string `json:"link"`
		Name   string `json:"name"`
	}
	if err := decodeJSON(w, r, &request); err != nil {
		http.Error(w, "invalid managed Setup Link request", http.StatusBadRequest)
		return
	}
	s.managedLinkMu.Lock()
	if s.managedLinkBusy {
		s.managedLinkMu.Unlock()
		http.Error(w, "another pairing operation is in progress", http.StatusConflict)
		return
	}
	s.managedLinkBusy = true
	s.managedLinkMu.Unlock()
	defer func() { s.managedLinkMu.Lock(); s.managedLinkBusy = false; s.managedLinkMu.Unlock() }()
	switch request.Action {
	case "preview":
		preview, err := manage.PreviewManagedPeer(request.Link, request.Name)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		id := manage.NewToken()
		s.managedLinkMu.Lock()
		s.managedLink = &managedLinkPlan{id: id, raw: request.Link, name: preview.Name, expires: time.Now().Add(2 * time.Minute)}
		s.managedLinkMu.Unlock()
		time.AfterFunc(2*time.Minute, func() {
			s.managedLinkMu.Lock()
			if s.managedLink != nil && s.managedLink.id == id {
				s.managedLink = nil
			}
			s.managedLinkMu.Unlock()
		})
		writeJSON(w, map[string]any{"id": id, "preview": preview})
	case "commit", "cancel":
		s.managedLinkMu.Lock()
		p := s.managedLink
		if p == nil || request.ID == "" || request.ID != p.id || time.Now().After(p.expires) {
			s.managedLinkMu.Unlock()
			http.Error(w, "link preview expired; review it again", http.StatusConflict)
			return
		}
		s.managedLink = nil
		s.managedLinkMu.Unlock()
		if request.Action == "cancel" {
			writeJSON(w, map[string]any{"cancelled": true})
			return
		}
		result, err := manage.CreateManagedPeer(p.raw, p.name)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		writeJSON(w, map[string]any{"name": result.Name, "service": result.Service, "active": result.Active})
	default:
		http.Error(w, "action must be preview, commit or cancel", http.StatusBadRequest)
	}
}

func (s *server) handleConnTestHistory(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	runs, err := manage.LoadConnTestHistory()
	result := map[string]any{"runs": runs, "comparison": testhistory.Compare(runs)}
	if err != nil {
		result["warning"] = "Some saved samples could not be read; valid samples are shown."
	}
	writeJSON(w, result)
}

func (s *server) handleAutomatic(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var request struct {
		Action string `json:"action"`
		ID     string `json:"id"`
		manage.AutomaticRequest
	}
	if err := decodeJSON(w, r, &request); err != nil {
		http.Error(w, "invalid Automatic request", http.StatusBadRequest)
		return
	}
	s.automaticMu.Lock()
	if s.automaticBusy {
		s.automaticMu.Unlock()
		http.Error(w, "another Automatic operation is in progress", http.StatusConflict)
		return
	}
	s.automaticBusy = true
	s.automaticMu.Unlock()
	defer func() { s.automaticMu.Lock(); s.automaticBusy = false; s.automaticMu.Unlock() }()
	switch request.Action {
	case "preview":
		ctx, cancel := context.WithTimeout(r.Context(), 45*time.Second)
		defer cancel()
		plan, err := manage.PrepareAutomatic(ctx, request.AutomaticRequest)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		id := manage.NewToken()
		s.automaticMu.Lock()
		old := s.automaticPlan
		s.automaticPlan, s.automaticID = plan, id
		s.automaticMu.Unlock()
		if old != nil {
			old.Close()
		}
		// Release reservations on panel shutdown, and clear the retained plan
		// after expiry even when the browser never follows through.
		if s.ctx != nil {
			stop := context.AfterFunc(s.ctx, plan.Close)
			time.AfterFunc(time.Until(plan.Preview.Expires), func() { stop() })
		}
		time.AfterFunc(time.Until(plan.Preview.Expires), func() {
			s.automaticMu.Lock()
			if s.automaticID == id {
				s.automaticPlan = nil
				s.automaticID = ""
			}
			s.automaticMu.Unlock()
		})
		writeJSON(w, map[string]any{"id": id, "preview": plan.Preview})
	case "commit", "cancel":
		s.automaticMu.Lock()
		plan := s.automaticPlan
		if plan == nil || request.ID == "" || request.ID != s.automaticID {
			s.automaticMu.Unlock()
			http.Error(w, "preview expired; prepare it again", http.StatusConflict)
			return
		}
		s.automaticPlan = nil
		s.automaticID = ""
		s.automaticMu.Unlock()
		if request.Action == "cancel" {
			plan.Close()
			writeJSON(w, map[string]any{"cancelled": true})
			return
		}
		name, service, link, active, err := plan.Commit()
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		writeJSON(w, map[string]any{"name": name, "service": service, "link": link, "active": active})
	default:
		http.Error(w, "action must be preview, commit or cancel", http.StatusBadRequest)
	}
}

func (s *server) handleCarrierDiagnose(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var request struct {
		Name string `json:"name"`
		Path string `json:"path"`
	}
	if err := decodeJSON(w, r, &request); err != nil || !manage.ValidName(request.Name) {
		http.Error(w, "provide a valid local tunnel name", http.StatusBadRequest)
		return
	}
	if _, ok := manage.Find(request.Name); !ok {
		http.Error(w, "tunnel not found", http.StatusNotFound)
		return
	}
	if !s.diagnosticMu.TryLock() {
		http.Error(w, "a carrier diagnostic is already running", http.StatusConflict)
		return
	}
	defer s.diagnosticMu.Unlock()
	ctx, cancel := context.WithTimeout(r.Context(), 55*time.Second)
	defer cancel()
	reports, err := manage.DiagnoseDatagramConfig(ctx, app.ConfigPath(request.Name), request.Path)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	writeJSON(w, map[string]any{"reports": reports})
}
