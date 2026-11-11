package server

import (
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"jaild/internal/manager"
	"jaild/internal/model"
	"jaild/internal/store"
)

//go:embed web/index.html
var webFS embed.FS

type Server struct {
	mgr *manager.Manager
	mux *http.ServeMux
}

func New(mgr *manager.Manager) *Server {
	s := &Server{
		mgr: mgr,
		mux: http.NewServeMux(),
	}
	s.routes()
	return s
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mux.ServeHTTP(w, r)
}

func (s *Server) routes() {
	s.mux.HandleFunc("GET /{$}", s.handleIndex)
	s.mux.HandleFunc("GET /api/events", s.handleEvents)
	s.mux.HandleFunc("GET /api/jails", s.handleListJails)
	s.mux.HandleFunc("POST /api/jails", s.handlePutJail)
	s.mux.HandleFunc("GET /api/jails/{name}", s.handleGetJail)
	s.mux.HandleFunc("DELETE /api/jails/{name}", s.handleDeleteJail)
	s.mux.HandleFunc("POST /api/jails/{name}/stage", s.handleEnsureStage)
	s.mux.HandleFunc("DELETE /api/jails/{name}/stage", s.handleStopStage)
	s.mux.HandleFunc("POST /api/jails/{name}/expose", s.handleExpose)
	s.mux.HandleFunc("POST /api/jails/{name}/hide", s.handleHide)
	s.mux.HandleFunc("POST /api/jails/{name}/packages", s.handleAddPackages)
	s.mux.HandleFunc("POST /api/jails/{name}/run-spec", s.handleRunSpec)
}

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	data, err := webFS.ReadFile("web/index.html")
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(data)
}

func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	ch, unsub := s.mgr.SubscribeEvents()
	defer unsub()

	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-r.Context().Done():
			return
		case <-ticker.C:
			_, _ = fmt.Fprint(w, ": keepalive\n\n")
			flusher.Flush()
		case ev, ok := <-ch:
			if !ok {
				return
			}
			raw, err := json.Marshal(ev)
			if err != nil {
				continue
			}
			_, _ = fmt.Fprintf(w, "data: %s\n\n", raw)
			flusher.Flush()
		}
	}
}

func (s *Server) handleListJails(w http.ResponseWriter, r *http.Request) {
	list, err := s.mgr.ListStatuses()
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, list)
}

func (s *Server) handlePutJail(w http.ResponseWriter, r *http.Request) {
	var cfg model.JailConfig
	if err := json.NewDecoder(r.Body).Decode(&cfg); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	st, err := s.mgr.CreateOrUpdateJail(cfg)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, st)
}

func (s *Server) handleGetJail(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	st, err := s.mgr.GetStatus(name)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, st)
}

func (s *Server) handleDeleteJail(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if err := s.mgr.DeleteJail(name); err != nil {
		writeErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleEnsureStage(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	st, err := s.mgr.EnsureStage(name)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, st)
}

func (s *Server) handleStopStage(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if err := s.mgr.StopStage(name); err != nil {
		writeErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleExpose(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	var req model.MountEntry
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	st, err := s.mgr.Expose(name, req.Path, req.Target, req.ReadWrite)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, st)
}

func (s *Server) handleHide(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	var req struct {
		Path string `json:"path"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	st, err := s.mgr.Hide(name, req.Path)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, st)
}

func (s *Server) handleAddPackages(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	var req struct {
		Packages []string `json:"packages"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if r.URL.Query().Get("async") == "1" {
		if err := s.mgr.AddPackagesAsync(name, req.Packages); err != nil {
			writeErr(w, err)
			return
		}
		w.WriteHeader(http.StatusAccepted)
		return
	}
	st, err := s.mgr.AddPackages(name, req.Packages)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, st)
}

func (s *Server) handleRunSpec(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	var req model.RunRequest
	if r.ContentLength != 0 {
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
	}
	spec, err := s.mgr.BuildRunSpec(name, req)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, spec)
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, err error) {
	if errors.Is(err, store.ErrNotFound) {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	if errors.Is(err, store.ErrInvalidName) {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if errors.Is(err, manager.ErrJailBusy) {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	http.Error(w, err.Error(), http.StatusInternalServerError)
}
