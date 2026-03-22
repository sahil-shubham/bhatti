package server

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/gorilla/websocket"

	"github.com/sahil-shubham/bhatti/pkg/agent"
	"github.com/sahil-shubham/bhatti/pkg/agent/proto"
	"github.com/sahil-shubham/bhatti/pkg/engine"
	"github.com/sahil-shubham/bhatti/pkg/secrets"
	"github.com/sahil-shubham/bhatti/pkg/store"
)

// saveVMState persists Firecracker VM state to the store if the engine supports it.
func (s *Server) saveVMState(sandboxID, engineID string) {
	provider, ok := s.engine.(engine.VMStateProvider)
	if !ok {
		return
	}
	state := provider.VMState(engineID)
	if state == nil {
		return
	}
	s.store.SaveFirecrackerState(sandboxID, store.FirecrackerState{
		RootfsPath:  strOrEmpty(state, "rootfs_path"),
		SnapMemPath: strOrEmpty(state, "snap_mem_path"),
		SnapVMPath:  strOrEmpty(state, "snap_vm_path"),
		VsockCID:    intOrZero(state, "vsock_cid"),
		TapDevice:   strOrEmpty(state, "tap_device"),
		GuestIP:     strOrEmpty(state, "guest_ip"),
		GuestMAC:    strOrEmpty(state, "guest_mac"),
		VcpuCount:   floatOrZero(state, "vcpu_count"),
		MemSizeMib:  intOrZero(state, "mem_size_mib"),
		SocketPath:  strOrEmpty(state, "socket_path"),
		VsockPath:   strOrEmpty(state, "vsock_path"),
	})
}

func strOrEmpty(m map[string]interface{}, k string) string {
	if v, ok := m[k].(string); ok {
		return v
	}
	return ""
}

func intOrZero(m map[string]interface{}, k string) int {
	switch v := m[k].(type) {
	case int:
		return v
	case int64:
		return int(v)
	case uint32:
		return int(v)
	case float64:
		return int(v)
	}
	return 0
}

func floatOrZero(m map[string]interface{}, k string) float64 {
	switch v := m[k].(type) {
	case float64:
		return v
	case int64:
		return float64(v)
	}
	return 0
}

func (s *Server) routes() {
	s.mux.HandleFunc("/health", s.handleHealth)
	s.mux.HandleFunc("/metrics", s.handleMetrics)
	s.mux.HandleFunc("/templates", s.handleTemplates)
	s.mux.HandleFunc("/templates/", s.handleTemplate)
	s.mux.HandleFunc("/sandboxes", s.handleSandboxes)
	s.mux.HandleFunc("/sandboxes/", s.handleSandbox)
	s.mux.HandleFunc("/secrets", s.handleSecrets)
	s.mux.HandleFunc("/secrets/", s.handleSecret)
	s.mux.HandleFunc("/volumes", s.handleVolumes)
	s.mux.HandleFunc("/volumes/", s.handleVolume)
	s.mux.HandleFunc("/ports", s.handleAllPorts)
}

func (s *Server) handleMetrics(w http.ResponseWriter, r *http.Request) {
	sandboxes, _ := s.store.ListAllSandboxes()
	users, _ := s.store.ListUsers()

	// Count thermal states
	var hot, warm, cold int
	if te, ok := s.engine.(ThermalEngine); ok {
		for _, sb := range sandboxes {
			if sb.Status != "running" {
				cold++
				continue
			}
			switch te.ThermalState(sb.EngineID) {
			case "hot":
				hot++
			case "warm":
				warm++
			default:
				cold++
			}
		}
	} else {
		for _, sb := range sandboxes {
			if sb.Status == "running" {
				hot++
			} else {
				cold++
			}
		}
	}

	// Count active users (users with at least one non-destroyed sandbox)
	activeUsers := 0
	userHasSandbox := make(map[string]bool)
	for _, sb := range sandboxes {
		userHasSandbox[sb.CreatedBy] = true
	}
	activeUsers = len(userHasSandbox)

	// Host stats (best effort — works on Linux, graceful on others)
	host := map[string]any{}
	if data, err := os.ReadFile("/proc/loadavg"); err == nil {
		var load1 float64
		fmt.Sscanf(string(data), "%f", &load1)
		host["load_1m"] = load1
	}
	if data, err := os.ReadFile("/proc/meminfo"); err == nil {
		lines := strings.Split(string(data), "\n")
		for _, line := range lines {
			if strings.HasPrefix(line, "MemTotal:") {
				var kb int64
				fmt.Sscanf(line, "MemTotal: %d kB", &kb)
				host["memory_total_mb"] = kb / 1024
			}
			if strings.HasPrefix(line, "MemAvailable:") {
				var kb int64
				fmt.Sscanf(line, "MemAvailable: %d kB", &kb)
				host["memory_available_mb"] = kb / 1024
			}
		}
	}

	writeJSON(w, 200, map[string]any{
		"uptime": time.Since(s.startTime).Round(time.Second).String(),
		"sandboxes": map[string]any{
			"total": len(sandboxes),
			"hot":   hot,
			"warm":  warm,
			"cold":  cold,
		},
		"users": map[string]any{
			"total":  len(users),
			"active": activeUsers,
		},
		"host": host,
		"requests": map[string]any{
			"total":         s.requestTotal.Load(),
			"errors_5xx":    s.requestErrors.Load(),
			"auth_failures": s.authFailures.Load(),
		},
	})
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	sandboxes, _ := s.store.ListAllSandboxes()
	writeJSON(w, 200, map[string]any{
		"status":    "ok",
		"sandboxes": len(sandboxes),
		"uptime":    time.Since(s.startTime).Round(time.Second).String(),
	})
}

// --- Templates ---

func (s *Server) handleTemplates(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		list, err := s.store.ListTemplates()
		if err != nil {
			errResp(w, 500, err.Error())
			return
		}
		if list == nil {
			list = []store.Template{}
		}
		writeJSON(w, 200, list)
	case http.MethodPost:
		var t store.Template
		if err := readJSON(r, &t); err != nil {
			errResp(w, 400, "invalid json: "+err.Error())
			return
		}
		if t.ID == "" {
			t.ID = genID()
		}
		if t.Engine == "" {
			t.Engine = "docker"
		}
		if t.CPUs == 0 {
			t.CPUs = 1
		}
		if t.MemoryMB == 0 {
			t.MemoryMB = 512
		}
		t.CreatedAt = time.Now()
		if err := s.store.CreateTemplate(t); err != nil {
			errResp(w, 500, err.Error())
			return
		}
		writeJSON(w, 201, t)
	default:
		errResp(w, 405, "method not allowed")
	}
}

func (s *Server) handleTemplate(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, "/templates/")
	if id == "" {
		errResp(w, 400, "missing template id")
		return
	}

	switch r.Method {
	case http.MethodGet:
		t, err := s.store.GetTemplate(id)
		if err != nil {
			errResp(w, 404, "not found")
			return
		}
		writeJSON(w, 200, t)
	case http.MethodDelete:
		if err := s.store.DeleteTemplate(id); err != nil {
			errResp(w, 404, err.Error())
			return
		}
		writeJSON(w, 200, map[string]string{"status": "deleted"})
	default:
		errResp(w, 405, "method not allowed")
	}
}

// --- Sandboxes ---

type createSandboxReq struct {
	Name       string               `json:"name"`
	TemplateID string               `json:"template_id,omitempty"`
	CPUs       float64              `json:"cpus,omitempty"`
	MemoryMB   int                  `json:"memory_mb,omitempty"`
	Env        map[string]string    `json:"env,omitempty"`
	Init       string               `json:"init,omitempty"`
	NewVolumes []engine.VolumeSpec  `json:"new_volumes,omitempty"`
	Volumes    []engine.VolumeMount `json:"volumes,omitempty"`
}

func (s *Server) handleSandboxes(w http.ResponseWriter, r *http.Request) {
	user := UserFromContext(r.Context())

	switch r.Method {
	case http.MethodGet:
		list, err := s.store.ListSandboxes(user.ID)
		if err != nil {
			errResp(w, 500, err.Error())
			return
		}
		if list == nil {
			list = []store.Sandbox{}
		}
		writeJSON(w, 200, list)
	case http.MethodPost:
		var req createSandboxReq
		if err := readJSON(r, &req); err != nil {
			errResp(w, 400, "invalid json: "+err.Error())
			return
		}

		// Enforce per-user sandbox count limit
		count, _ := s.store.CountUserSandboxes(user.ID)
		if count >= user.MaxSandboxes {
			errResp(w, 429, fmt.Sprintf("sandbox limit reached (%d/%d)", count, user.MaxSandboxes))
			return
		}

		// Enforce per-user resource caps
		if req.CPUs > float64(user.MaxCPUsPerSandbox) {
			errResp(w, 400, fmt.Sprintf("max %d CPUs per sandbox", user.MaxCPUsPerSandbox))
			return
		}
		if req.MemoryMB > user.MaxMemoryMBPerSandbox {
			errResp(w, 400, fmt.Sprintf("max %d MB memory per sandbox", user.MaxMemoryMBPerSandbox))
			return
		}

		// Validate sandbox name
		if req.Name != "" && !isValidName(req.Name) {
			errResp(w, 400, "invalid sandbox name: must match [a-zA-Z0-9][a-zA-Z0-9._-]{0,62}")
			return
		}

		var spec engine.SandboxSpec
		var templateID string
		var volumes []engine.VolumeMount

		if req.TemplateID != "" {
			// --- Template-based creation (existing behavior) ---
			tmpl, err := s.store.GetTemplate(req.TemplateID)
			if err != nil {
				errResp(w, 404, "template not found")
				return
			}
			templateID = tmpl.ID

			name := req.Name
			if name == "" {
				name = tmpl.Name + "-" + genID()[:6]
			}

			// Resolve volumes: use request volumes if provided, else template defaults
			volumes = req.Volumes
			if len(volumes) == 0 && len(tmpl.Mounts) > 0 {
				for _, m := range tmpl.Mounts {
					volName := m.VolumeName
					if volName == "" {
						volName = "bhatti-" + name + "-workspace"
					}
					if m.AutoCreate {
						s.store.CreateVolume(volName) // idempotent
					}
					volumes = append(volumes, engine.VolumeMount{
						Name: volName, Target: m.Target, ReadOnly: m.ReadOnly,
					})
				}
			}

			// Resolve secrets from template — decrypt before injecting
			secretEnv := make(map[string]string)
			secretFiles := make(map[string]engine.FileSpec)
			for _, secretName := range tmpl.Secrets {
				ciphertext, err := s.store.GetSecretValue(user.ID, secretName)
				if err != nil {
					errResp(w, 400, fmt.Sprintf("secret %q not found", secretName))
					return
				}
				plaintext, err := s.decryptSecret(ciphertext)
				if err != nil {
					errResp(w, 500, fmt.Sprintf("decrypt secret %q failed", secretName))
					return
				}
				secretEnv[secretName] = string(plaintext)
			}

			// Merge request env overrides
			env := make(map[string]string)
			for k, v := range secretEnv {
				env[k] = v
			}
			for k, v := range req.Env {
				env[k] = v
			}

			spec = engine.SandboxSpec{
				Name:     name,
				Image:    tmpl.Image,
				CPUs:     tmpl.CPUs,
				MemoryMB: tmpl.MemoryMB,
				Labels:   tmpl.Labels,
				UserData: tmpl.UserData,
				Env:      env,
				Files:    secretFiles,
				Volumes:  volumes,
			}
		} else {
			// --- Direct creation (no template) ---
			spec = engine.SandboxSpec{
				Name:       req.Name,
				CPUs:       req.CPUs,
				MemoryMB:   req.MemoryMB,
				Env:        req.Env,
				Init:       req.Init,
				NewVolumes: req.NewVolumes,
				Volumes:    req.Volumes,
			}
			volumes = req.Volumes

			// Apply defaults
			if spec.CPUs == 0 {
				spec.CPUs = 1
			}
			if spec.MemoryMB == 0 {
				spec.MemoryMB = 512
			}
			if spec.Name == "" {
				spec.Name = "sandbox-" + genID()[:6]
			}
		}

		// Check for duplicate name before booting a VM.
		// Without this, a name conflict is only discovered after engine.Create()
		// has already booted a VM (~3.5s), which then gets destroyed.
		if spec.Name != "" {
			existing, _ := s.store.ListSandboxes(user.ID)
			for _, sb := range existing {
				if sb.Name == spec.Name && sb.Status != "destroyed" {
					errResp(w, 409, fmt.Sprintf("sandbox %q already exists", spec.Name))
					return
				}
			}
		}

		// Set user context for engine-level network isolation
		spec.UserID = user.ID
		spec.SubnetIndex = user.SubnetIndex

		info, err := s.engine.Create(r.Context(), spec)
		if err != nil {
			errRespInternal(w, r, "sandbox create failed", err)
			return
		}

		sbID := genID()
		sb := store.Sandbox{
			ID:         sbID,
			Name:       spec.Name,
			TemplateID: templateID,
			EngineID:   info.EngineID,
			Status:     info.Status,
			IP:         info.IP,
			EngineMeta: json.RawMessage("{}"),
			CreatedBy:  user.ID,
			CreatedAt:  time.Now(),
		}
		if err := s.store.CreateSandbox(sb); err != nil {
			s.engine.Destroy(r.Context(), info.EngineID)
			errRespInternal(w, r, "store sandbox failed", err)
			return
		}

		// Record volume attachments
		for _, v := range volumes {
			s.store.AttachVolume(sbID, v.Name, v.Target, v.ReadOnly)
		}

		// Persist Firecracker VM state
		s.saveVMState(sbID, info.EngineID)

		slog.Info("sandbox.created",
			"sandbox_id", sb.ID, "name", sb.Name, "user", user.Name,
			"cpus", spec.CPUs, "memory_mb", spec.MemoryMB)
		writeJSON(w, 201, sb)
	default:
		errResp(w, 405, "method not allowed")
	}
}

func (s *Server) handleSandbox(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/sandboxes/")
	parts := strings.SplitN(path, "/", 2)
	id := parts[0]

	if id == "" {
		errResp(w, 400, "missing sandbox id")
		return
	}

	// Sub-routes
	if len(parts) == 2 {
		sub := parts[1]

		// Handle proxy/:port/... — sub may be "proxy/4321" or "proxy/4321/some/path"
		if strings.HasPrefix(sub, "proxy/") {
			s.handleSandboxProxyRoute(w, r, id, strings.TrimPrefix(sub, "proxy/"))
			return
		}

		switch sub {
		case "stop":
			s.handleSandboxStop(w, r, id)
		case "start":
			s.handleSandboxStart(w, r, id)
		case "exec":
			s.handleSandboxExec(w, r, id)
		case "ports":
			s.handleSandboxPorts(w, r, id)
		case "ws":
			s.handleSandboxWS(w, r, id)
		case "files":
			s.handleSandboxFiles(w, r, id)
		case "sessions":
			s.handleSandboxSessions(w, r, id)
		default:
			errResp(w, 404, "not found")
		}
		return
	}

	user := UserFromContext(r.Context())

	switch r.Method {
	case http.MethodGet:
		sb, err := s.store.GetSandbox(user.ID, id)
		if err != nil {
			errResp(w, 404, "not found")
			return
		}
		// Refresh status from engine
		info, err := s.engine.Status(r.Context(), sb.EngineID)
		if err == nil {
			sb.Status = info.Status
			sb.IP = info.IP
			s.store.UpdateSandboxStatus(id, info.Status)
		}
		writeJSON(w, 200, sb)
	case http.MethodDelete:
		sb, err := s.store.GetSandbox(user.ID, id)
		if err != nil {
			errResp(w, 404, "not found")
			return
		}
		if err := s.engine.Destroy(r.Context(), sb.EngineID); err != nil {
			slog.Warn("engine destroy failed", "sandbox", sb.ID, "error", err)
		}
		s.store.DetachVolumes(id)
		if err := s.store.DeleteSandbox(user.ID, id); err != nil {
			errRespInternal(w, r, "delete sandbox failed", err)
			return
		}
		slog.Info("sandbox.destroyed", "sandbox_id", sb.ID, "name", sb.Name, "user", user.Name)
		writeJSON(w, 200, map[string]string{"status": "destroyed"})
	default:
		errResp(w, 405, "method not allowed")
	}
}

func (s *Server) handleSandboxStop(w http.ResponseWriter, r *http.Request, id string) {
	if r.Method != http.MethodPost {
		errResp(w, 405, "method not allowed")
		return
	}
	sb := s.getUserSandbox(w, r, id)
	if sb == nil {
		return
	}
	if err := s.engine.Stop(r.Context(), sb.EngineID); err != nil {
		errResp(w, 500, err.Error())
		return
	}
	s.store.StopSandbox(id)
	s.saveVMState(id, sb.EngineID) // persist snapshot paths
	updated, _ := s.store.GetSandboxByID(id)
	writeJSON(w, 200, updated)
}

func (s *Server) handleSandboxStart(w http.ResponseWriter, r *http.Request, id string) {
	if r.Method != http.MethodPost {
		errResp(w, 405, "method not allowed")
		return
	}
	sb := s.getUserSandbox(w, r, id)
	if sb == nil {
		return
	}
	if err := s.engine.Start(r.Context(), sb.EngineID); err != nil {
		errResp(w, 500, err.Error())
		return
	}
	// Refresh from engine — IP may have changed after restart
	info, err := s.engine.Status(r.Context(), sb.EngineID)
	if err == nil {
		s.store.UpdateSandboxStatus(id, info.Status)
		s.store.UpdateSandboxEngine(id, sb.EngineID, info.IP)
	} else {
		s.store.UpdateSandboxStatus(id, "running")
	}
	s.saveVMState(id, sb.EngineID) // persist updated state
	updated, _ := s.store.GetSandboxByID(id)
	writeJSON(w, 200, updated)
}

type execReq struct {
	Cmd        []string `json:"cmd"`
	TimeoutSec int      `json:"timeout_sec,omitempty"` // default 300, max 3600
}

func (s *Server) handleSandboxExec(w http.ResponseWriter, r *http.Request, id string) {
	if r.Method != http.MethodPost {
		errResp(w, 405, "method not allowed")
		return
	}
	sb := s.getUserSandbox(w, r, id)
	if sb == nil {
		return
	}
	var req execReq
	if err := readJSON(r, &req); err != nil {
		errResp(w, 400, "invalid json: "+err.Error())
		return
	}
	if len(req.Cmd) == 0 {
		errResp(w, 400, "cmd required")
		return
	}
	if err := s.ensureHot(r.Context(), sb.EngineID); err != nil {
		errResp(w, 500, "wake sandbox: "+err.Error())
		return
	}

	// Apply exec timeout (default 300s, max 3600s)
	timeout := 300 * time.Second
	if req.TimeoutSec > 0 && req.TimeoutSec <= 3600 {
		timeout = time.Duration(req.TimeoutSec) * time.Second
	}
	execCtx, cancel := context.WithTimeout(r.Context(), timeout)
	defer cancel()

	// Streaming NDJSON when requested via Accept header
	if r.Header.Get("Accept") == "application/x-ndjson" {
		s.handleSandboxExecStream(w, r.WithContext(execCtx), sb, req)
		return
	}

	// Buffered JSON (existing behavior)
	result, err := s.engine.Exec(execCtx, sb.EngineID, req.Cmd)
	if err != nil {
		errResp(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, result)
}

// handleSandboxExecStream streams exec output as NDJSON (one JSON object per line).
// Each line is flushed immediately so the client sees output in real time.
func (s *Server) handleSandboxExecStream(w http.ResponseWriter, r *http.Request, sb *store.Sandbox, req execReq) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		errResp(w, 500, "streaming not supported")
		return
	}

	w.Header().Set("Content-Type", "application/x-ndjson")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(200)

	enc := json.NewEncoder(w)

	// If engine supports streaming, use it directly
	if se, ok := s.engine.(engine.StreamExecEngine); ok {
		se.ExecStream(r.Context(), sb.EngineID, req.Cmd, func(event engine.StreamEvent) {
			enc.Encode(event)
			flusher.Flush()
		})
		return
	}

	// Fallback: buffer then emit as NDJSON events
	result, err := s.engine.Exec(r.Context(), sb.EngineID, req.Cmd)
	if err != nil {
		enc.Encode(engine.StreamEvent{Type: "error", Data: err.Error()})
		flusher.Flush()
		return
	}
	if result.Stdout != "" {
		enc.Encode(engine.StreamEvent{Type: "stdout", Data: result.Stdout})
		flusher.Flush()
	}
	if result.Stderr != "" {
		enc.Encode(engine.StreamEvent{Type: "stderr", Data: result.Stderr})
		flusher.Flush()
	}
	code := result.ExitCode
	enc.Encode(engine.StreamEvent{Type: "exit", ExitCode: &code})
	flusher.Flush()
}

var upgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool { return true },
}

func (s *Server) handleSandboxWS(w http.ResponseWriter, r *http.Request, id string) {
	sb := s.getUserSandbox(w, r, id)
	if sb == nil {
		return
	}

	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		slog.Error("websocket upgrade failed", "error", err)
		return
	}
	defer conn.Close()

	if err := s.ensureHot(r.Context(), sb.EngineID); err != nil {
		conn.WriteMessage(websocket.TextMessage, []byte("wake sandbox: "+err.Error()))
		return
	}
	term, err := s.engine.Shell(r.Context(), sb.EngineID)
	if err != nil {
		conn.WriteMessage(websocket.TextMessage, []byte("shell error: "+err.Error()))
		return
	}
	defer term.Close()

	// Terminal → WebSocket
	go func() {
		buf := make([]byte, 4096)
		for {
			n, err := term.Read(buf)
			if err != nil {
				conn.WriteMessage(websocket.CloseMessage, nil)
				return
			}
			if err := conn.WriteMessage(websocket.BinaryMessage, buf[:n]); err != nil {
				return
			}
		}
	}()

	// WebSocket → Terminal
	for {
		msgType, msg, err := conn.ReadMessage()
		if err != nil {
			return
		}

		// Handle resize messages (JSON: {"type":"resize","rows":N,"cols":N})
		if msgType == websocket.TextMessage {
			var resize struct {
				Type string `json:"type"`
				Rows int    `json:"rows"`
				Cols int    `json:"cols"`
			}
			if json.Unmarshal(msg, &resize) == nil && resize.Type == "resize" {
				if err := term.Resize(resize.Rows, resize.Cols); err != nil {
					slog.Debug("terminal resize failed", "error", err)
				}
				continue
			}
		}

		if _, err := term.Write(msg); err != nil {
			return
		}
	}
}

// --- Secrets ---

type createSecretReq struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

func (s *Server) handleSecrets(w http.ResponseWriter, r *http.Request) {
	user := UserFromContext(r.Context())

	switch r.Method {
	case http.MethodGet:
		list, err := s.store.ListUserSecrets(user.ID)
		if err != nil {
			errResp(w, 500, err.Error())
			return
		}
		if list == nil {
			list = []store.SecretRecord{}
		}
		writeJSON(w, 200, list)
	case http.MethodPost:
		var req createSecretReq
		if err := readJSON(r, &req); err != nil {
			errResp(w, 400, "invalid json: "+err.Error())
			return
		}
		if req.Name == "" || req.Value == "" {
			errResp(w, 400, "name and value required")
			return
		}
		// Encrypt the secret value before storing
		ciphertext, err := s.encryptSecret([]byte(req.Value))
		if err != nil {
			errResp(w, 500, "encryption failed")
			return
		}
		if err := s.store.SetSecret(user.ID, req.Name, ciphertext); err != nil {
			errResp(w, 500, err.Error())
			return
		}
		sr, _ := s.store.GetSecret(user.ID, req.Name)
		writeJSON(w, 201, sr)
	default:
		errResp(w, 405, "method not allowed")
	}
}

func (s *Server) handleSecret(w http.ResponseWriter, r *http.Request) {
	user := UserFromContext(r.Context())
	name := strings.TrimPrefix(r.URL.Path, "/secrets/")
	if name == "" {
		errResp(w, 400, "missing secret name")
		return
	}
	switch r.Method {
	case http.MethodDelete:
		if err := s.store.DeleteSecret(user.ID, name); err != nil {
			errResp(w, 404, err.Error())
			return
		}
		writeJSON(w, 200, map[string]string{"status": "deleted"})
	default:
		errResp(w, 405, "method not allowed")
	}
}

// --- Ports ---

type portInfo struct {
	SandboxID     string `json:"sandbox_id,omitempty"`
	ContainerPort int    `json:"container_port"`
	ProxyURL      string `json:"proxy_url"`
}

func (s *Server) handleSandboxPorts(w http.ResponseWriter, r *http.Request, id string) {
	if r.Method != http.MethodGet {
		errResp(w, 405, "method not allowed")
		return
	}
	sb := s.getUserSandbox(w, r, id)
	if sb == nil {
		return
	}

	if err := s.ensureHot(r.Context(), sb.EngineID); err != nil {
		errResp(w, 500, "wake sandbox: "+err.Error())
		return
	}
	ports, err := s.engine.ListeningPorts(r.Context(), sb.EngineID)
	if err != nil {
		ports = []int{}
	}

	out := make([]portInfo, 0, len(ports))
	for _, p := range ports {
		out = append(out, portInfo{
			ContainerPort: p,
			ProxyURL:      fmt.Sprintf("/sandboxes/%s/proxy/%d/", id, p),
		})
	}
	writeJSON(w, 200, out)
}

func (s *Server) handleAllPorts(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		errResp(w, 405, "method not allowed")
		return
	}
	user := UserFromContext(r.Context())
	sandboxes, err := s.store.ListSandboxes(user.ID)
	if err != nil {
		errResp(w, 500, err.Error())
		return
	}

	var out []portInfo
	for _, sb := range sandboxes {
		if sb.Status != "running" {
			continue
		}
		ports, err := s.engine.ListeningPorts(context.Background(), sb.EngineID)
		if err != nil {
			continue
		}
		for _, p := range ports {
			out = append(out, portInfo{
				SandboxID:     sb.ID,
				ContainerPort: p,
				ProxyURL:      fmt.Sprintf("/sandboxes/%s/proxy/%d/", sb.ID, p),
			})
		}
	}
	if out == nil {
		out = []portInfo{}
	}
	writeJSON(w, 200, out)
}

// --- Volumes ---

func (s *Server) handleVolumes(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		list, err := s.store.ListVolumes()
		if err != nil {
			errResp(w, 500, err.Error())
			return
		}
		if list == nil {
			list = []store.Volume{}
		}
		writeJSON(w, 200, list)
	case http.MethodPost:
		var req struct {
			Name string `json:"name"`
		}
		if err := readJSON(r, &req); err != nil {
			errResp(w, 400, "invalid json: "+err.Error())
			return
		}
		if req.Name == "" {
			errResp(w, 400, "name required")
			return
		}
		if err := s.store.CreateVolume(req.Name); err != nil {
			errResp(w, 500, err.Error())
			return
		}
		vol, _ := s.store.GetVolume(req.Name)
		writeJSON(w, 201, vol)
	default:
		errResp(w, 405, "method not allowed")
	}
}

func (s *Server) handleVolume(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimPrefix(r.URL.Path, "/volumes/")
	if name == "" {
		errResp(w, 400, "missing volume name")
		return
	}
	switch r.Method {
	case http.MethodGet:
		vol, err := s.store.GetVolume(name)
		if err != nil {
			errResp(w, 404, "not found")
			return
		}
		writeJSON(w, 200, vol)
	case http.MethodDelete:
		if err := s.store.DeleteVolume(name); err != nil {
			if strings.Contains(err.Error(), "in use") {
				errResp(w, 409, err.Error())
			} else if strings.Contains(err.Error(), "not found") {
				errResp(w, 404, err.Error())
			} else {
				errResp(w, 500, err.Error())
			}
			return
		}
		writeJSON(w, 200, map[string]string{"status": "deleted"})
	default:
		errResp(w, 405, "method not allowed")
	}
}

// --- HTTP Reverse Proxy (tunneled through Engine) ---

// handleSandboxProxyRoute parses the port from the path and delegates.
// Path format: ":port" or ":port/rest/of/path"
func (s *Server) handleSandboxProxyRoute(w http.ResponseWriter, r *http.Request, sandboxID, portPath string) {
	// Split "4321/some/path" → port=4321, rest="/some/path"
	portStr := portPath
	rest := "/"
	if idx := strings.IndexByte(portPath, '/'); idx >= 0 {
		portStr = portPath[:idx]
		rest = portPath[idx:]
	}
	port, err := strconv.Atoi(portStr)
	if err != nil || port < 1 || port > 65535 {
		errResp(w, 400, "invalid port")
		return
	}

	sb := s.getUserSandbox(w, r, sandboxID)
	if sb == nil {
		return
	}

	if err := s.ensureHot(r.Context(), sb.EngineID); err != nil {
		errResp(w, 500, "wake sandbox: "+err.Error())
		return
	}

	// WebSocket upgrade → tunnel raw bytes
	if websocket.IsWebSocketUpgrade(r) {
		s.handleProxyWS(w, r, sb.EngineID, port, rest)
		return
	}

	// Regular HTTP → tunnel through exec
	s.handleProxyHTTP(w, r, sb.EngineID, port, rest)
}

// handleProxyHTTP tunnels an HTTP request/response through Engine.Tunnel().
func (s *Server) handleProxyHTTP(w http.ResponseWriter, r *http.Request, engineID string, port int, path string) {
	tunnel, err := s.engine.Tunnel(r.Context(), engineID, port)
	if err != nil {
		errResp(w, 502, "tunnel failed: "+err.Error())
		return
	}
	defer tunnel.Close()

	// Rewrite the request to target localhost:port inside the sandbox
	outReq := r.Clone(r.Context())
	outReq.URL.Scheme = "http"
	outReq.URL.Host = fmt.Sprintf("localhost:%d", port)
	outReq.URL.Path = path
	outReq.URL.RawQuery = r.URL.RawQuery
	outReq.RequestURI = ""
	outReq.Host = fmt.Sprintf("localhost:%d", port)

	// Write the HTTP request into the tunnel
	if err := outReq.Write(tunnel); err != nil {
		errResp(w, 502, "failed to write request")
		return
	}

	// Read the HTTP response from the tunnel
	resp, err := http.ReadResponse(bufio.NewReader(tunnel), outReq)
	if err != nil {
		errResp(w, 502, "bad gateway: "+err.Error())
		return
	}
	defer resp.Body.Close()

	// Copy response headers and body
	for k, vv := range resp.Header {
		for _, v := range vv {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	io.Copy(w, resp.Body)
}

// handleProxyWS tunnels a WebSocket connection through Engine.Tunnel().
// The browser's WS connects to bhatti; bhatti opens a tunnel into the sandbox
// and relays the raw HTTP upgrade + subsequent WS frames bidirectionally.
func (s *Server) handleProxyWS(w http.ResponseWriter, r *http.Request, engineID string, port int, path string) {
	tunnel, err := s.engine.Tunnel(r.Context(), engineID, port)
	if err != nil {
		errResp(w, 502, "tunnel failed: "+err.Error())
		return
	}
	defer tunnel.Close()

	// Hijack the browser connection to get the raw net.Conn
	hj, ok := w.(http.Hijacker)
	if !ok {
		errResp(w, 500, "server doesn't support hijacking")
		return
	}
	clientConn, clientBuf, err := hj.Hijack()
	if err != nil {
		errResp(w, 500, "hijack failed")
		return
	}
	defer clientConn.Close()

	// Forward the original upgrade request into the tunnel
	outReq := r.Clone(r.Context())
	outReq.URL.Scheme = "http"
	outReq.URL.Host = fmt.Sprintf("localhost:%d", port)
	outReq.URL.Path = path
	outReq.URL.RawQuery = r.URL.RawQuery
	outReq.RequestURI = ""
	outReq.Host = fmt.Sprintf("localhost:%d", port)
	outReq.Write(tunnel)

	// Read the upgrade response from the tunnel and forward to browser
	resp, err := http.ReadResponse(bufio.NewReader(tunnel), outReq)
	if err != nil {
		return
	}
	resp.Write(clientBuf)
	clientBuf.Flush()

	// Bidirectional relay — both sides are now speaking WebSocket frames
	done := make(chan struct{})
	go func() {
		io.Copy(tunnel, clientConn)
		close(done)
	}()
	io.Copy(clientConn, tunnel)
	<-done
}

// --- File Operations ---

// FileEngine is optionally implemented by engines that support direct file operations.
type FileEngine interface {
	FileRead(ctx context.Context, id, path string, w io.Writer, opts ...agent.FileReadOpts) (int64, string, error)
	FileWrite(ctx context.Context, id, path, mode string, size int64, r io.Reader) error
	FileStat(ctx context.Context, id, path string) (*proto.FileInfo, error)
	FileList(ctx context.Context, id, path string) ([]proto.FileInfo, error)
}

func (s *Server) handleSandboxFiles(w http.ResponseWriter, r *http.Request, id string) {
	sb := s.getUserSandbox(w, r, id)
	if sb == nil {
		return
	}

	if err := s.ensureHot(r.Context(), sb.EngineID); err != nil {
		errResp(w, 500, "wake sandbox: "+err.Error())
		return
	}

	path := r.URL.Query().Get("path")
	if path == "" {
		errResp(w, 400, "path query parameter required")
		return
	}

	fe, ok := s.engine.(FileEngine)
	if !ok {
		errResp(w, 501, "engine does not support file operations")
		return
	}

	switch r.Method {
	case http.MethodGet:
		if r.URL.Query().Get("ls") == "true" {
			files, err := fe.FileList(r.Context(), sb.EngineID, path)
			if err != nil {
				errResp(w, 500, err.Error())
				return
			}
			writeJSON(w, 200, files)
		} else {
			// Parse truncation parameters
			offset, _ := strconv.Atoi(r.URL.Query().Get("offset"))
			limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
			maxBytes, _ := strconv.Atoi(r.URL.Query().Get("max_bytes"))
			truncating := limit > 0 || maxBytes > 0

			// Stat first to detect errors before writing response body.
			info, err := fe.FileStat(r.Context(), sb.EngineID, path)
			if err != nil {
				errResp(w, 500, err.Error())
				return
			}
			w.Header().Set("Content-Type", "application/octet-stream")
			// Only set Content-Length for full reads — truncated reads
			// produce an unknown final size.
			if !truncating {
				w.Header().Set("Content-Length", fmt.Sprint(info.Size))
			}
			// Report the total file size so the client knows if content
			// was truncated vs the full file.
			w.Header().Set("X-File-Size", fmt.Sprint(info.Size))
			w.WriteHeader(200)

			if truncating {
				fe.FileRead(r.Context(), sb.EngineID, path, w, agent.FileReadOpts{
					Offset:   offset,
					Limit:    limit,
					MaxBytes: maxBytes,
				})
			} else {
				fe.FileRead(r.Context(), sb.EngineID, path, w)
			}
		}
	case http.MethodPut:
		size := r.ContentLength
		// Bug #2: Reject unknown Content-Length (chunked/missing)
		if size < 0 {
			errResp(w, 400, "Content-Length header required for file upload")
			return
		}
		mode := r.URL.Query().Get("mode")
		if mode == "" {
			mode = "0644"
		}
		if err := fe.FileWrite(r.Context(), sb.EngineID, path, mode, size, r.Body); err != nil {
			errResp(w, 500, err.Error())
			return
		}
		writeJSON(w, 200, map[string]string{"status": "ok"})
	case http.MethodHead:
		info, err := fe.FileStat(r.Context(), sb.EngineID, path)
		if err != nil {
			errResp(w, 404, err.Error())
			return
		}
		w.Header().Set("X-File-Size", fmt.Sprint(info.Size))
		w.Header().Set("X-File-Mode", info.Mode)
		w.Header().Set("X-File-IsDir", fmt.Sprint(info.IsDir))
		w.WriteHeader(200)
	default:
		errResp(w, 405, "method not allowed")
	}
}

// --- Sessions ---

func (s *Server) handleSandboxSessions(w http.ResponseWriter, r *http.Request, id string) {
	if r.Method != http.MethodGet {
		errResp(w, 405, "method not allowed")
		return
	}
	sb := s.getUserSandbox(w, r, id)
	if sb == nil {
		return
	}
	if err := s.ensureHot(r.Context(), sb.EngineID); err != nil {
		errResp(w, 500, "wake sandbox: "+err.Error())
		return
	}

	// Use the engine to query sessions via the agent
	type sessionLister interface {
		SessionList(ctx context.Context, id string) ([]proto.SessionInfo, error)
	}
	if sl, ok := s.engine.(sessionLister); ok {
		sessions, err := sl.SessionList(r.Context(), sb.EngineID)
		if err != nil {
			errResp(w, 500, err.Error())
			return
		}
		writeJSON(w, 200, sessions)
		return
	}
	errResp(w, 501, "engine does not support session listing")
}

// errRespInternal logs the real error and returns a generic message with request ID.
// Used for 500 errors to avoid leaking internal paths, IPs, or system details.
func errRespInternal(w http.ResponseWriter, r *http.Request, logMsg string, err error) {
	reqID := RequestIDFromContext(r.Context())
	slog.Error(logMsg, "request_id", reqID, "error", err)
	writeJSON(w, 500, map[string]string{
		"error":      "internal error",
		"request_id": reqID,
	})
}

// encryptSecret encrypts a plaintext secret using the age key in dataDir.
func (s *Server) encryptSecret(plaintext []byte) ([]byte, error) {
	if s.dataDir == "" {
		// No dataDir configured (e.g., tests) — store plaintext
		return plaintext, nil
	}
	keyPath := filepath.Join(s.dataDir, "age.key")
	_, recipient, err := secrets.EnsureKey(keyPath)
	if err != nil {
		return nil, fmt.Errorf("load age key: %w", err)
	}
	return secrets.Encrypt(plaintext, recipient)
}

// decryptSecret decrypts a ciphertext secret using the age key in dataDir.
func (s *Server) decryptSecret(ciphertext []byte) ([]byte, error) {
	if s.dataDir == "" {
		// No dataDir configured (e.g., tests) — data is plaintext
		return ciphertext, nil
	}
	keyPath := filepath.Join(s.dataDir, "age.key")
	identity, _, err := secrets.EnsureKey(keyPath)
	if err != nil {
		return nil, fmt.Errorf("load age key: %w", err)
	}
	return secrets.Decrypt(ciphertext, identity)
}

func genID() string {
	b := make([]byte, 8)
	rand.Read(b)
	return hex.EncodeToString(b)
}

var validNameRe = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]{0,62}$`)

func isValidName(name string) bool {
	return validNameRe.MatchString(name)
}

// getUserSandbox is a helper that retrieves a sandbox scoped to the authenticated user.
// Returns nil and writes a 404 error if not found.
func (s *Server) getUserSandbox(w http.ResponseWriter, r *http.Request, id string) *store.Sandbox {
	user := UserFromContext(r.Context())
	sb, err := s.store.GetSandbox(user.ID, id)
	if err != nil {
		errResp(w, 404, "not found")
		return nil
	}
	return sb
}
