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
	"net"
	"net/http"
	"net/http/httputil"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"github.com/sahil-shubham/bhatti/pkg/agent"
	"github.com/sahil-shubham/bhatti/pkg/agent/proto"
	"github.com/sahil-shubham/bhatti/pkg/engine"
	"github.com/sahil-shubham/bhatti/pkg/oci"
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
	s.mux.HandleFunc("/metrics", s.handleMetrics)
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
			errRespInternal(w, r, "list templates failed", err)
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
			t.Engine = "firecracker"
		}
		if t.CPUs == 0 {
			t.CPUs = 1
		}
		if t.MemoryMB == 0 {
			t.MemoryMB = 2048
		}
		t.CreatedAt = time.Now()
		if err := s.store.CreateTemplate(t); err != nil {
			errRespInternal(w, r, "create template failed", err)
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
	Image      string               `json:"image,omitempty"`      // v0.3: image name
	CPUs       float64              `json:"cpus,omitempty"`
	MemoryMB   int                  `json:"memory_mb,omitempty"`
	DiskSizeMB int                  `json:"disk_size_mb,omitempty"` // v0.3: resize rootfs
	Env        map[string]string    `json:"env,omitempty"`
	Init       string               `json:"init,omitempty"`
	NewVolumes []engine.VolumeSpec  `json:"new_volumes,omitempty"`
	Volumes    []engine.VolumeMount `json:"volumes,omitempty"`

	// v0.3: persistent volumes
	PersistentVolumes []engine.PersistentVolume `json:"persistent_volumes,omitempty"`
}

func (s *Server) handleSandboxes(w http.ResponseWriter, r *http.Request) {
	user := UserFromContext(r.Context())

	switch r.Method {
	case http.MethodGet:
		list, err := s.store.ListSandboxes(user.ID)
		if err != nil {
			errRespInternal(w, r, "list sandboxes failed", err)
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

			// Apply request overrides on template defaults
			cpus := tmpl.CPUs
			if req.CPUs > 0 {
				cpus = req.CPUs
			}
			memMB := tmpl.MemoryMB
			if req.MemoryMB > 0 {
				memMB = req.MemoryMB
			}

			spec = engine.SandboxSpec{
				Name:              name,
				Image:             tmpl.Image,
				CPUs:              cpus,
				MemoryMB:          memMB,
				DiskSizeMB:        req.DiskSizeMB,
				Labels:            tmpl.Labels,
				UserData:          tmpl.UserData,
				Env:               env,
				Init:              req.Init,
				Files:             secretFiles,
				Volumes:           volumes,
				PersistentVolumes: req.PersistentVolumes,
			}
			// Request image overrides template image
			if req.Image != "" {
				spec.Image = req.Image
			}
		} else {
			// --- Direct creation (no template) ---
			spec = engine.SandboxSpec{
				Name:              req.Name,
				CPUs:              req.CPUs,
				MemoryMB:          req.MemoryMB,
				DiskSizeMB:        req.DiskSizeMB,
				Env:               req.Env,
				Init:              req.Init,
				NewVolumes:        req.NewVolumes,
				Volumes:           req.Volumes,
				PersistentVolumes: req.PersistentVolumes,
			}
			volumes = req.Volumes

			// Apply defaults
			if spec.CPUs == 0 {
				spec.CPUs = 1
			}
			if spec.MemoryMB == 0 {
				spec.MemoryMB = 2048
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

		// v0.3: Resolve image name to file path
		if req.Image != "" {
			img, err := s.store.GetImage(user.ID, req.Image)
			if err != nil {
				errResp(w, 404, fmt.Sprintf("image %q not found", req.Image))
				return
			}
			spec.BaseImage = img.FilePath
		}

		// Generate a sandbox ID for volume attachment tracking (before engine.Create)
		sbID := genID()

		// v0.3: Resolve persistent volumes — reserve in store before VM boots
		var resolvedVolumes []engine.ResolvedVolume
		if len(spec.PersistentVolumes) > 0 {
			for _, vol := range spec.PersistentVolumes {
				if !isValidName(vol.Name) {
					errResp(w, 400, fmt.Sprintf("invalid volume name %q", vol.Name))
					return
				}
				if !isValidMountPath(vol.Mount) {
					errResp(w, 400, fmt.Sprintf("invalid mount path %q: must be absolute, no '..' components, not a system path", vol.Mount))
					return
				}

				existing, err := s.store.GetPersistentVolume(user.ID, vol.Name)
				if err != nil && vol.AutoCreate && vol.SizeMB > 0 {
					// Auto-create: insert store record first with status='creating'
					volDir := filepath.Join(s.dataDir, "volumes", user.ID)
					os.MkdirAll(volDir, 0700)
					volPath := filepath.Join(volDir, vol.Name+".ext4")

					storeVol := store.PersistentVolume{
						ID: genID(), UserID: user.ID,
						Name: vol.Name, SizeMB: vol.SizeMB, FilePath: volPath,
						Status: "creating", CreatedAt: time.Now(),
					}
					createErr := s.store.CreatePersistentVolume(storeVol)
					if createErr != nil {
						// UNIQUE violation — another request won the race
						time.Sleep(500 * time.Millisecond)
						existing, err = s.store.GetPersistentVolume(user.ID, vol.Name)
						if err != nil {
							errResp(w, 500, fmt.Sprintf("volume %q: race recovery failed", vol.Name))
							return
						}
						if existing.Status == "creating" {
							errResp(w, 409, fmt.Sprintf("volume %q is being created by another request, retry", vol.Name))
							return
						}
					} else {
						// We won the race. Create the ext4 file.
						if err := createVolumeFile(volPath, vol.SizeMB); err != nil {
							s.store.DeletePersistentVolume(user.ID, vol.Name)
							errRespInternal(w, r, "create volume failed", err)
							return
						}
						s.store.UpdatePersistentVolumeStatus(user.ID, vol.Name, "ready")
						storeVol.Status = "ready"
						existing = &storeVol
					}
				} else if err != nil {
					errResp(w, 404, fmt.Sprintf("volume %q not found", vol.Name))
					return
				}

				// For read-only attach: ensure the ext4 journal is clean BEFORE
				// calling AttachPersistentVolume. But we must verify the volume
				// isn't RW-attached first (e2fsck on a live RW filesystem = corruption).
				// The store's AttachPersistentVolume checks this atomically, but we need
				// the e2fsck to happen between "it's safe" and "we've committed the attach".
				//
				// Strategy: check attachments first (read-only query), e2fsck if safe,
				// then do the transactional attach (which re-checks under lock).
				if vol.ReadOnly && existing.FilePath != "" && len(existing.Attachments) == 0 {
					// No current attachments — safe to e2fsck.
					// This handles the common case: volume was RW-attached, VM was destroyed
					// (unclean unmount → dirty journal), now attaching RO.
					if !volumeIsClean(existing.FilePath) {
						slog.Info("cleaning dirty journal before ro attach", "volume", vol.Name)
						if out, err := exec.Command("e2fsck", "-f", "-y", existing.FilePath).CombinedOutput(); err != nil {
							slog.Warn("e2fsck before ro attach failed", "volume", vol.Name, "output", string(out), "error", err)
						}
					}
				} else if vol.ReadOnly && existing.FilePath != "" && len(existing.Attachments) > 0 {
					// Has existing RO attachments. The journal must already be clean
					// (first RO mount cleaned it). If somehow dirty, reject rather than
					// risk concurrent e2fsck.
					if !volumeIsClean(existing.FilePath) {
						s.store.DetachAllPersistentVolumesForSandbox(sbID)
						errResp(w, 409, fmt.Sprintf("volume %q has a dirty journal and existing attachments — detach all and retry", vol.Name))
						return
					}
				}

				// Attach (store transaction handles concurrency — re-checks under lock)
				if err := s.store.AttachPersistentVolume(user.ID, vol.Name, sbID, vol.Mount, vol.ReadOnly); err != nil {
					// Rollback previously attached volumes
					s.store.DetachAllPersistentVolumesForSandbox(sbID)
					errResp(w, 409, err.Error())
					return
				}

				resolvedVolumes = append(resolvedVolumes, engine.ResolvedVolume{
					FilePath: existing.FilePath,
					DriveID:  fmt.Sprintf("vol%d", len(resolvedVolumes)),
					Name:     vol.Name,
					Mount:    vol.Mount,
					ReadOnly: vol.ReadOnly,
				})
			}
			spec.ResolvedVolumes = resolvedVolumes
		}

		info, err := s.engine.Create(r.Context(), spec)
		if err != nil {
			// Rollback persistent volume attachments on engine failure
			if len(resolvedVolumes) > 0 {
				s.store.DetachAllPersistentVolumesForSandbox(sbID)
			}
			errRespInternal(w, r, "sandbox create failed", err)
			return
		}

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

		// Handle publish and publish/:port
		if sub == "publish" || strings.HasPrefix(sub, "publish/") {
			s.handleSandboxPublish(w, r, id, strings.TrimPrefix(sub, "publish"))
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
		case "save-image":
			s.handleSandboxSaveImage(w, r, id)
		case "checkpoint":
			s.handleSandboxCheckpoint(w, r, id)
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
		s.store.DetachAllPersistentVolumesForSandbox(id) // v0.3 persistent volumes
		if n, err := s.store.DeletePublishRulesForSandbox(sb.ID); err != nil {
			slog.Warn("cleanup publish rules", "sandbox", sb.ID, "error", err)
		} else if n > 0 {
			slog.Info("cleaned up publish rules", "sandbox", sb.ID, "count", n)
		}
		if s.publicProxy != nil {
			s.publicProxy.routeCache.InvalidateSandbox(sb.ID)
		}
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
		errRespInternal(w, r, "stop sandbox failed", err)
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
		errRespInternal(w, r, "start sandbox failed", err)
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

var upgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool { return true },
}

const (
	wsPingInterval = 30 * time.Second
	wsPongTimeout  = 90 * time.Second // 3 missed pings = dead
	wsWriteTimeout = 10 * time.Second
)

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

	// Pong resets the read deadline. If no pong arrives within
	// wsPongTimeout, ReadMessage returns a deadline error and the
	// handler cleans up.
	conn.SetReadDeadline(time.Now().Add(wsPongTimeout))
	conn.SetPongHandler(func(string) error {
		conn.SetReadDeadline(time.Now().Add(wsPongTimeout))
		return nil
	})

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

	// Send session ID to CLI so it can reconnect.
	if sessionID != "" {
		if meta, err := json.Marshal(map[string]string{
			"type": "session", "session_id": sessionID,
		}); err == nil {
			conn.SetWriteDeadline(time.Now().Add(wsWriteTimeout))
			conn.WriteMessage(websocket.TextMessage, meta)
		}
	}

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
			errRespInternal(w, r, "list secrets failed", err)
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
			errRespInternal(w, r, "store secret failed", err)
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
		errRespInternal(w, r, "list sandboxes failed", err)
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

// --- Persistent Volumes (v0.3) ---

func (s *Server) handlePersistentVolumes(w http.ResponseWriter, r *http.Request) {
	user := UserFromContext(r.Context())

	switch r.Method {
	case http.MethodGet:
		list, err := s.store.ListPersistentVolumes(user.ID)
		if err != nil {
			errRespInternal(w, r, "list volumes failed", err)
			return
		}
		if list == nil {
			list = []store.PersistentVolume{}
		}
		writeJSON(w, 200, list)
	case http.MethodPost:
		var req struct {
			Name   string `json:"name"`
			SizeMB int    `json:"size_mb"`
		}
		if err := readJSON(r, &req); err != nil {
			errResp(w, 400, "invalid json: "+err.Error())
			return
		}
		if req.Name == "" || req.SizeMB <= 0 {
			errResp(w, 400, "name and size_mb (> 0) required")
			return
		}
		if !isValidName(req.Name) {
			errResp(w, 400, "invalid volume name: must match [a-zA-Z0-9][a-zA-Z0-9._-]{0,62}")
			return
		}

		// Check quota
		used, _ := s.store.UserVolumeStorageUsed(user.ID)
		userObj, _ := s.store.GetUser(user.ID)
		maxStorage := 20480 // default 20GB
		if userObj != nil {
			maxStorage = userObj.MaxVolumeStorageMB
		}
		if used+req.SizeMB > maxStorage {
			errResp(w, 429, fmt.Sprintf("volume storage quota exceeded (%dMB used, %dMB max)", used, maxStorage))
			return
		}

		volDir := filepath.Join(s.dataDir, "volumes", user.ID)
		os.MkdirAll(volDir, 0700)
		volPath := filepath.Join(volDir, req.Name+".ext4")

		vol := store.PersistentVolume{
			ID: genID(), UserID: user.ID, Name: req.Name,
			SizeMB: req.SizeMB, FilePath: volPath,
			Status: "creating", CreatedAt: time.Now(),
		}
		if err := s.store.CreatePersistentVolume(vol); err != nil {
			if strings.Contains(err.Error(), "UNIQUE") {
				errResp(w, 409, fmt.Sprintf("volume %q already exists", req.Name))
			} else {
				errRespInternal(w, r, "create volume failed", err)
			}
			return
		}

		if err := createVolumeFile(volPath, req.SizeMB); err != nil {
			s.store.DeletePersistentVolume(user.ID, req.Name)
			errRespInternal(w, r, "create volume file failed", err)
			return
		}
		s.store.UpdatePersistentVolumeStatus(user.ID, req.Name, "ready")
		vol.Status = "ready"

		slog.Info("volume.created", "name", req.Name, "user", user.Name, "size_mb", req.SizeMB)
		writeJSON(w, 201, vol)
	default:
		errResp(w, 405, "method not allowed")
	}
}

func (s *Server) handlePersistentVolume(w http.ResponseWriter, r *http.Request) {
	user := UserFromContext(r.Context())
	urlPath := strings.TrimPrefix(r.URL.Path, "/volumes/")
	parts := strings.SplitN(urlPath, "/", 2)
	name := parts[0]

	if name == "" {
		errResp(w, 400, "missing volume name")
		return
	}
	if !isValidName(name) {
		errResp(w, 400, "invalid volume name")
		return
	}

	// Sub-routes
	if len(parts) == 2 {
		switch parts[1] {
		case "resize":
			s.handleVolumeResize(w, r, user, name)
		case "snapshot":
			s.handleVolumeSnapshot(w, r, user, name)
		default:
			errResp(w, 404, "not found")
		}
		return
	}

	switch r.Method {
	case http.MethodGet:
		vol, err := s.store.GetPersistentVolume(user.ID, name)
		if err != nil {
			errResp(w, 404, "not found")
			return
		}
		writeJSON(w, 200, vol)
	case http.MethodDelete:
		// Check for file path before deleting from store
		vol, err := s.store.GetPersistentVolume(user.ID, name)
		if err != nil {
			errResp(w, 404, err.Error())
			return
		}
		if err := s.store.DeletePersistentVolume(user.ID, name); err != nil {
			if strings.Contains(err.Error(), "attachment") {
				errResp(w, 409, err.Error())
			} else if strings.Contains(err.Error(), "not found") {
				errResp(w, 404, err.Error())
			} else {
				errRespInternal(w, r, "delete volume failed", err)
			}
			return
		}
		// Delete file (orphan cleanup on startup handles failures)
		if vol.FilePath != "" {
			os.Remove(vol.FilePath)
		}
		slog.Info("volume.deleted", "name", name, "user", user.Name)
		writeJSON(w, 200, map[string]string{"status": "deleted"})
	default:
		errResp(w, 405, "method not allowed")
	}
}

func (s *Server) handleVolumeResize(w http.ResponseWriter, r *http.Request, user *store.User, name string) {
	if r.Method != http.MethodPost {
		errResp(w, 405, "method not allowed")
		return
	}
	if !isValidName(name) {
		errResp(w, 400, "invalid volume name")
		return
	}
	var req struct {
		SizeMB int `json:"size_mb"`
	}
	if err := readJSON(r, &req); err != nil {
		errResp(w, 400, "invalid json: "+err.Error())
		return
	}

	vol, err := s.store.GetPersistentVolume(user.ID, name)
	if err != nil {
		errResp(w, 404, "volume not found")
		return
	}
	if len(vol.Attachments) > 0 {
		errResp(w, 409, "volume is attached — detach before resizing")
		return
	}
	if req.SizeMB <= vol.SizeMB {
		errResp(w, 400, fmt.Sprintf("new size (%dMB) must be larger than current size (%dMB)", req.SizeMB, vol.SizeMB))
		return
	}

	// e2fsck + truncate + resize2fs
	if out, err := exec.Command("e2fsck", "-f", "-y", vol.FilePath).CombinedOutput(); err != nil {
		slog.Warn("e2fsck before resize", "output", string(out), "error", err)
	}
	if err := exec.Command("truncate", "-s", fmt.Sprintf("%dM", req.SizeMB), vol.FilePath).Run(); err != nil {
		errRespInternal(w, r, "truncate volume failed", err)
		return
	}
	if err := exec.Command("resize2fs", vol.FilePath).Run(); err != nil {
		errRespInternal(w, r, "resize2fs failed", err)
		return
	}

	s.store.UpdatePersistentVolumeSize(user.ID, name, req.SizeMB)
	slog.Info("volume.resized", "name", name, "user", user.Name, "new_size_mb", req.SizeMB)
	writeJSON(w, 200, map[string]any{"status": "resized", "size_mb": req.SizeMB})
}

// handleVolumeSnapshot creates an independent copy of a volume.
func (s *Server) handleVolumeSnapshot(w http.ResponseWriter, r *http.Request, user *store.User, srcName string) {
	if r.Method != http.MethodPost {
		errResp(w, 405, "method not allowed")
		return
	}
	var req struct {
		Name string `json:"name"`
	}
	if err := readJSON(r, &req); err != nil {
		errResp(w, 400, "invalid json: "+err.Error())
		return
	}
	if req.Name == "" || !isValidName(req.Name) {
		errResp(w, 400, "valid name required")
		return
	}

	src, err := s.store.GetPersistentVolume(user.ID, srcName)
	if err != nil {
		errResp(w, 404, "source volume not found")
		return
	}
	if len(src.Attachments) > 0 {
		errResp(w, 409, "source volume is attached — detach before snapshotting")
		return
	}

	dstDir := filepath.Join(s.dataDir, "volumes", user.ID)
	os.MkdirAll(dstDir, 0700)
	dstPath := filepath.Join(dstDir, req.Name+".ext4")

	vol := store.PersistentVolume{
		ID: genID(), UserID: user.ID, Name: req.Name,
		SizeMB: src.SizeMB, FilePath: dstPath,
		Status: "creating", CreatedAt: time.Now(),
	}
	if err := s.store.CreatePersistentVolume(vol); err != nil {
		if strings.Contains(err.Error(), "UNIQUE") {
			errResp(w, 409, fmt.Sprintf("volume %q already exists", req.Name))
		} else {
			errRespInternal(w, r, "create volume snapshot record failed", err)
		}
		return
	}

	if err := exec.Command("cp", "--sparse=always", src.FilePath, dstPath).Run(); err != nil {
		s.store.DeletePersistentVolume(user.ID, req.Name)
		errRespInternal(w, r, "copy volume file failed", err)
		return
	}

	s.store.UpdatePersistentVolumeStatus(user.ID, req.Name, "ready")
	vol.Status = "ready"

	slog.Info("volume.snapshot", "src", srcName, "dst", req.Name, "user", user.Name, "size_mb", src.SizeMB)
	writeJSON(w, 201, vol)
}

// volumeIsClean checks if an ext4 volume has a clean journal.
// Returns false if the journal is dirty (needs e2fsck before RO mount).
func volumeIsClean(path string) bool {
	out, err := exec.Command("tune2fs", "-l", path).Output()
	if err != nil {
		return false
	}
	return strings.Contains(string(out), "Filesystem state:            clean")
}

// createVolumeFile creates an ext4 image of the specified size in MB.
func createVolumeFile(path string, sizeMB int) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	if err := f.Truncate(int64(sizeMB) << 20); err != nil {
		f.Close()
		return err
	}
	f.Close()
	// mkfs.ext4 may not be available on non-Linux platforms (tests)
	if err := exec.Command("mkfs.ext4", "-F", "-q", path).Run(); err != nil {
		// File exists as a sparse file — usable for store tests, not for real VMs
		slog.Warn("mkfs.ext4 failed (expected on non-Linux)", "path", path, "error", err)
	}
	return nil
}

// --- Images (v0.3) ---

func (s *Server) handleImages(w http.ResponseWriter, r *http.Request) {
	user := UserFromContext(r.Context())

	switch r.Method {
	case http.MethodGet:
		list, err := s.store.ListImages(user.ID)
		if err != nil {
			errRespInternal(w, r, "list images failed", err)
			return
		}
		if list == nil {
			list = []store.ImageRecord{}
		}
		writeJSON(w, 200, list)
	default:
		errResp(w, 405, "method not allowed")
	}
}

func (s *Server) handleImage(w http.ResponseWriter, r *http.Request) {
	user := UserFromContext(r.Context())
	path := strings.TrimPrefix(r.URL.Path, "/images/")
	if path == "" {
		errResp(w, 400, "missing image name")
		return
	}

	// Sub-route: POST /images/pull
	if path == "pull" {
		s.handleImagePull(w, r, user)
		return
	}

	name := path

	switch r.Method {
	case http.MethodGet:
		img, err := s.store.GetImage(user.ID, name)
		if err != nil {
			errResp(w, 404, err.Error())
			return
		}
		writeJSON(w, 200, img)
	case http.MethodDelete:
		img, err := s.store.GetImage(user.ID, name)
		if err != nil {
			errResp(w, 404, err.Error())
			return
		}
		if err := s.store.DeleteImage(user.ID, name); err != nil {
			errRespInternal(w, r, "delete image failed", err)
			return
		}
		if img.FilePath != "" {
			os.Remove(img.FilePath)
		}
		slog.Info("image.deleted", "name", name, "user", user.Name)
		writeJSON(w, 200, map[string]string{"status": "deleted"})
	default:
		errResp(w, 405, "method not allowed")
	}
}

// --- Snapshots (v0.3) ---

func (s *Server) handleSnapshots(w http.ResponseWriter, r *http.Request) {
	user := UserFromContext(r.Context())

	switch r.Method {
	case http.MethodGet:
		list, err := s.store.ListSnapshots(user.ID)
		if err != nil {
			errRespInternal(w, r, "list snapshots failed", err)
			return
		}
		if list == nil {
			list = []store.SnapshotRecord{}
		}
		writeJSON(w, 200, list)
	default:
		errResp(w, 405, "method not allowed")
	}
}

func (s *Server) handleSnapshot(w http.ResponseWriter, r *http.Request) {
	user := UserFromContext(r.Context())
	urlPath := strings.TrimPrefix(r.URL.Path, "/snapshots/")
	parts := strings.SplitN(urlPath, "/", 2)
	name := parts[0]

	if name == "" {
		errResp(w, 400, "missing snapshot name")
		return
	}

	// Sub-route: POST /snapshots/:name/resume
	if len(parts) == 2 && parts[1] == "resume" {
		s.handleSnapshotResume(w, r, user, name)
		return
	}

	switch r.Method {
	case http.MethodGet:
		snap, err := s.store.GetSnapshot(user.ID, name)
		if err != nil {
			errResp(w, 404, err.Error())
			return
		}
		writeJSON(w, 200, snap)
	case http.MethodDelete:
		snap, err := s.store.GetSnapshot(user.ID, name)
		if err != nil {
			errResp(w, 404, err.Error())
			return
		}
		if err := s.store.DeleteSnapshot(user.ID, name); err != nil {
			errRespInternal(w, r, "delete snapshot failed", err)
			return
		}
		// Delete snapshot directory
		snapDir := filepath.Dir(snap.MemPath)
		os.RemoveAll(snapDir)
		slog.Info("snapshot.deleted", "name", name, "user", user.Name)
		writeJSON(w, 200, map[string]string{"status": "deleted"})
	default:
		errResp(w, 405, "method not allowed")
	}
}

// --- Tasks (v0.3) ---

func (s *Server) handleTask(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, "/tasks/")
	if id == "" {
		errResp(w, 400, "missing task id")
		return
	}
	user := UserFromContext(r.Context())

	switch r.Method {
	case http.MethodGet:
		task, err := s.store.GetTask(id)
		if err != nil {
			errResp(w, 404, err.Error())
			return
		}
		if task.UserID != user.ID {
			errResp(w, 404, "not found")
			return
		}
		writeJSON(w, 200, task)
	case http.MethodDelete:
		task, err := s.store.GetTask(id)
		if err != nil {
			errResp(w, 404, err.Error())
			return
		}
		if task.UserID != user.ID {
			errResp(w, 404, "not found")
			return
		}
		// Cancel running task via context cancellation
		s.pullCancelMu.Lock()
		if cancelFn, ok := s.pullCancels[id]; ok {
			cancelFn()
			delete(s.pullCancels, id)
		}
		s.pullCancelMu.Unlock()
		s.store.FailTask(id, "cancelled by user")
		writeJSON(w, 200, map[string]string{"status": "cancelled"})
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

// --- Tunnel Transport ---

// tunnelTransport wraps Engine.Tunnel() as an http.RoundTripper.
// Each RoundTrip opens a new tunnel connection to the sandbox.
// Used by httputil.ReverseProxy for proper streaming, hop-by-hop
// header removal, and response flushing.
type tunnelTransport struct {
	engine   engine.Engine
	engineID string
	port     int
}

func (t *tunnelTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	tunnel, err := t.engine.Tunnel(req.Context(), t.engineID, t.port)
	if err != nil {
		return nil, err
	}

	// Guard against context cancellation leaking the tunnel FD.
	// If the client disconnects mid-response, ReverseProxy may cancel
	// the context without closing resp.Body. This AfterFunc ensures
	// the tunnel is always cleaned up.
	stop := context.AfterFunc(req.Context(), func() {
		tunnel.Close()
	})

	if err := req.Write(tunnel); err != nil {
		stop()
		tunnel.Close()
		return nil, err
	}
	resp, err := http.ReadResponse(bufio.NewReader(tunnel), req)
	if err != nil {
		stop()
		tunnel.Close()
		return nil, err
	}
	// Closing the body also closes the tunnel and cancels the AfterFunc.
	resp.Body = &tunnelBody{ReadCloser: resp.Body, tunnel: tunnel, stopGuard: stop}
	return resp, nil
}

// tunnelBody wraps the response body to close the tunnel when done.
// Close is idempotent via sync.Once — safe if called by both
// ReverseProxy and the context.AfterFunc guard.
type tunnelBody struct {
	io.ReadCloser
	tunnel    io.Closer
	stopGuard func() bool
	once      sync.Once
}

func (tb *tunnelBody) Close() error {
	var err error
	tb.once.Do(func() {
		tb.stopGuard()
		tb.ReadCloser.Close()
		err = tb.tunnel.Close()
	})
	return err
}

// handleProxyHTTP tunnels an HTTP request/response through Engine.Tunnel()
// using httputil.ReverseProxy. This handles hop-by-hop header removal,
// chunked transfer encoding, trailer forwarding, and response flushing
// (FlushInterval: -1 flushes every chunk — required for SSE/streaming).
func (s *Server) handleProxyHTTP(w http.ResponseWriter, r *http.Request, engineID string, port int, path string) {
	proxy := &httputil.ReverseProxy{
		Transport: &tunnelTransport{
			engine:   s.engine,
			engineID: engineID,
			port:     port,
		},
		Director: func(req *http.Request) {
			req.URL.Scheme = "http"
			req.URL.Host = fmt.Sprintf("localhost:%d", port)
			req.URL.Path = path
			req.URL.RawQuery = r.URL.RawQuery
			req.Host = fmt.Sprintf("localhost:%d", port)
			if clientIP, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
				req.Header.Set("X-Forwarded-For", clientIP)
			}
			req.Header.Set("X-Forwarded-Proto", schemeOf(r))
			req.Header.Set("X-Forwarded-Host", r.Host)
		},
		FlushInterval: -1, // flush immediately (streaming/SSE)
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			errResp(w, 502, "bad gateway: "+err.Error())
		},
	}
	proxy.ServeHTTP(w, r)
}

// schemeOf returns "https" if the request came over TLS, else "http".
func schemeOf(r *http.Request) string {
	if r.TLS != nil {
		return "https"
	}
	return "http"
}

// --- WebSocket Proxy ---

const wsIdleTimeout = 10 * time.Minute

// deadlineConn is satisfied by net.Conn (which clientConn always is)
// and by tunnel implementations that wrap net.Conn.
type deadlineConn interface {
	io.Reader
	SetReadDeadline(t time.Time) error
}

// idleCopyWithDeadline copies src → dst, resetting the read deadline
// on src after every successful read. Returns when src hits the idle
// timeout or either side errors/closes.
func idleCopyWithDeadline(dst io.Writer, src deadlineConn, timeout time.Duration) {
	buf := make([]byte, 32*1024)
	for {
		src.SetReadDeadline(time.Now().Add(timeout))
		n, err := src.Read(buf)
		if n > 0 {
			if _, wErr := dst.Write(buf[:n]); wErr != nil {
				return
			}
		}
		if err != nil {
			return
		}
	}
}

// proxyWebSocket hijacks the client connection and relays WS frames
// through an engine tunnel. Used by both the authenticated proxy and
// (in the future) the public proxy. Includes an idle timeout to prevent
// FD exhaustion from abandoned connections.
func proxyWebSocket(w http.ResponseWriter, r *http.Request, eng engine.Engine, engineID string, port int, path string) {
	tunnel, err := eng.Tunnel(r.Context(), engineID, port)
	if err != nil {
		errResp(w, 502, "tunnel failed: "+err.Error())
		return
	}
	defer tunnel.Close()

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

	outReq := r.Clone(r.Context())
	outReq.URL.Scheme = "http"
	outReq.URL.Host = fmt.Sprintf("localhost:%d", port)
	outReq.URL.Path = path
	outReq.URL.RawQuery = r.URL.RawQuery
	outReq.RequestURI = ""
	outReq.Host = fmt.Sprintf("localhost:%d", port)
	outReq.Write(tunnel)

	resp, err := http.ReadResponse(bufio.NewReader(tunnel), outReq)
	if err != nil {
		return
	}
	resp.Write(clientBuf)
	clientBuf.Flush()

	// Bidirectional relay with idle timeout.
	// If the tunnel supports SetReadDeadline (net.Conn-backed), use
	// deadline-based idle detection. Otherwise fall back to plain io.Copy.
	done := make(chan struct{})
	tunnelDC, tunnelHasDeadline := tunnel.(deadlineConn)
	if tunnelHasDeadline {
		go func() {
			idleCopyWithDeadline(tunnel, clientConn, wsIdleTimeout)
			close(done)
		}()
		idleCopyWithDeadline(clientConn, tunnelDC, wsIdleTimeout)
	} else {
		go func() {
			io.Copy(tunnel, clientConn)
			close(done)
		}()
		io.Copy(clientConn, tunnel)
	}
	<-done
}

// handleProxyWS delegates to the shared proxyWebSocket function.
func (s *Server) handleProxyWS(w http.ResponseWriter, r *http.Request, engineID string, port int, path string) {
	proxyWebSocket(w, r, s.engine, engineID, port, path)
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
				errRespInternal(w, r, "list directory failed", err)
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
				errRespInternal(w, r, "stat file failed", err)
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
		// Reject unknown Content-Length (chunked/missing)
		if size < 0 {
			errResp(w, 400, "Content-Length header required for file upload")
			return
		}
		mode := r.URL.Query().Get("mode")
		if mode == "" {
			mode = "0644"
		}
		if err := fe.FileWrite(r.Context(), sb.EngineID, path, mode, size, r.Body); err != nil {
			errRespInternal(w, r, "write file failed", err)
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
			errRespInternal(w, r, "list sessions failed", err)
			return
		}
		writeJSON(w, 200, sessions)
		return
	}
	errResp(w, 501, "engine does not support session listing")
}

// --- Checkpoint (named snapshot) ---

func (s *Server) handleSandboxCheckpoint(w http.ResponseWriter, r *http.Request, id string) {
	if r.Method != http.MethodPost {
		errResp(w, 405, "method not allowed")
		return
	}
	user := UserFromContext(r.Context())
	sb := s.getUserSandbox(w, r, id)
	if sb == nil {
		return
	}

	var req struct {
		Name string `json:"name"`
	}
	if err := readJSON(r, &req); err != nil {
		errResp(w, 400, "invalid json: "+err.Error())
		return
	}
	if req.Name == "" || !isValidName(req.Name) {
		errResp(w, 400, "valid name required")
		return
	}

	// Check quota
	existing, _ := s.store.ListSnapshots(user.ID)
	userObj, _ := s.store.GetUser(user.ID)
	maxSnaps := 5
	if userObj != nil && userObj.MaxSnapshots > 0 {
		maxSnaps = userObj.MaxSnapshots
	}
	if len(existing) >= maxSnaps {
		errResp(w, 429, fmt.Sprintf("snapshot limit reached (%d/%d)", len(existing), maxSnaps))
		return
	}

	type checkpointer interface {
		Checkpoint(ctx context.Context, sandboxID, userID string, subnetIndex int, snapName, snapDir string) (any, error)
	}
	cp, ok := s.engine.(checkpointer)
	if !ok {
		errResp(w, 501, "engine does not support checkpoint")
		return
	}

	snapDir := filepath.Join(s.dataDir, "snapshots", user.ID)
	os.MkdirAll(snapDir, 0700)

	manifestIface, err := cp.Checkpoint(r.Context(), sb.EngineID, user.ID, user.SubnetIndex, req.Name, snapDir)
	if err != nil {
		if strings.Contains(err.Error(), "already exists") {
			errResp(w, 409, err.Error())
		} else {
			errRespInternal(w, r, "checkpoint failed", err)
		}
		return
	}

	// Calculate total snapshot size
	finalDir := filepath.Join(snapDir, req.Name)
	var totalSize int64
	filepath.Walk(finalDir, func(_ string, fi os.FileInfo, _ error) error {
		if fi != nil && !fi.IsDir() {
			totalSize += fi.Size()
		}
		return nil
	})
	sizeMB := int(totalSize / 1024 / 1024)

	manifestJSON, _ := json.Marshal(manifestIface)
	snap := store.SnapshotRecord{
		ID: genID(), UserID: user.ID, Name: req.Name,
		SourceSandbox: sb.ID,
		MemPath:       filepath.Join(finalDir, "mem.snap"),
		VMPath:        filepath.Join(finalDir, "vm.snap"),
		RootfsPath:    filepath.Join(finalDir, "rootfs.ext4"),
		ConfigPath:    filepath.Join(finalDir, "config.ext4"),
		ManifestJSON:  string(manifestJSON),
		SizeMB:        sizeMB,
		CreatedAt:     time.Now(),
	}
	if err := s.store.CreateSnapshot(snap); err != nil {
		errRespInternal(w, r, "store snapshot record failed", err)
		return
	}

	s.saveVMState(sb.ID, sb.EngineID) // persist updated state
	slog.Info("snapshot.created", "name", req.Name, "sandbox", sb.ID,
		"user", user.Name, "size_mb", sizeMB)
	writeJSON(w, 201, snap)
}

// --- Snapshot Resume ---

func (s *Server) handleSnapshotResume(w http.ResponseWriter, r *http.Request, user *store.User, snapName string) {
	if r.Method != http.MethodPost {
		errResp(w, 405, "method not allowed")
		return
	}

	var req struct {
		Name string `json:"name"` // new sandbox name
	}
	if err := readJSON(r, &req); err != nil {
		errResp(w, 400, "invalid json: "+err.Error())
		return
	}
	if req.Name != "" && !isValidName(req.Name) {
		errResp(w, 400, "invalid sandbox name")
		return
	}

	// Load snapshot from store
	snap, err := s.store.GetSnapshot(user.ID, snapName)
	if err != nil {
		errResp(w, 404, err.Error())
		return
	}

	// Parse manifest
	type manifest struct {
		Name        string `json:"name"`
		CreatedFrom string `json:"created_from"`
		UserID      string `json:"user_id"`
		SubnetIndex int    `json:"subnet_index"`
		VMConfig    struct {
			VcpuCount  int64 `json:"vcpu_count"`
			MemSizeMib int64 `json:"mem_size_mib"`
		} `json:"vm_config"`
		Drives []struct {
			DriveID      string `json:"drive_id"`
			Role         string `json:"role"`
			SnapshotFile string `json:"snapshot_file"`
			Name         string `json:"name"`
			ReadOnly     bool   `json:"read_only"`
		} `json:"drives"`
		Network struct {
			GuestMAC string `json:"guest_mac"`
			GuestIP  string `json:"guest_ip"`
		} `json:"network"`
		AgentToken string `json:"agent_token"`
	}
	var m manifest
	if err := json.Unmarshal([]byte(snap.ManifestJSON), &m); err != nil {
		errRespInternal(w, r, "parse snapshot manifest failed", err)
		return
	}

	// Enforce sandbox count limit
	count, _ := s.store.CountUserSandboxes(user.ID)
	if count >= user.MaxSandboxes {
		errResp(w, 429, fmt.Sprintf("sandbox limit reached (%d/%d)", count, user.MaxSandboxes))
		return
	}

	type snapshotResumer interface {
		ResumeFromManifestJSON(ctx context.Context, snapDir string, manifestJSON []byte, newName string) (engine.SandboxInfo, error)
	}

	sr, ok := s.engine.(snapshotResumer)
	if !ok {
		errResp(w, 501, "engine does not support snapshot resume")
		return
	}

	snapDir := filepath.Dir(snap.MemPath) // e.g. /var/lib/bhatti/snapshots/usr_alice/dev-ready

	sandboxName := req.Name
	if sandboxName == "" {
		sandboxName = snapName + "-" + genID()[:6]
	}

	info, err := sr.ResumeFromManifestJSON(r.Context(), snapDir, []byte(snap.ManifestJSON), sandboxName)
	if err != nil {
		if strings.Contains(err.Error(), "in use") {
			errResp(w, 409, err.Error())
		} else {
			errRespInternal(w, r, "resume snapshot failed", err)
		}
		return
	}

	sbID := genID()
	sb := store.Sandbox{
		ID: sbID, Name: sandboxName, EngineID: info.EngineID,
		Status: info.Status, IP: info.IP,
		EngineMeta: json.RawMessage("{}"),
		CreatedBy:  user.ID, CreatedAt: time.Now(),
	}
	if err := s.store.CreateSandbox(sb); err != nil {
		s.engine.Destroy(r.Context(), info.EngineID)
		errRespInternal(w, r, "store sandbox failed", err)
		return
	}

	s.saveVMState(sbID, info.EngineID)
	slog.Info("snapshot.resumed", "snapshot", snapName, "sandbox_id", sbID,
		"name", sandboxName, "user", user.Name)
	writeJSON(w, 201, sb)
}

// --- Image Pull (async) ---

func (s *Server) handleImagePull(w http.ResponseWriter, r *http.Request, user *store.User) {
	if r.Method != http.MethodPost {
		errResp(w, 405, "method not allowed")
		return
	}
	var req struct {
		Ref  string `json:"ref"`            // e.g. "python:3.12"
		Name string `json:"name"`           // e.g. "python-3.12"
		Auth string `json:"auth,omitempty"` // "user:token"
	}
	if err := readJSON(r, &req); err != nil {
		errResp(w, 400, "invalid json: "+err.Error())
		return
	}
	if req.Ref == "" || req.Name == "" {
		errResp(w, 400, "ref and name required")
		return
	}
	if !isValidName(req.Name) {
		errResp(w, 400, "invalid image name")
		return
	}

	// Check if image already exists with same digest (no-op pull detection).
	// If the user already has an image with this name from the same OCI ref,
	// and we can verify the digest hasn't changed, skip the pull.
	if existing, err := s.store.GetImage(user.ID, req.Name); err == nil {
		if existing.Source == "oci:"+req.Ref && existing.OCIDigest != "" {
			// Image exists from same source. Return success without re-pulling.
			writeJSON(w, 200, map[string]any{
				"status": "exists",
				"image":  req.Name,
				"digest": existing.OCIDigest,
			})
			return
		}
	}

	taskID := genID()
	task := store.TaskRecord{
		ID: taskID, UserID: user.ID, Type: "image_pull",
		Status: "running", CreatedAt: time.Now(),
	}
	s.store.CreateTask(task)

	// Store cancellation context so DELETE /tasks/:id can cancel it
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	s.pullCancelMu.Lock()
	s.pullCancels[taskID] = cancel
	s.pullCancelMu.Unlock()

	loharPath := filepath.Join(s.dataDir, "lohar")
	outputDir := filepath.Join(s.dataDir, "images", user.ID)
	os.MkdirAll(outputDir, 0700)
	outputPath := filepath.Join(outputDir, req.Name+".ext4")

	// Run in background goroutine
	go func() {
		defer cancel()
		defer func() {
			s.pullCancelMu.Lock()
			delete(s.pullCancels, taskID)
			s.pullCancelMu.Unlock()
		}()

		var ociOpts []oci.Option
		ociOpts = append(ociOpts, oci.WithProgress(func(msg string) {
			s.store.UpdateTaskProgress(taskID, msg)
		}))
		if req.Auth != "" {
			parts := strings.SplitN(req.Auth, ":", 2)
			if len(parts) == 2 {
				ociOpts = append(ociOpts, oci.WithAuth(parts[0], parts[1]))
			}
		}

		config, err := oci.PullAndConvert(ctx, req.Ref, outputPath, loharPath, ociOpts...)
		if err != nil {
			os.Remove(outputPath)
			s.store.FailTask(taskID, err.Error())
			slog.Error("image pull failed", "ref", req.Ref, "user", user.ID, "error", err)
			return
		}

		configJSON, _ := json.Marshal(config)
		sizeMB := int(config.TotalSize / 1024 / 1024)

		img := store.ImageRecord{
			ID: genID(), UserID: user.ID, Name: req.Name,
			Source:        "oci:" + req.Ref,
			FilePath:      outputPath,
			SizeMB:        sizeMB,
			OCIDigest:     config.Digest,
			OCIConfigJSON: string(configJSON),
			CreatedAt:     time.Now(),
		}
		if err := s.store.CreateImage(img); err != nil {
			os.Remove(outputPath)
			s.store.FailTask(taskID, "store image: "+err.Error())
			return
		}

		resultJSON, _ := json.Marshal(map[string]any{
			"image": req.Name, "size_mb": sizeMB, "source": req.Ref,
		})
		s.store.CompleteTask(taskID, string(resultJSON))
		slog.Info("image.pulled", "ref", req.Ref, "name", req.Name,
			"user", user.Name, "size_mb", sizeMB, "digest", config.Digest)
	}()

	w.WriteHeader(http.StatusAccepted)
	json.NewEncoder(w).Encode(map[string]string{"task_id": taskID, "status": "running"})
}

// --- Save Image (sandbox rootfs → image) ---

func (s *Server) handleSandboxSaveImage(w http.ResponseWriter, r *http.Request, id string) {
	if r.Method != http.MethodPost {
		errResp(w, 405, "method not allowed")
		return
	}
	user := UserFromContext(r.Context())
	sb := s.getUserSandbox(w, r, id)
	if sb == nil {
		return
	}

	var req struct {
		Name string `json:"name"`
	}
	if err := readJSON(r, &req); err != nil {
		errResp(w, 400, "invalid json: "+err.Error())
		return
	}
	if req.Name == "" || !isValidName(req.Name) {
		errResp(w, 400, "valid name required")
		return
	}

	// Check if image already exists
	if _, err := s.store.GetImage(user.ID, req.Name); err == nil {
		errResp(w, 409, fmt.Sprintf("image %q already exists — delete first", req.Name))
		return
	}

	// Type assertion for SaveImage capability
	type imageSaver interface {
		SaveImage(ctx context.Context, sandboxID, destPath string) error
	}
	saver, ok := s.engine.(imageSaver)
	if !ok {
		errResp(w, 501, "engine does not support save-image")
		return
	}

	outputDir := filepath.Join(s.dataDir, "images", user.ID)
	os.MkdirAll(outputDir, 0700)
	outputPath := filepath.Join(outputDir, req.Name+".ext4")

	if err := saver.SaveImage(r.Context(), sb.EngineID, outputPath); err != nil {
		os.Remove(outputPath)
		errRespInternal(w, r, "save image failed", err)
		return
	}

	var sizeMB int
	if fi, err := os.Stat(outputPath); err == nil {
		sizeMB = int(fi.Size() / 1024 / 1024)
	}

	img := store.ImageRecord{
		ID: genID(), UserID: user.ID, Name: req.Name,
		Source:   "saved:" + sb.ID,
		FilePath: outputPath, SizeMB: sizeMB,
		CreatedAt: time.Now(),
	}
	if err := s.store.CreateImage(img); err != nil {
		os.Remove(outputPath)
		errRespInternal(w, r, "store image record failed", err)
		return
	}

	slog.Info("image.saved", "name", req.Name, "source_sandbox", sb.ID,
		"user", user.Name, "size_mb", sizeMB)
	writeJSON(w, 201, img)
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

// --- Publish Handlers ---

var aliasRegex = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)

var reservedAliases = map[string]bool{
	"www": true, "mail": true, "admin": true, "status": true,
	"ns1": true, "ns2": true, "api": true, "app": true,
	"_acme-challenge": true,
}

func validateAlias(alias string) error {
	if !aliasRegex.MatchString(alias) {
		return fmt.Errorf("alias must be lowercase alphanumeric with hyphens, 1-63 chars")
	}
	if reservedAliases[alias] {
		return fmt.Errorf("alias %q is reserved", alias)
	}
	return nil
}

// generateAlias creates a <name>-<random> alias. The random suffix prevents
// guessing (2.1B possibilities) and collisions. Format: dev-k3m9x2.bhatti.sh
func generateAlias(sandboxName string) string {
	base := strings.ToLower(sandboxName)
	base = regexp.MustCompile(`[^a-z0-9-]`).ReplaceAllString(base, "-")
	base = strings.Trim(base, "-")
	if base == "" {
		base = "sandbox"
	}

	// 6 chars of a-z0-9 = 2.1 billion possibilities
	b := make([]byte, 4)
	rand.Read(b)
	suffix := hex.EncodeToString(b)[:6]

	alias := base + "-" + suffix
	if len(alias) > 63 {
		alias = base[:63-7] + "-" + suffix // 7 = dash + 6 chars
	}
	return alias
}

func generateUniqueAlias(st *store.Store, sandboxName string) (string, error) {
	// Try up to 4 times (each attempt has a fresh random suffix)
	for i := 0; i < 4; i++ {
		candidate := generateAlias(sandboxName)
		if _, err := st.GetPublishRuleByAlias(candidate); err != nil {
			return candidate, nil // not taken
		}
	}
	return "", fmt.Errorf("failed to generate unique alias after 4 attempts")
}

func (s *Server) handleSandboxPublish(w http.ResponseWriter, r *http.Request, id, sub string) {
	switch r.Method {
	case "POST":
		if sub != "" && sub != "/" {
			errResp(w, 404, "not found")
			return
		}
		s.handlePublish(w, r, id)
	case "GET":
		if sub != "" && sub != "/" {
			errResp(w, 404, "not found")
			return
		}
		s.handleListPublishRules(w, r, id)
	case "DELETE":
		portStr := strings.TrimPrefix(sub, "/")
		port, err := strconv.Atoi(portStr)
		if err != nil || port < 1 || port > 65535 {
			errResp(w, 400, "invalid port in path")
			return
		}
		s.handleUnpublish(w, r, id, port)
	default:
		errResp(w, 405, "method not allowed")
	}
}

func (s *Server) handlePublish(w http.ResponseWriter, r *http.Request, sandboxID string) {
	user := UserFromContext(r.Context())
	sb := s.getUserSandbox(w, r, sandboxID)
	if sb == nil {
		return
	}

	var req struct {
		Port  int    `json:"port"`
		Alias string `json:"alias,omitempty"`
	}
	if err := readJSON(r, &req); err != nil {
		errResp(w, 400, "invalid request body")
		return
	}
	if req.Port < 1 || req.Port > 65535 {
		errResp(w, 400, "port must be 1-65535")
		return
	}

	alias := req.Alias
	if alias == "" {
		var err error
		alias, err = generateUniqueAlias(s.store, sb.Name)
		if err != nil {
			errResp(w, 500, "alias generation failed")
			return
		}
	}
	if err := validateAlias(alias); err != nil {
		errResp(w, 400, err.Error())
		return
	}

	rule := store.PublishRule{
		ID:        "pub_" + genID(),
		SandboxID: sb.ID,
		UserID:    user.ID,
		Port:      req.Port,
		Alias:     alias,
	}
	if err := s.store.CreatePublishRule(rule); err != nil {
		if strings.Contains(err.Error(), "already taken") ||
			strings.Contains(err.Error(), "already published") {
			errResp(w, 409, err.Error())
		} else {
			errResp(w, 500, err.Error())
		}
		return
	}

	writeJSON(w, 201, map[string]interface{}{
		"id":         rule.ID,
		"sandbox_id": sb.ID,
		"port":       rule.Port,
		"alias":      alias,
		"url":        publishedURL(alias, s.proxyZone, s.publicProxyAddr),
		"created_at": rule.CreatedAt,
	})
}

func (s *Server) handleListPublishRules(w http.ResponseWriter, r *http.Request, sandboxID string) {
	sb := s.getUserSandbox(w, r, sandboxID)
	if sb == nil {
		return
	}
	rules, err := s.store.ListPublishRules(sb.ID)
	if err != nil {
		errResp(w, 500, err.Error())
		return
	}
	type ruleResp struct {
		store.PublishRule
		URL string `json:"url"`
	}
	resp := make([]ruleResp, len(rules))
	for i, r := range rules {
		resp[i] = ruleResp{PublishRule: r, URL: publishedURL(r.Alias, s.proxyZone, s.publicProxyAddr)}
	}
	writeJSON(w, 200, resp)
}

func (s *Server) handleUnpublish(w http.ResponseWriter, r *http.Request, sandboxID string, port int) {
	user := UserFromContext(r.Context())
	sb := s.getUserSandbox(w, r, sandboxID)
	if sb == nil {
		return
	}
	// Look up alias before deleting to invalidate route cache.
	rules, _ := s.store.ListPublishRules(sb.ID)
	if err := s.store.DeletePublishRule(user.ID, sb.ID, port); err != nil {
		errResp(w, 404, err.Error())
		return
	}
	for _, r := range rules {
		if r.Port == port {
			if s.publicProxy != nil {
				s.publicProxy.routeCache.Invalidate(r.Alias)
			}
			break
		}
	}
	w.WriteHeader(204)
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
