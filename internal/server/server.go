// Package server exposes the merge queue over HTTP and serves the browser UI.
package server

import (
	"embed"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"

	"mergebot/internal/queue"
	"mergebot/internal/review"
)

//go:embed index.html
var staticFS embed.FS

// Queue is the subset of the queue manager the HTTP layer needs.
type Queue interface {
	List() []queue.Item
	Add(number int)
	Remove(number int)
	Requeue(number int) bool
	Clear(phases []queue.Phase)
}

// Reviewer supplies the "my open PRs awaiting approvals" dashboard.
type Reviewer interface {
	List() []review.Entry
	Loaded() bool
	TriggerRefresh()
}

// Server adapts a queue manager to an http.Handler.
type Server struct {
	queue    Queue
	reviewer Reviewer
	repo     string
}

// New builds a Server backed by the given queue and review dashboard. repo is the
// owner/name shown in the UI for building PR links. reviewer may be nil.
func New(q Queue, repo string, reviewer Reviewer) *Server {
	return &Server{queue: q, reviewer: reviewer, repo: repo}
}

// Handler returns the routed HTTP handler.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /", s.index)
	mux.HandleFunc("GET /api/config", s.config)
	mux.HandleFunc("GET /api/ready", s.readyForReview)
	mux.HandleFunc("POST /api/ready/refresh", s.refreshReady)
	mux.HandleFunc("GET /api/items", s.listItems)
	mux.HandleFunc("POST /api/items", s.addItem)
	mux.HandleFunc("DELETE /api/items", s.clearItems)
	mux.HandleFunc("DELETE /api/items/{number}", s.removeItem)
	mux.HandleFunc("POST /api/items/{number}/requeue", s.requeueItem)

	return mux
}

func (s *Server) config(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"repo": s.repo})
}

func (s *Server) index(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}

	data, err := staticFS.ReadFile("index.html")
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(data)
}

func (s *Server) listItems(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.queue.List())
}

// readyForReview returns the token owner's open PRs and whether the first
// background refresh has finished (so the UI can show a loading state).
func (s *Server) readyForReview(w http.ResponseWriter, r *http.Request) {
	if s.reviewer == nil {
		writeJSON(w, http.StatusOK, map[string]any{"loaded": true, "prs": []review.Entry{}})
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"loaded": s.reviewer.Loaded(),
		"prs":    s.reviewer.List(),
	})
}

// refreshReady asks the dashboard to rebuild now (fresh data on demand).
func (s *Server) refreshReady(w http.ResponseWriter, r *http.Request) {
	if s.reviewer != nil {
		s.reviewer.TriggerRefresh()
	}

	writeJSON(w, http.StatusAccepted, map[string]bool{"ok": true})
}

func (s *Server) addItem(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Number int `json:"number"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}

	if body.Number <= 0 {
		http.Error(w, "number must be a positive integer", http.StatusBadRequest)
		return
	}

	s.queue.Add(body.Number)
	writeJSON(w, http.StatusAccepted, map[string]bool{"ok": true})
}

// clearItems drops every finished item in the phases named by the comma-
// separated ?phase= query (e.g. ?phase=merged,stopped).
func (s *Server) clearItems(w http.ResponseWriter, r *http.Request) {
	raw := r.URL.Query().Get("phase")
	if raw == "" {
		http.Error(w, "phase query parameter is required", http.StatusBadRequest)
		return
	}

	var phases []queue.Phase
	for _, p := range strings.Split(raw, ",") {
		if p = strings.TrimSpace(p); p != "" {
			phases = append(phases, queue.Phase(p))
		}
	}

	s.queue.Clear(phases)
	writeJSON(w, http.StatusAccepted, map[string]bool{"ok": true})
}

func (s *Server) removeItem(w http.ResponseWriter, r *http.Request) {
	number, err := strconv.Atoi(r.PathValue("number"))
	if err != nil || number <= 0 {
		http.Error(w, "invalid PR number", http.StatusBadRequest)
		return
	}

	s.queue.Remove(number)
	writeJSON(w, http.StatusAccepted, map[string]bool{"ok": true})
}

// requeueItem sends a parked PR back to the main queue immediately.
func (s *Server) requeueItem(w http.ResponseWriter, r *http.Request) {
	number, err := strconv.Atoi(r.PathValue("number"))
	if err != nil || number <= 0 {
		http.Error(w, "invalid PR number", http.StatusBadRequest)
		return
	}

	if !s.queue.Requeue(number) {
		http.Error(w, "PR is not parked", http.StatusConflict)
		return
	}

	writeJSON(w, http.StatusAccepted, map[string]bool{"ok": true})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
