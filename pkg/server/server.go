package server

import (
	"encoding/json"
	"log"
	"net/http"
	"strings"

	"github.com/sahilshubham/bhatti/pkg/engine"
	"github.com/sahilshubham/bhatti/pkg/store"
)

// Server is the HTTP API server.
type Server struct {
	engine    engine.Engine
	store     *store.Store
	authToken string
	mux       *http.ServeMux
}

// New creates a new API server. If webDir is non-empty, serves the web UI at /.
func New(eng engine.Engine, st *store.Store, authToken string, webDir ...string) *Server {
	s := &Server{
		engine:    eng,
		store:     st,
		authToken: authToken,
		mux:       http.NewServeMux(),
	}
	s.routes()
	if len(webDir) > 0 && webDir[0] != "" {
		s.mux.Handle("/", http.FileServer(http.Dir(webDir[0])))
	}
	return s
}

// ServeHTTP implements http.Handler.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Skip auth for static files and WebSocket upgrades
	if s.authToken != "" && !isStaticPath(r.URL.Path) {
		// Allow WS auth via query param
		token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if token == "" {
			token = r.URL.Query().Get("token")
		}
		if token != s.authToken && !strings.HasSuffix(r.URL.Path, "/ws") {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
			return
		}
	}
	s.mux.ServeHTTP(w, r)
}

func isStaticPath(path string) bool {
	return path == "/" || path == "/index.html" || strings.HasPrefix(path, "/static/")
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
