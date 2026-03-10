package server

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/gorilla/websocket"

	"github.com/sahilshubham/forge/pkg/engine"
	"github.com/sahilshubham/forge/pkg/store"
)

func (s *Server) routes() {
	s.mux.HandleFunc("/templates", s.handleTemplates)
	s.mux.HandleFunc("/templates/", s.handleTemplate)
	s.mux.HandleFunc("/sandboxes", s.handleSandboxes)
	s.mux.HandleFunc("/sandboxes/", s.handleSandbox)
	s.mux.HandleFunc("/secrets", s.handleSecrets)
	s.mux.HandleFunc("/secrets/", s.handleSecret)
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
	Name       string `json:"name"`
	TemplateID string `json:"template_id"`
}

func (s *Server) handleSandboxes(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		list, err := s.store.ListSandboxes()
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
		if req.TemplateID == "" {
			errResp(w, 400, "template_id required")
			return
		}

		tmpl, err := s.store.GetTemplate(req.TemplateID)
		if err != nil {
			errResp(w, 404, "template not found")
			return
		}

		name := req.Name
		if name == "" {
			name = tmpl.Name + "-" + genID()[:6]
		}

		spec := engine.SandboxSpec{
			Name:     name,
			Image:    tmpl.Image,
			CPUs:     tmpl.CPUs,
			MemoryMB: tmpl.MemoryMB,
			Labels:   tmpl.Labels,
			UserData: tmpl.UserData,
		}

		info, err := s.engine.Create(r.Context(), spec)
		if err != nil {
			errResp(w, 500, "create failed: "+err.Error())
			return
		}

		sb := store.Sandbox{
			ID:         genID(),
			Name:       name,
			TemplateID: tmpl.ID,
			EngineID:   info.EngineID,
			Status:     info.Status,
			IP:         info.IP,
			EngineMeta: json.RawMessage("{}"),
			CreatedAt:  time.Now(),
		}
		if err := s.store.CreateSandbox(sb); err != nil {
			// try to clean up container
			s.engine.Destroy(r.Context(), info.EngineID)
			errResp(w, 500, "store failed: "+err.Error())
			return
		}
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
		switch parts[1] {
		case "stop":
			s.handleSandboxStop(w, r, id)
		case "start":
			s.handleSandboxStart(w, r, id)
		case "exec":
			s.handleSandboxExec(w, r, id)
		case "ws":
			s.handleSandboxWS(w, r, id)
		default:
			errResp(w, 404, "not found")
		}
		return
	}

	switch r.Method {
	case http.MethodGet:
		sb, err := s.store.GetSandbox(id)
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
		sb, err := s.store.GetSandbox(id)
		if err != nil {
			errResp(w, 404, "not found")
			return
		}
		if err := s.engine.Destroy(r.Context(), sb.EngineID); err != nil {
			log.Printf("engine destroy warning: %v", err)
		}
		if err := s.store.DeleteSandbox(id); err != nil {
			errResp(w, 500, err.Error())
			return
		}
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
	sb, err := s.store.GetSandbox(id)
	if err != nil {
		errResp(w, 404, "not found")
		return
	}
	if err := s.engine.Stop(r.Context(), sb.EngineID); err != nil {
		errResp(w, 500, err.Error())
		return
	}
	s.store.StopSandbox(id)
	writeJSON(w, 200, map[string]string{"status": "stopped"})
}

func (s *Server) handleSandboxStart(w http.ResponseWriter, r *http.Request, id string) {
	if r.Method != http.MethodPost {
		errResp(w, 405, "method not allowed")
		return
	}
	sb, err := s.store.GetSandbox(id)
	if err != nil {
		errResp(w, 404, "not found")
		return
	}
	if err := s.engine.Start(r.Context(), sb.EngineID); err != nil {
		errResp(w, 500, err.Error())
		return
	}
	s.store.UpdateSandboxStatus(id, "running")
	writeJSON(w, 200, map[string]string{"status": "running"})
}

type execReq struct {
	Cmd []string `json:"cmd"`
}

func (s *Server) handleSandboxExec(w http.ResponseWriter, r *http.Request, id string) {
	if r.Method != http.MethodPost {
		errResp(w, 405, "method not allowed")
		return
	}
	sb, err := s.store.GetSandbox(id)
	if err != nil {
		errResp(w, 404, "not found")
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
	result, err := s.engine.Exec(r.Context(), sb.EngineID, req.Cmd)
	if err != nil {
		errResp(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, result)
}

var upgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool { return true },
}

func (s *Server) handleSandboxWS(w http.ResponseWriter, r *http.Request, id string) {
	sb, err := s.store.GetSandbox(id)
	if err != nil {
		errResp(w, 404, "not found")
		return
	}

	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("ws upgrade error: %v", err)
		return
	}
	defer conn.Close()

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
					log.Printf("resize error: %v", err)
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
	switch r.Method {
	case http.MethodGet:
		list, err := s.store.ListSecrets()
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
		// For now, store as plaintext metadata — age encryption in secrets package later
		sr := store.SecretRecord{
			Name:      req.Name,
			Path:      fmt.Sprintf("secrets/%s.age", req.Name),
			CreatedAt: time.Now(),
		}
		if err := s.store.CreateSecret(sr); err != nil {
			errResp(w, 500, err.Error())
			return
		}
		writeJSON(w, 201, sr)
	default:
		errResp(w, 405, "method not allowed")
	}
}

func (s *Server) handleSecret(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimPrefix(r.URL.Path, "/secrets/")
	if name == "" {
		errResp(w, 400, "missing secret name")
		return
	}
	switch r.Method {
	case http.MethodDelete:
		if err := s.store.DeleteSecret(name); err != nil {
			errResp(w, 404, err.Error())
			return
		}
		writeJSON(w, 200, map[string]string{"status": "deleted"})
	default:
		errResp(w, 405, "method not allowed")
	}
}

func genID() string {
	b := make([]byte, 8)
	rand.Read(b)
	return hex.EncodeToString(b)
}
