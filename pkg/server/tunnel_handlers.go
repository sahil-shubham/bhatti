package server

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/gorilla/websocket"
	"github.com/sahil-shubham/bhatti/pkg/agent"
	"github.com/sahil-shubham/bhatti/pkg/forward"
)

// tunnelSession tracks both ends so Destroy can interrupt a live relay even
// when the engine does not close its guest streams on shutdown.
type tunnelSession struct {
	ws     *websocket.Conn
	guest  io.ReadWriteCloser // guarded by tunnelMu until Destroy takes the session
	cancel context.CancelFunc
}

func (s *Server) handleSandboxTunnel(w http.ResponseWriter, r *http.Request, id string) {
	if r.Method != http.MethodGet {
		errResp(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	sb := s.getUserSandbox(w, r, id)
	if sb == nil {
		return
	}

	ports := r.URL.Query()["port"]
	if len(ports) != 1 {
		errResp(w, http.StatusBadRequest, "port required (1..65535)")
		return
	}
	port, err := strconv.Atoi(ports[0])
	if err != nil || port < 1 || port > 65535 {
		errResp(w, http.StatusBadRequest, "port required (1..65535)")
		return
	}

	ws, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		slog.Debug("sandbox.tunnel.upgrade_failed", "sandbox_id", sb.ID, "error", err)
		return
	}
	defer ws.Close()

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	session := &tunnelSession{ws: ws, cancel: cancel}
	if !s.registerTunnel(sb.EngineID, session) {
		return // Destroy began while the WebSocket was upgrading.
	}
	defer s.unregisterTunnel(sb.EngineID, session)
	// Pin before waking: a concurrent thermal tick must not pause the guest
	// between ensureHot and establishing the tunnel.
	s.attachInteractive(sb.EngineID)
	defer s.detachInteractive(sb.EngineID)

	if err := s.ensureHot(ctx, sb.EngineID); err != nil {
		slog.Warn("sandbox.tunnel.wake_failed", "sandbox_id", sb.ID, "error", err)
		ws.WriteControl(websocket.CloseMessage,
			websocket.FormatCloseMessage(websocket.CloseInternalServerErr, "wake sandbox failed"),
			time.Now().Add(wsWriteTimeout))
		return
	}

	guest, err := s.engine.Tunnel(ctx, sb.EngineID, port)
	if err != nil {
		code, reason := websocket.CloseInternalServerErr, "open guest tunnel failed"
		if errors.Is(err, agent.ErrPortRefused) {
			code = websocket.CloseTryAgainLater
			reason = fmt.Sprintf("nothing listening on guest port %d", port)
		} else {
			slog.Warn("sandbox.tunnel.open_failed", "sandbox_id", sb.ID, "port", port, "error", err)
		}
		// WriteControl is safe concurrently with other WebSocket writes. The
		// adapter has not started yet, but Destroy may close the socket.
		ws.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(code, reason),
			time.Now().Add(wsWriteTimeout))
		return
	}
	if !s.setTunnelGuest(sb.EngineID, session, guest) {
		guest.Close() // Destroy won the race with opening the guest stream.
		return
	}

	forward.Relay(forward.NewWSConn(ws), guest)
}

func (s *Server) registerTunnel(engineID string, session *tunnelSession) bool {
	s.tunnelMu.Lock()
	defer s.tunnelMu.Unlock()
	if s.tunnelDestroying[engineID] > 0 {
		return false
	}
	if s.tunnels[engineID] == nil {
		s.tunnels[engineID] = make(map[*tunnelSession]struct{})
	}
	s.tunnels[engineID][session] = struct{}{}
	return true
}

func (s *Server) setTunnelGuest(engineID string, session *tunnelSession, guest io.ReadWriteCloser) bool {
	s.tunnelMu.Lock()
	defer s.tunnelMu.Unlock()
	if _, ok := s.tunnels[engineID][session]; !ok {
		return false
	}
	session.guest = guest
	return true
}

func (s *Server) unregisterTunnel(engineID string, session *tunnelSession) {
	s.tunnelMu.Lock()
	defer s.tunnelMu.Unlock()
	delete(s.tunnels[engineID], session)
	if len(s.tunnels[engineID]) == 0 {
		delete(s.tunnels, engineID)
	}
}

func (s *Server) closeTunnels(engineID string) {
	s.tunnelMu.Lock()
	s.tunnelDestroying[engineID]++
	active := s.tunnels[engineID]
	delete(s.tunnels, engineID)
	s.tunnelMu.Unlock()

	for session := range active {
		session.cancel()
		session.ws.Close()
		if session.guest != nil {
			session.guest.Close()
		}
	}
}

func (s *Server) finishTunnelDestroy(engineID string) {
	s.tunnelMu.Lock()
	if s.tunnelDestroying[engineID] <= 1 {
		delete(s.tunnelDestroying, engineID)
	} else {
		s.tunnelDestroying[engineID]--
	}
	s.tunnelMu.Unlock()
}
