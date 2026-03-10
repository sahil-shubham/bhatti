package server

import (
	"encoding/json"
	"log"
	"net/http"
	"strings"

	"github.com/sahilshubham/forge/pkg/engine"
	"github.com/sahilshubham/forge/pkg/store"
)

// Server is the HTTP API server.
type Server struct {
	engine    engine.Engine
	store     *store.Store
	authToken string
	mux       *http.ServeMux
}

// New creates a new API server.
func New(eng engine.Engine, st *store.Store, authToken string) *Server {
	s := &Server{
		engine:    eng,
		store:     st,
		authToken: authToken,
		mux:       http.NewServeMux(),
	}
	s.routes()
	return s
}

// ServeHTTP implements http.Handler.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Auth middleware
	if s.authToken != "" {
		token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if token != s.authToken {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
			return
		}
	}
	s.mux.ServeHTTP(w, r)
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		log.Printf("json encode error: %v", err)
	}
}

func readJSON(r *http.Request, v any) error {
	defer r.Body.Close()
	return json.NewDecoder(r.Body).Decode(v)
}

// errResp is a helper for error responses.
func errResp(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}
