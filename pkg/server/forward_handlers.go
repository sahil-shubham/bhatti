package server

import (
	"context"
	"fmt"
	"net"
	"net/http"

	"github.com/sahil-shubham/bhatti/pkg/forward"
	"github.com/sahil-shubham/bhatti/pkg/store"
)

// handleSandboxForward manages host↔guest TCP forwards (POST start, GET list,
// DELETE stop all). The daemon binds a host port on 127.0.0.1 and bridges each
// connection to the guest port over the engine's vsock Tunnel (pkg/forward).
// Connecting wakes the sandbox (ensureHot as the per-connection hook). Forwards
// are torn down on Destroy.
func (s *Server) handleSandboxForward(w http.ResponseWriter, r *http.Request, id string) {
	sb := s.getUserSandbox(w, r, id)
	if sb == nil {
		return
	}
	switch r.Method {
	case http.MethodPost:
		var req struct {
			GuestPort int `json:"guest_port"`
			HostPort  int `json:"host_port"` // 0 = ephemeral
		}
		if err := readJSON(r, &req); err != nil {
			errResp(w, 400, "invalid json: "+err.Error())
			return
		}
		if req.GuestPort < 1 || req.GuestPort > 65535 {
			errResp(w, 400, "guest_port required (1..65535)")
			return
		}
		engineID := sb.EngineID
		onConnect := func(ctx context.Context) error { return s.ensureHot(ctx, engineID) }
		ln, err := forward.Serve(s.engine, engineID, req.GuestPort,
			fmt.Sprintf("127.0.0.1:%d", req.HostPort), onConnect)
		if err != nil {
			errRespInternal(w, r, "start forward", err)
			return
		}
		s.registerForward(engineID, ln)
		hostPort := ln.Addr().(*net.TCPAddr).Port
		s.RecordEvent(store.Event{
			Type: "forward.start", SandboxID: sb.ID,
			Meta: map[string]any{"guest_port": req.GuestPort, "host_port": hostPort},
		})
		writeJSON(w, 200, map[string]any{
			"guest_port": req.GuestPort,
			"host_port":  hostPort,
			"host_addr":  ln.Addr().String(),
		})
	case http.MethodGet:
		s.forwardMu.Lock()
		lns := s.forwards[sb.EngineID]
		out := make([]map[string]any, 0, len(lns))
		for _, ln := range lns {
			out = append(out, map[string]any{"host_addr": ln.Addr().String()})
		}
		s.forwardMu.Unlock()
		writeJSON(w, 200, out)
	case http.MethodDelete:
		n := s.closeForwards(sb.EngineID)
		writeJSON(w, 200, map[string]any{"closed": n})
	default:
		errResp(w, 405, "method not allowed")
	}
}

func (s *Server) registerForward(engineID string, ln net.Listener) {
	s.forwardMu.Lock()
	s.forwards[engineID] = append(s.forwards[engineID], ln)
	s.forwardMu.Unlock()
}

// closeForwards stops every forward for a sandbox (called on Destroy). Returns
// the number closed.
func (s *Server) closeForwards(engineID string) int {
	s.forwardMu.Lock()
	lns := s.forwards[engineID]
	delete(s.forwards, engineID)
	s.forwardMu.Unlock()
	for _, ln := range lns {
		ln.Close()
	}
	return len(lns)
}
