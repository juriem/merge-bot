// Package server exposes the merge queue over HTTP and serves the browser UI.
package server

import (
	"embed"
	"encoding/json"
	"net/http"
	"strconv"

	"github.com/wallester/mergebot/internal/queue"
)

//go:embed index.html
var staticFS embed.FS

// Queue is the subset of the queue manager the HTTP layer needs.
type Queue interface {
	List() []queue.Item
	Add(number int)
	Remove(number int)
}

// Server adapts a queue manager to an http.Handler.
type Server struct {
	queue Queue
	repo  string
}

// New builds a Server backed by the given queue. repo is the owner/name shown
// in the UI for building PR links.
func New(q Queue, repo string) *Server {
	return &Server{queue: q, repo: repo}
}

// Handler returns the routed HTTP handler.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /", s.index)
	mux.HandleFunc("GET /api/config", s.config)
	mux.HandleFunc("GET /api/items", s.listItems)
	mux.HandleFunc("POST /api/items", s.addItem)
	mux.HandleFunc("DELETE /api/items/{number}", s.removeItem)

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

func (s *Server) removeItem(w http.ResponseWriter, r *http.Request) {
	number, err := strconv.Atoi(r.PathValue("number"))
	if err != nil || number <= 0 {
		http.Error(w, "invalid PR number", http.StatusBadRequest)
		return
	}

	s.queue.Remove(number)
	writeJSON(w, http.StatusAccepted, map[string]bool{"ok": true})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
