package server

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/sahilshubham/bhatti/pkg/engine"
	"github.com/sahilshubham/bhatti/pkg/store"
)

// Server is the HTTP API server.
type Server struct {
	engine    engine.Engine
	store     *store.Store
	authToken string
	mux       *http.ServeMux
	proxy     *ProxyManager
	stopScan  context.CancelFunc
}

// New creates a new API server. If webDir is non-empty, serves the web UI at /.
func New(eng engine.Engine, st *store.Store, authToken string, webDir ...string) *Server {
	s := &Server{
		engine:    eng,
		store:     st,
		authToken: authToken,
		mux:       http.NewServeMux(),
		proxy:     NewProxyManager(),
	}
	s.routes()
	if len(webDir) > 0 && webDir[0] != "" {
		s.mux.Handle("/", http.FileServer(http.Dir(webDir[0])))
	}
	s.startPortScanner(3 * time.Second)
	return s
}

// Close stops the port scanner and cleans up resources.
func (s *Server) Close() {
	if s.stopScan != nil {
		s.stopScan()
	}
}

// startPortScanner polls running sandboxes for listening ports and auto-forwards them.
func (s *Server) startPortScanner(interval time.Duration) {
	ctx, cancel := context.WithCancel(context.Background())
	s.stopScan = cancel

	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				s.scanPorts()
			}
		}
	}()
}

func (s *Server) scanPorts() {
	sandboxes, err := s.store.ListSandboxes()
	if err != nil {
		return
	}
	for _, sb := range sandboxes {
		if sb.Status != "running" {
			// Sandbox stopped — tear down any forwards
			s.proxy.StopAll(sb.ID)
			continue
		}

		ports, err := s.engine.ListeningPorts(context.Background(), sb.EngineID)
		if err != nil {
			continue
		}

		// Get current forwards
		current := s.proxy.ActiveForwards(sb.ID)
		currentSet := map[int]bool{}
		for _, f := range current {
			currentSet[f.ContainerPort] = true
		}

		// Get sandbox IP for forwarding
		info, err := s.engine.Status(context.Background(), sb.EngineID)
		if err != nil {
			continue
		}

		// Forward new ports
		for _, p := range ports {
			if !currentSet[p] {
				s.proxy.Forward(sb.ID, info.IP, p)
			}
		}

		// Remove stale forwards
		portSet := map[int]bool{}
		for _, p := range ports {
			portSet[p] = true
		}
		for _, f := range current {
			if !portSet[f.ContainerPort] {
				s.proxy.StopForward(sb.ID, f.ContainerPort)
			}
		}
	}
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
