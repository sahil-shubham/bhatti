package server

import (
	"crypto/rand"
	"encoding/hex"
	"net/http"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/sahil-shubham/bhatti/pkg/engine"
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
		RootfsPath:      strOrEmpty(state, "rootfs_path"),
		SnapMemPath:     strOrEmpty(state, "snap_mem_path"),
		SnapVMPath:      strOrEmpty(state, "snap_vm_path"),
		VsockCID:        intOrZero(state, "vsock_cid"),
		TapDevice:       strOrEmpty(state, "tap_device"),
		GuestIP:         strOrEmpty(state, "guest_ip"),
		GuestMAC:        strOrEmpty(state, "guest_mac"),
		VcpuCount:       floatOrZero(state, "vcpu_count"),
		MemSizeMib:      intOrZero(state, "mem_size_mib"),
		SocketPath:      strOrEmpty(state, "socket_path"),
		VsockPath:       strOrEmpty(state, "vsock_path"),
		AgentToken:      strOrEmpty(state, "agent_token"),
		HasBaseSnapshot: boolOrFalse(state, "has_base_snapshot"),
		FCPathOrigin:    strOrEmpty(state, "fc_path_origin"),
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

func boolOrFalse(m map[string]interface{}, k string) bool {
	switch v := m[k].(type) {
	case bool:
		return v
	case int:
		return v != 0
	case float64:
		return v != 0
	}
	return false
}

func (s *Server) routes() {
	s.mux.HandleFunc("/health", s.handleHealth)
	s.mux.HandleFunc("/templates", s.handleTemplates)
	s.mux.HandleFunc("/templates/", s.handleTemplate)
	s.mux.HandleFunc("/sandboxes", s.handleSandboxes)
	s.mux.HandleFunc("/sandboxes/", s.handleSandbox)
	s.mux.HandleFunc("/secrets", s.handleSecrets)
	s.mux.HandleFunc("/secrets/", s.handleSecret)
	s.mux.HandleFunc("/volumes", s.handlePersistentVolumes)
	s.mux.HandleFunc("/volumes/", s.handlePersistentVolume)
	s.mux.HandleFunc("/images", s.handleImages)
	s.mux.HandleFunc("/images/", s.handleImage)
	s.mux.HandleFunc("/snapshots", s.handleSnapshots)
	s.mux.HandleFunc("/snapshots/", s.handleSnapshot)
	s.mux.HandleFunc("/tasks/", s.handleTask)
	s.mux.HandleFunc("/ports", s.handleAllPorts)
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, map[string]any{
		"status": "ok",
		"uptime": time.Since(s.startTime).Round(time.Second).String(),
	})
}

// --- Templates ---

// --- Ports ---

func genID() string {
	b := make([]byte, 8)
	rand.Read(b)
	return hex.EncodeToString(b)
}

var validNameRe = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]{0,62}$`)

func isValidName(name string) bool {
	return validNameRe.MatchString(name)
}

// isValidMountPath validates that a volume mount path is safe.
// Rejects system paths that would overlay critical guest filesystems.
func isValidMountPath(mount string) bool {
	if mount == "" || mount[0] != '/' {
		return false // must be absolute
	}
	clean := filepath.Clean(mount)
	if strings.Contains(clean, "..") {
		return false
	}
	// Reject system mount points that lohar or the kernel use
	forbidden := []string{"/", "/proc", "/sys", "/dev", "/dev/pts",
		"/run", "/tmp", "/etc", "/bin", "/sbin", "/lib", "/lib64",
		"/usr", "/usr/local/bin", "/boot", "/root"}
	for _, f := range forbidden {
		if clean == f {
			return false
		}
	}
	return true
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
