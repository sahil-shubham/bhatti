package server

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/sahil-shubham/bhatti/pkg/agent/proto"
	"github.com/sahil-shubham/bhatti/pkg/engine"
	"github.com/sahil-shubham/bhatti/pkg/store"
)

type execReq struct {
	Cmd        []string `json:"cmd"`
	TimeoutSec int      `json:"timeout_sec,omitempty"` // default 300, max 86400
	Detach     bool     `json:"detach,omitempty"`      // fire-and-forget
	OutputFile string   `json:"output_file,omitempty"` // detach: redirect output to this file
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

	// Apply exec timeout (default 300s, max 86400s / 24h)
	timeout := 300 * time.Second
	if req.TimeoutSec > 0 && req.TimeoutSec <= 86400 {
		timeout = time.Duration(req.TimeoutSec) * time.Second
	}
	execCtx, cancel := context.WithTimeout(r.Context(), timeout)
	defer cancel()

	// Detached exec: fire-and-forget, returns PID immediately
	if req.Detach {
		de, ok := s.engine.(engine.DetachedExecEngine)
		if !ok {
			errResp(w, 501, "engine does not support detached exec")
			return
		}
		outputFile := req.OutputFile
		if outputFile == "" {
			outputFile = fmt.Sprintf("/tmp/bhatti-exec-%s.log", genID()[:8])
		}
		// Use a short timeout — launch should be near-instant
		detachCtx, detachCancel := context.WithTimeout(r.Context(), 30*time.Second)
		defer detachCancel()
		pid, outPath, err := de.ExecDetached(detachCtx, sb.EngineID, req.Cmd, outputFile)
		if err != nil {
			errRespInternal(w, r, "detached exec failed", err)
			return
		}
		writeJSON(w, 200, map[string]any{
			"pid":         pid,
			"output_file": outPath,
			"detached":    true,
		})
		return
	}

	// Streaming NDJSON when requested via Accept header
	if r.Header.Get("Accept") == "application/x-ndjson" {
		s.handleSandboxExecStream(w, r.WithContext(execCtx), sb, req)
		return
	}

	// Buffered JSON (existing behavior)
	result, err := s.engine.Exec(execCtx, sb.EngineID, req.Cmd)
	if err != nil {
		errRespInternal(w, r, "exec failed", err)
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
		conn.SetWriteDeadline(time.Now().Add(wsWriteTimeout))
		conn.WriteMessage(websocket.TextMessage, []byte("wake sandbox: "+err.Error()))
		return
	}

	// Session reattach logic. Use context.Background — r.Context() is
	// tied to the HTTP request and may cancel after WebSocket upgrade.
	sessionParam := r.URL.Query().Get("session")
	forceNew := r.URL.Query().Get("new") == "true"

	var term engine.TerminalConn
	var sessionID string

	sa, canAttach := s.engine.(engine.SessionAttacher)
	sl, canList := s.engine.(interface {
		SessionList(ctx context.Context, id string) ([]proto.SessionInfo, error)
	})

	if sessionParam != "" && canAttach {
		// Explicit session reattach — forcibly detaches any existing client.
		info, t, err := sa.ShellAttach(context.Background(), sb.EngineID, sessionParam, false)
		if err != nil {
			conn.SetWriteDeadline(time.Now().Add(wsWriteTimeout))
			conn.WriteMessage(websocket.TextMessage, []byte("attach error: "+err.Error()))
			return
		}
		term = t
		sessionID = info.SessionID
	} else if !forceNew && canAttach && canList {
		// Auto-reattach: find a detached, running TTY session.
		// Uses ifDetached=true to avoid stealing a session that was
		// attached between the list call and the attach call.
		sessions, err := sl.SessionList(context.Background(), sb.EngineID)
		if err == nil {
			var candidate *proto.SessionInfo
			for i := range sessions {
				si := &sessions[i]
				if si.TTY && si.Running && !si.Attached {
					if candidate == nil || si.CreatedAt > candidate.CreatedAt {
						candidate = si
					}
				}
			}
			if candidate != nil {
				info, t, err := sa.ShellAttach(context.Background(), sb.EngineID, candidate.SessionID, true)
				if err == nil {
					term = t
					sessionID = info.SessionID
				}
				// If attach fails (race: session exited or was attached
				// between list and attach), fall through to create new.
			}
		}
	}

	if term == nil {
		// No session to reattach — create new.
		if ss, ok := s.engine.(engine.ShellSessioner); ok {
			sid, t, err := ss.ShellSession(context.Background(), sb.EngineID)
			if err != nil {
				conn.SetWriteDeadline(time.Now().Add(wsWriteTimeout))
				conn.WriteMessage(websocket.TextMessage, []byte("shell error: "+err.Error()))
				return
			}
			term = t
			sessionID = sid
		} else {
			t, err := s.engine.Shell(context.Background(), sb.EngineID)
			if err != nil {
				conn.SetWriteDeadline(time.Now().Add(wsWriteTimeout))
				conn.WriteMessage(websocket.TextMessage, []byte("shell error: "+err.Error()))
				return
			}
			term = t
		}
	}
	// N.B. defer order matters: conn.Close() (from earlier defer) runs
	// after term.Close(). term.Close() unblocks the term→WS goroutine's
	// Read(); conn.Close() unblocks the WS→term goroutine's ReadMessage().
	defer term.Close()

	// Record shell session event at disconnect (defer runs after wsRelay returns).
	user := UserFromContext(r.Context())
	shellStart := time.Now()
	reattach := sessionParam != ""
	defer func() {
		s.RecordEvent(store.Event{
			Type: "shell.session", UserID: user.ID, SandboxID: sb.ID,
			Meta: map[string]any{
				"sandbox":    sb.Name,
				"session_id": sessionID,
				"reattach":   reattach,
				"duration_s": int(time.Since(shellStart).Seconds()),
			},
		})
	}()

	// Send session ID to CLI so it can reconnect.
	if sessionID != "" {
		if meta, err := json.Marshal(map[string]string{
			"type": "session", "session_id": sessionID,
		}); err == nil {
			conn.SetWriteDeadline(time.Now().Add(wsWriteTimeout))
			conn.WriteMessage(websocket.TextMessage, meta)
		}
	}

	wsRelay(conn, term)
}

// wsRelay bridges a WebSocket connection and a terminal, with
// ping/pong keepalives and resize handling. Blocks until one side
// closes. Caller is responsible for closing conn and term.
func wsRelay(conn *websocket.Conn, term engine.TerminalConn) {
	// Serialize all WebSocket writes through a mutex. gorilla allows
	// one concurrent reader + one concurrent writer, but we have
	// multiple write sources: terminal data, ping ticker, and close frame.
	var wsMu sync.Mutex
	wsWrite := func(msgType int, data []byte) error {
		wsMu.Lock()
		defer wsMu.Unlock()
		conn.SetWriteDeadline(time.Now().Add(wsWriteTimeout))
		return conn.WriteMessage(msgType, data)
	}

	// done signals all goroutines to exit.
	done := make(chan struct{})
	var closeOnce sync.Once
	closeDone := func() { closeOnce.Do(func() { close(done) }) }

	// Pong resets read deadline (peer is alive)
	conn.SetReadDeadline(time.Now().Add(wsPongTimeout))
	conn.SetPongHandler(func(string) error {
		conn.SetReadDeadline(time.Now().Add(wsPongTimeout))
		return nil
	})

	// Ping ticker — keeps the connection alive through proxies.
	go func() {
		ticker := time.NewTicker(wsPingInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				if err := wsWrite(websocket.PingMessage, nil); err != nil {
					closeDone()
					return
				}
			case <-done:
				return
			}
		}
	}()

	// Terminal → WebSocket
	go func() {
		defer closeDone()
		buf := make([]byte, 4096)
		for {
			n, err := term.Read(buf)
			if err != nil {
				wsWrite(websocket.CloseMessage,
					websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""))
				return
			}
			if err := wsWrite(websocket.BinaryMessage, buf[:n]); err != nil {
				return
			}
		}
	}()

	// WebSocket → Terminal
	go func() {
		defer closeDone()
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
					term.Resize(resize.Rows, resize.Cols)
					continue
				}
			}

			if _, err := term.Write(msg); err != nil {
				return
			}
		}
	}()

	<-done
}

// --- Piped Sessions (WebSocket) ---

func (s *Server) handleSandboxExecWS(w http.ResponseWriter, r *http.Request, id string) {
	sb := s.getUserSandbox(w, r, id)
	if sb == nil {
		return
	}

	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		slog.Error("exec ws upgrade failed", "error", err)
		return
	}
	defer conn.Close()

	if err := s.ensureHot(r.Context(), sb.EngineID); err != nil {
		conn.SetWriteDeadline(time.Now().Add(wsWriteTimeout))
		conn.WriteMessage(websocket.TextMessage, []byte(`{"error":"wake sandbox: `+err.Error()+`"}`))
		return
	}

	pe, ok := s.engine.(engine.PipedSessionEngine)
	if !ok {
		conn.SetWriteDeadline(time.Now().Add(wsWriteTimeout))
		conn.WriteMessage(websocket.TextMessage, []byte(`{"error":"engine does not support piped sessions"}`))
		return
	}

	sessionParam := r.URL.Query().Get("session")

	var pipedConn engine.PipedConn
	var sessionID string

	if sessionParam != "" {
		// Reattach to existing session
		info, pc, err := pe.PipedSessionAttach(context.Background(), sb.EngineID, sessionParam, false)
		if err != nil {
			conn.SetWriteDeadline(time.Now().Add(wsWriteTimeout))
			conn.WriteMessage(websocket.TextMessage, []byte(`{"error":"attach: `+err.Error()+`"}`))
			return
		}
		pipedConn = pc
		sessionID = info.SessionID
	} else {
		// Read first WebSocket message as JSON command spec
		conn.SetReadDeadline(time.Now().Add(10 * time.Second))
		_, msg, err := conn.ReadMessage()
		if err != nil {
			return
		}
		conn.SetReadDeadline(time.Time{})

		var spec struct {
			Cmd        []string          `json:"cmd"`
			Env        map[string]string `json:"env,omitempty"`
			MaxIdleSec int               `json:"max_idle_sec,omitempty"`
		}
		if json.Unmarshal(msg, &spec) != nil || len(spec.Cmd) == 0 {
			conn.SetWriteDeadline(time.Now().Add(wsWriteTimeout))
			conn.WriteMessage(websocket.TextMessage, []byte(`{"error":"cmd required"}`))
			return
		}
		maxIdleSec := spec.MaxIdleSec
		if maxIdleSec == 0 {
			maxIdleSec = 3600 // default 1 hour
		}

		info, pc, err := pe.PipedSession(context.Background(), sb.EngineID, spec.Cmd, spec.Env, maxIdleSec)
		if err != nil {
			conn.SetWriteDeadline(time.Now().Add(wsWriteTimeout))
			conn.WriteMessage(websocket.TextMessage, []byte(`{"error":"start: `+err.Error()+`"}`))
			return
		}
		pipedConn = pc
		sessionID = info.SessionID
	}
	defer pipedConn.Close()

	// Send session info
	if meta, err := json.Marshal(map[string]string{
		"type": "session", "session_id": sessionID,
	}); err == nil {
		conn.SetWriteDeadline(time.Now().Add(wsWriteTimeout))
		conn.WriteMessage(websocket.TextMessage, meta)
	}

	// Record event
	user := UserFromContext(r.Context())
	start := time.Now()
	reattach := sessionParam != ""
	defer func() {
		s.RecordEvent(store.Event{
			Type: "exec.piped_session", UserID: user.ID, SandboxID: sb.ID,
			Meta: map[string]any{
				"sandbox":    sb.Name,
				"session_id": sessionID,
				"reattach":   reattach,
				"duration_s": int(time.Since(start).Seconds()),
			},
		})
	}()

	// Bidirectional relay
	pipedWSRelay(conn, pipedConn)
}

// pipedWSRelay bridges a WebSocket and a piped session connection.
// WebSocket text messages → STDIN frames to child.
// STDOUT frames from child → WebSocket text messages.
// EXIT frame → WebSocket close.
func pipedWSRelay(conn *websocket.Conn, pc engine.PipedConn) {
	var wsMu sync.Mutex
	wsWrite := func(msgType int, data []byte) error {
		wsMu.Lock()
		defer wsMu.Unlock()
		conn.SetWriteDeadline(time.Now().Add(wsWriteTimeout))
		return conn.WriteMessage(msgType, data)
	}

	done := make(chan struct{})
	var closeOnce sync.Once
	closeDone := func() { closeOnce.Do(func() { close(done) }) }

	// Ping/pong keepalive
	conn.SetReadDeadline(time.Now().Add(wsPongTimeout))
	conn.SetPongHandler(func(string) error {
		conn.SetReadDeadline(time.Now().Add(wsPongTimeout))
		return nil
	})
	go func() {
		ticker := time.NewTicker(wsPingInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				if err := wsWrite(websocket.PingMessage, nil); err != nil {
					closeDone()
					return
				}
			case <-done:
				return
			}
		}
	}()

	// Piped session → WebSocket (STDOUT/EXIT frames)
	go func() {
		defer closeDone()
		for {
			frameType, payload, err := pc.ReadFrame()
			if err != nil {
				return
			}
			switch frameType {
			case proto.STDOUT, proto.STDERR:
				if err := wsWrite(websocket.TextMessage, payload); err != nil {
					return
				}
			case proto.EXIT:
				code, _ := proto.ParseExitCode(payload)
				exitMsg, _ := json.Marshal(map[string]any{
					"type": "exit", "exit_code": code,
				})
				wsWrite(websocket.TextMessage, exitMsg)
				wsWrite(websocket.CloseMessage,
					websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""))
				return
			case proto.ERROR:
				errMsg, _ := json.Marshal(map[string]any{
					"type": "error", "message": string(payload),
				})
				wsWrite(websocket.TextMessage, errMsg)
				return
			}
		}
	}()

	// WebSocket → piped session (text messages → STDIN frames)
	go func() {
		defer closeDone()
		for {
			_, msg, err := conn.ReadMessage()
			if err != nil {
				return
			}
			if err := pc.WriteStdin(msg); err != nil {
				return
			}
		}
	}()

	<-done
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
			errRespInternal(w, r, "list sessions failed", err)
			return
		}
		writeJSON(w, 200, sessions)
		return
	}
	errResp(w, 501, "engine does not support session listing")
}

// --- Checkpoint (named snapshot) ---
