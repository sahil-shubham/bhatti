package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/gorilla/websocket"
	"github.com/sahil-shubham/bhatti/pkg/agent"
	"github.com/sahil-shubham/bhatti/pkg/agent/proto"
	"github.com/sahil-shubham/bhatti/pkg/engine"
	"github.com/sahil-shubham/bhatti/pkg/oci"
	"github.com/sahil-shubham/bhatti/pkg/secrets"
	"github.com/sahil-shubham/bhatti/pkg/store"
)

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

var upgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool { return true },
}

const (
	wsPingInterval = 30 * time.Second
	wsPongTimeout  = 90 * time.Second // 3 missed pings = dead
	wsWriteTimeout = 10 * time.Second
)

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

	// Sub-route: POST /images/import
	if path == "import" {
		s.handleImageImport(w, r, user)
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
		s.RecordEvent(store.Event{
			Type: "image.deleted", UserID: user.ID,
			Meta: map[string]any{"name": name},
		})
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
		s.RecordEvent(store.Event{
			Type: "snapshot.deleted", UserID: user.ID,
			Meta: map[string]any{"name": name},
		})
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
type FileEngine interface {
	FileRead(ctx context.Context, id, path string, w io.Writer, opts ...agent.FileReadOpts) (int64, string, error)
	FileWrite(ctx context.Context, id, path, mode string, size int64, r io.Reader) error
	FileStat(ctx context.Context, id, path string) (*proto.FileInfo, error)
	FileList(ctx context.Context, id, path string) ([]proto.FileInfo, error)
}

// --- Sessions ---

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
	s.RecordEvent(store.Event{
		Type: "snapshot.created", UserID: user.ID, SandboxID: sb.ID,
		Meta: map[string]any{"name": req.Name, "sandbox": sb.ID, "size_mb": sizeMB},
	})
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
			Mount        string `json:"mount"`
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

	// Create volume_attachments for volumes in the snapshot manifest.
	// Without this, recoverVMs can't find volumes after daemon restart (Bug #1).
	for _, d := range m.Drives {
		if d.Role != "volume" || d.Name == "" {
			continue
		}
		vol, err := s.store.GetPersistentVolume(user.ID, d.Name)
		if err != nil {
			slog.Warn("snapshot resume: volume not found in store",
				"volume", d.Name, "sandbox", sbID)
			continue
		}
		mount := d.Mount
		if mount == "" {
			mount = "/vol-" + d.Name
		}
		if err := s.store.AttachPersistentVolume(
			user.ID, d.Name, sbID, mount, d.ReadOnly,
		); err != nil {
			slog.Warn("snapshot resume: attach volume failed",
				"volume", d.Name, "sandbox", sbID, "error", err)
		}
		_ = vol // used for store lookup
	}

	s.saveVMState(sbID, info.EngineID)
	slog.Info("snapshot.resumed", "snapshot", snapName, "sandbox_id", sbID,
		"name", sandboxName, "user", user.Name)
	s.RecordEvent(store.Event{
		Type: "snapshot.resumed", UserID: user.ID, SandboxID: sbID,
		Meta: map[string]any{"name": snapName, "new_sandbox": sandboxName},
	})
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
			s.RecordEvent(store.Event{
				Type: "image.pull_failed", UserID: user.ID,
				Meta: map[string]any{"ref": req.Ref, "error": err.Error()},
			})
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
		s.RecordEvent(store.Event{
			Type: "image.pulled", UserID: user.ID,
			Meta: map[string]any{"ref": req.Ref, "name": req.Name, "size_mb": sizeMB, "digest": config.Digest},
		})
	}()

	w.WriteHeader(http.StatusAccepted)
	json.NewEncoder(w).Encode(map[string]string{"task_id": taskID, "status": "running"})
}

// --- Import Image (tarball → ext4) ---

func (s *Server) handleImageImport(w http.ResponseWriter, r *http.Request, user *store.User) {
	if r.Method != http.MethodPost {
		errResp(w, 405, "method not allowed")
		return
	}

	name := r.URL.Query().Get("name")
	if name == "" || !isValidName(name) {
		errResp(w, 400, "valid name required")
		return
	}

	if _, err := s.store.GetImage(user.ID, name); err == nil {
		errResp(w, 409, fmt.Sprintf("image %q already exists \u2014 delete first", name))
		return
	}

	// Write streamed tarball to temp file (don't hold multi-GB in memory)
	tmpFile, err := os.CreateTemp("", "bhatti-import-*.tar")
	if err != nil {
		errRespInternal(w, r, "create temp file", err)
		return
	}
	defer os.Remove(tmpFile.Name())

	limited := io.LimitReader(r.Body, 10<<30) // 10GB cap
	if _, err := io.Copy(tmpFile, limited); err != nil {
		tmpFile.Close()
		errRespInternal(w, r, "receive tarball", err)
		return
	}
	tmpFile.Close()

	loharPath := filepath.Join(s.dataDir, "lohar")
	outputDir := filepath.Join(s.dataDir, "images", user.ID)
	os.MkdirAll(outputDir, 0700)
	outputPath := filepath.Join(outputDir, name+".ext4")

	config, err := oci.ImportFromTarball(r.Context(), tmpFile.Name(), outputPath, loharPath)
	if err != nil {
		os.Remove(outputPath)
		errResp(w, 400, "import failed: "+err.Error())
		return
	}

	configJSON, _ := json.Marshal(config)
	sizeMB := int(config.TotalSize / 1024 / 1024)

	img := store.ImageRecord{
		ID: genID(), UserID: user.ID, Name: name,
		Source:        "import:" + name,
		FilePath:      outputPath,
		SizeMB:        sizeMB,
		OCIConfigJSON: string(configJSON),
		CreatedAt:     time.Now(),
	}
	if err := s.store.CreateImage(img); err != nil {
		os.Remove(outputPath)
		errRespInternal(w, r, "store image", err)
		return
	}

	slog.Info("image.imported", "name", name, "user", user.Name, "size_mb", sizeMB)
	s.RecordEvent(store.Event{
		Type: "image.imported", UserID: user.ID,
		Meta: map[string]any{"name": name, "size_mb": sizeMB},
	})
	writeJSON(w, 201, map[string]any{"name": name, "size_mb": sizeMB})
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
	s.RecordEvent(store.Event{
		Type: "image.saved", UserID: user.ID, SandboxID: sb.ID,
		Meta: map[string]any{"name": req.Name, "source_sandbox": sb.ID, "size_mb": sizeMB},
	})
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
