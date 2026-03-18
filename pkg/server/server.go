package server

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/sahilshubham/bhatti/pkg/agent/proto"
	"github.com/sahilshubham/bhatti/pkg/engine"
	"github.com/sahilshubham/bhatti/pkg/store"
)

// ThermalEngine is optionally implemented by engines that support thermal management.
type ThermalEngine interface {
	EnsureHot(ctx context.Context, id string) error
	ThermalState(id string) string
	Pause(ctx context.Context, id string) error
	Activity(ctx context.Context, id string) (*proto.ActivityInfo, error)
}

// ThermalConfig controls automatic thermal transitions.
type ThermalConfig struct {
	WarmTimeout time.Duration // idle → warm (default 30s)
	ColdTimeout time.Duration // warm idle → cold (default 30min)
}

// Server is the HTTP API server.
type Server struct {
	engine      engine.Engine
	store       *store.Store
	authToken   string
	mux         *http.ServeMux
	proxy       *ProxyManager
	stopScan    context.CancelFunc
	stopThermal context.CancelFunc
}

// New creates a new API server. If webDir is non-empty, serves the web UI at /.
func New(eng engine.Engine, st *store.Store, authToken string, webDir ...string) *Server {
	s := &Server{
		engine:    eng,
		store:     st,
		authToken: authToken,
		mux:       http.NewServeMux(),
		proxy:     NewProxyManager(eng),
	}
	s.routes()
	if len(webDir) > 0 && webDir[0] != "" {
		s.mux.Handle("/", http.FileServer(http.Dir(webDir[0])))
	}
	s.startPortScanner(3 * time.Second)
	return s
}

// Close stops the port scanner and thermal manager.
func (s *Server) Close() {
	if s.stopScan != nil {
		s.stopScan()
	}
	if s.stopThermal != nil {
		s.stopThermal()
	}
}

// StartThermalManager starts the background goroutine that transitions idle
// sandboxes through thermal states: hot → warm → cold.
func (s *Server) StartThermalManager(cfg ThermalConfig) {
	te, ok := s.engine.(ThermalEngine)
	if !ok {
		return // engine doesn't support thermal management
	}
	if cfg.WarmTimeout == 0 {
		cfg.WarmTimeout = 30 * time.Second
	}
	if cfg.ColdTimeout == 0 {
		cfg.ColdTimeout = 30 * time.Minute
	}

	ctx, cancel := context.WithCancel(context.Background())
	s.stopThermal = cancel

	go func() {
		ticker := time.NewTicker(10 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				s.runThermalCycle(te, cfg)
			}
		}
	}()
}

func (s *Server) runThermalCycle(te ThermalEngine, cfg ThermalConfig) {
	sandboxes, err := s.store.ListSandboxes()
	if err != nil {
		return
	}
	for _, sb := range sandboxes {
		if sb.Status != "running" {
			continue
		}

		thermal := te.ThermalState(sb.EngineID)
		if thermal == "cold" || thermal == "" {
			continue
		}

		activity, err := te.Activity(context.Background(), sb.EngineID)
		if err != nil {
			continue
		}

		idle := time.Since(time.Unix(activity.LastActivityUnix, 0))

		if thermal == "hot" && idle > cfg.WarmTimeout && activity.AttachedSessions == 0 {
			if err := te.Pause(context.Background(), sb.EngineID); err != nil {
				log.Printf("thermal: pause %s: %v", sb.Name, err)
				continue
			}
			log.Printf("thermal: %s → warm (idle %v)", sb.Name, idle.Round(time.Second))
		}

		if thermal == "warm" && idle > cfg.ColdTimeout {
			if err := s.engine.Stop(context.Background(), sb.EngineID); err != nil {
				log.Printf("thermal: snapshot %s: %v", sb.Name, err)
				continue
			}
			s.saveVMState(sb.ID, sb.EngineID)
			log.Printf("thermal: %s → cold (idle %v)", sb.Name, idle.Round(time.Second))
		}
	}
}

// ensureHot transparently wakes a sandbox from warm or cold state.
// Returns nil if the engine doesn't support thermal management.
func (s *Server) ensureHot(ctx context.Context, engineID string) error {
	te, ok := s.engine.(ThermalEngine)
	if !ok {
		return nil
	}
	return te.EnsureHot(ctx, engineID)
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
			s.proxy.StopAll(sb.ID)
			continue
		}

		// Skip non-hot VMs — don't wake them just to scan ports
		if te, ok := s.engine.(ThermalEngine); ok {
			if thermal := te.ThermalState(sb.EngineID); thermal != "hot" {
				continue
			}
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

		// Forward new ports (tunneled through engine, no IP needed)
		for _, p := range ports {
			if !currentSet[p] {
				s.proxy.Forward(sb.ID, sb.EngineID, p)
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
