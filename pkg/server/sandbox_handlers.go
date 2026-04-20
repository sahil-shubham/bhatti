package server

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/sahil-shubham/bhatti/pkg/engine"
	"github.com/sahil-shubham/bhatti/pkg/store"
)

// --- Sandboxes ---

type createSandboxReq struct {
	Name       string               `json:"name"`
	TemplateID string               `json:"template_id,omitempty"`
	Image      string               `json:"image,omitempty"` // v0.3: image name
	CPUs       float64              `json:"cpus,omitempty"`
	MemoryMB   int                  `json:"memory_mb,omitempty"`
	DiskSizeMB int                  `json:"disk_size_mb,omitempty"` // v0.3: resize rootfs
	Env        map[string]string    `json:"env,omitempty"`
	Init       string               `json:"init,omitempty"`
	NewVolumes []engine.VolumeSpec  `json:"new_volumes,omitempty"`
	Volumes    []engine.VolumeMount `json:"volumes,omitempty"`
	KeepHot    bool                 `json:"keep_hot,omitempty"`
	Hugepages  bool                 `json:"hugepages,omitempty"` // 2MB hugepages (faster boot, no Diff snapshots)

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

		// Enrich with thermal state + published URLs
		type enrichedSandbox struct {
			store.Sandbox
			Thermal string   `json:"thermal,omitempty"`
			URLs    []string `json:"urls,omitempty"`
		}

		// Thermal state (read-only, no VM interaction)
		te, hasThermal := s.engine.(ThermalEngine)

		// Published URLs (single query for all user's rules)
		rules, _ := s.store.ListUserPublishRules(user.ID)
		urlsByID := make(map[string][]string)
		for _, r := range rules {
			url := publishedURL(r.Alias, s.proxyZone, s.publicProxyAddr)
			urlsByID[r.SandboxID] = append(urlsByID[r.SandboxID], url)
		}

		enriched := make([]enrichedSandbox, len(list))
		for i, sb := range list {
			enriched[i] = enrichedSandbox{Sandbox: sb}
			if hasThermal && sb.Status == "running" {
				enriched[i].Thermal = te.ThermalState(sb.EngineID)
			}
			if urls, ok := urlsByID[sb.ID]; ok {
				enriched[i].URLs = urls
			}
		}
		writeJSON(w, 200, enriched)
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
				Hugepages:         req.Hugepages,
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
				Hugepages:         req.Hugepages,
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

		// Idempotent create: if a non-destroyed sandbox with this name already
		// exists, return it with 200 instead of 409. This eliminates the
		// TOCTOU race where two concurrent creates both pass a check, both
		// boot VMs, and one wastes ~3.5s. Callers (e.g. karkhana) no longer
		// need list→filter→create dance.
		if spec.Name != "" {
			existing, err := s.store.GetActiveSandboxByName(user.ID, spec.Name)
			if err == nil {
				w.Header().Set("X-Bhatti-Existing", "true")
				writeJSON(w, 200, existing)
				return
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
			KeepHot:    req.KeepHot,
		}
		if err := s.store.CreateSandbox(sb); err != nil {
			// UNIQUE constraint violation → name race. Another concurrent
			// request won the insert. Destroy the VM we just booted and
			// return the winner's sandbox.
			if strings.Contains(err.Error(), "UNIQUE") && spec.Name != "" {
				s.engine.Destroy(r.Context(), info.EngineID)
				if len(resolvedVolumes) > 0 {
					s.store.DetachAllPersistentVolumesForSandbox(sbID)
				}
				existing, lookupErr := s.store.GetActiveSandboxByName(user.ID, spec.Name)
				if lookupErr == nil {
					w.Header().Set("X-Bhatti-Existing", "true")
					writeJSON(w, 200, existing)
					return
				}
			}
			s.engine.Destroy(r.Context(), info.EngineID)
			if len(resolvedVolumes) > 0 {
				s.store.DetachAllPersistentVolumesForSandbox(sbID)
			}
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
		s.RecordEvent(store.Event{
			Type: "sandbox.created", UserID: user.ID, SandboxID: sb.ID,
			Meta: map[string]any{
				"name": sb.Name, "cpus": spec.CPUs, "memory_mb": spec.MemoryMB,
				"image": spec.Image, "template_id": templateID,
				"keep_hot": req.KeepHot,
			},
		})
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

		// Handle exec/ws sub-route for piped sessions
		if sub == "exec/ws" {
			s.handleSandboxExecWS(w, r, id)
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
		case "shell-token":
			s.handleShellToken(w, r, id)
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
		// Kill VM first, then release volume DB locks. Destroy() calls
		// Kill+Wait which guarantees the FC process is dead even if other
		// cleanup fails — safe to release volumes after.
		if err := s.engine.Destroy(r.Context(), sb.EngineID); err != nil {
			slog.Warn("engine destroy failed, releasing volumes anyway",
				"sandbox", sb.ID, "error", err)
		}
		s.store.DetachVolumes(sb.ID)
		s.store.DetachAllPersistentVolumesForSandbox(sb.ID) // v0.3 persistent volumes
		if n, err := s.store.DeletePublishRulesForSandbox(sb.ID); err != nil {
			slog.Warn("cleanup publish rules", "sandbox", sb.ID, "error", err)
		} else if n > 0 {
			slog.Info("cleaned up publish rules", "sandbox", sb.ID, "count", n)
		}
		if s.publicProxy != nil {
			s.publicProxy.routeCache.InvalidateSandbox(sb.ID)
		}
		if err := s.store.DeleteSandbox(user.ID, sb.ID); err != nil {
			errRespInternal(w, r, "delete sandbox failed", err)
			return
		}
		slog.Info("sandbox.destroyed", "sandbox_id", sb.ID, "name", sb.Name, "user", user.Name)
		s.RecordEvent(store.Event{
			Type: "sandbox.destroyed", UserID: user.ID, SandboxID: sb.ID,
			Meta: map[string]any{
				"name":       sb.Name,
				"lifetime_s": int(time.Since(sb.CreatedAt).Seconds()),
			},
		})
		writeJSON(w, 200, map[string]string{"status": "destroyed"})
	case http.MethodPatch:
		sb, err := s.store.GetSandbox(user.ID, id)
		if err != nil {
			errResp(w, 404, "not found")
			return
		}
		var req struct {
			KeepHot *bool `json:"keep_hot"`
		}
		if err := readJSON(r, &req); err != nil {
			errResp(w, 400, "invalid json: "+err.Error())
			return
		}
		if req.KeepHot != nil {
			if err := s.store.UpdateSandboxKeepHot(sb.ID, *req.KeepHot); err != nil {
				errRespInternal(w, r, "update keep_hot failed", err)
				return
			}
			sb.KeepHot = *req.KeepHot
			slog.Info("sandbox.updated", "sandbox_id", sb.ID, "name", sb.Name, "keep_hot", sb.KeepHot, "user", user.Name)
			s.RecordEvent(store.Event{
				Type: "sandbox.updated", UserID: user.ID, SandboxID: sb.ID,
				Meta: map[string]any{"name": sb.Name, "keep_hot": sb.KeepHot},
			})

			// If setting keep_hot=true, wake the sandbox immediately.
			if *req.KeepHot {
				if err := s.ensureHot(r.Context(), sb.EngineID); err != nil {
					errRespInternal(w, r, "wake sandbox failed", err)
					return
				}
				sb.Status = "running"
				s.store.UpdateSandboxStatus(sb.ID, "running")
			}
		}
		writeJSON(w, 200, sb)
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
	s.store.StopSandbox(sb.ID)
	s.saveVMState(sb.ID, sb.EngineID) // persist snapshot paths
	user := UserFromContext(r.Context())
	s.RecordEvent(store.Event{
		Type: "sandbox.stopped", UserID: user.ID, SandboxID: sb.ID,
		Meta: map[string]any{"name": sb.Name, "reason": "api"},
	})
	updated, _ := s.store.GetSandboxByID(sb.ID)
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
	user := UserFromContext(r.Context())
	s.RecordEvent(store.Event{
		Type: "sandbox.started", UserID: user.ID, SandboxID: sb.ID,
		Meta: map[string]any{"name": sb.Name, "reason": "api"},
	})
	updated, _ := s.store.GetSandboxByID(id)
	writeJSON(w, 200, updated)
}
