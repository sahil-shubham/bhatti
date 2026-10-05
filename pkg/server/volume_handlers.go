package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/sahil-shubham/bhatti/pkg/engine"
	"github.com/sahil-shubham/bhatti/pkg/store"
)

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
		s.RecordEvent(store.Event{
			Type: "volume.created", UserID: user.ID,
			Meta: map[string]any{"name": req.Name, "size_mb": req.SizeMB},
		})
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

	// Sub-routes: /volumes/{name}/{action}[/{id}]
	if len(parts) == 2 {
		sub := parts[1]
		// Handle /volumes/{name}/backups and /volumes/{name}/backups/{id}
		if sub == "backups" || strings.HasPrefix(sub, "backups/") {
			s.handleVolumeBackups(w, r, user, name, strings.TrimPrefix(sub, "backups"))
			return
		}
		switch sub {
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
		s.RecordEvent(store.Event{
			Type: "volume.deleted", UserID: user.ID,
			Meta: map[string]any{"name": name},
		})
		writeJSON(w, 200, map[string]string{"status": "deleted"})
	default:
		errResp(w, 405, "method not allowed")
	}
}

func (s *Server) handleVolumeBackups(w http.ResponseWriter, r *http.Request, user *store.User, volumeName, subPath string) {
	if s.backupBackend == nil {
		errResp(w, 501, "backup not configured — add backup section to config.yaml")
		return
	}

	// subPath is "" for /backups, "/{id}" for /backups/{id}, "/restore" for /backups/restore
	subPath = strings.TrimPrefix(subPath, "/")

	switch {
	case subPath == "" && r.Method == http.MethodGet:
		// List backups
		backups, err := s.store.ListVolumeBackups(user.ID, volumeName)
		if err != nil {
			errRespInternal(w, r, "list backups failed", err)
			return
		}
		if backups == nil {
			backups = []store.VolumeBackup{}
		}
		writeJSON(w, 200, backups)

	case subPath == "" && r.Method == http.MethodPost:
		// Trigger backup
		vol, err := s.store.GetPersistentVolume(user.ID, volumeName)
		if err != nil {
			errResp(w, 404, "volume not found")
			return
		}
		result, err := s.performVolumeBackup(r.Context(), user, vol)
		if err != nil {
			errRespInternal(w, r, "backup failed", err)
			return
		}
		writeJSON(w, 201, result)

	case subPath == "restore" && r.Method == http.MethodPost:
		// Restore from backup
		var req struct {
			BackupID string `json:"backup_id"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.BackupID == "" {
			errResp(w, 400, "missing backup_id")
			return
		}
		vol, err := s.store.GetPersistentVolume(user.ID, volumeName)
		if err != nil {
			errResp(w, 404, "volume not found")
			return
		}
		// Volume must be detached
		if len(vol.Attachments) > 0 {
			errResp(w, 409, "volume is attached to a sandbox — detach before restoring")
			return
		}
		backup, err := s.store.GetVolumeBackup(user.ID, req.BackupID)
		if err != nil {
			errResp(w, 404, "backup not found")
			return
		}
		if err := s.performVolumeRestore(r.Context(), vol, backup); err != nil {
			errRespInternal(w, r, "restore failed", err)
			return
		}
		s.RecordEvent(store.Event{
			Type: "volume.restored", UserID: user.ID,
			Meta: map[string]any{"name": vol.Name, "backup_id": req.BackupID},
		})
		writeJSON(w, 200, map[string]string{"status": "restored", "backup_id": req.BackupID})

	case subPath != "" && subPath != "restore" && r.Method == http.MethodDelete:
		// Delete a backup
		backupID := subPath
		b, err := s.store.GetVolumeBackup(user.ID, backupID)
		if err != nil {
			errResp(w, 404, "backup not found")
			return
		}
		if err := s.backupBackend.Delete(r.Context(), b.S3Key); err != nil {
			slog.Warn("s3 delete failed", "key", b.S3Key, "error", err)
			// Continue — remove from DB even if S3 delete fails
		}
		s.store.DeleteVolumeBackup(user.ID, backupID)
		writeJSON(w, 200, map[string]string{"status": "deleted"})

	default:
		errResp(w, 405, "method not allowed")
	}
}

// volumeBackupEngine holds the VM lifecycle through the local clone even when
// the guest agent only supports sync, not FIFREEZE.
type volumeBackupEngine interface {
	BeginVolumeBackup(ctx context.Context, id, mount string) (thaw func(context.Context) error, mode string, err error)
}

// A stopped attachment still owns the image. Its lifecycle must be held
// across the clone so an explicit Start cannot race a "detached" backup.
type stoppedVolumeBackupEngine interface {
	BeginStoppedVolumeBackup(ctx context.Context, id string) (release func(), err error)
}

func (s *Server) performVolumeBackup(ctx context.Context, user *store.User, vol *store.PersistentVolume) (*store.VolumeBackup, error) {
	tmpFile, mode, err := s.cloneVolumeForBackup(ctx, vol)
	if err != nil {
		return nil, err
	}
	defer os.Remove(tmpFile)

	// Compression and upload happen after thaw; a guest need not stay frozen
	// for an S3 upload or a slow zstd pass.
	id := genID()
	timestamp := time.Now().UTC().Format(time.RFC3339)
	s3Key := fmt.Sprintf("volumes/%s/%s/%s-%s.ext4.zst", user.ID, vol.Name, timestamp, id)
	compressedFile := tmpFile + ".zst"
	defer os.Remove(compressedFile)
	if err := exec.CommandContext(ctx, "zstd", "-3", "-q", tmpFile, "-o", compressedFile).Run(); err != nil {
		return nil, fmt.Errorf("compress volume: %w", err)
	}
	os.Remove(tmpFile) // free space early

	f, err := os.Open(compressedFile)
	if err != nil {
		return nil, fmt.Errorf("open compressed: %w", err)
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if err := s.backupBackend.Upload(ctx, s3Key, f, fi.Size()); err != nil {
		return nil, fmt.Errorf("upload to s3: %w", err)
	}

	record := store.VolumeBackup{
		ID: id, VolumeName: vol.Name, UserID: user.ID, S3Key: s3Key,
		SizeBytes: fi.Size(), ConsistencyMode: mode, CreatedAt: time.Now().UTC(),
	}
	if err := s.store.CreateVolumeBackup(record); err != nil {
		return nil, fmt.Errorf("record backup: %w", err)
	}
	slog.Info("volume backup created",
		"volume", vol.Name, "backup_id", id,
		"size", fi.Size(), "s3_key", s3Key, "consistency_mode", mode)
	s.RecordEvent(store.Event{
		Type: "volume.backup_created", UserID: user.ID,
		Meta: map[string]any{"name": vol.Name, "backup_id": id, "size_bytes": fi.Size(), "s3_key": s3Key, "consistency_mode": mode},
	})
	return &record, nil
}

// A thaw failure can mean FIFREEZE is still in effect. The krucible engine
// powers off before returning an unclean-stop error; mirror that state in the
// store and event stream so an operator cannot mistake the VM for running.
func (s *Server) reconcileBackupUncleanStop(sb *store.Sandbox, backupErr error) error {
	reason, unclean := uncleanStopReason(backupErr)
	if !unclean {
		return backupErr
	}
	if err := s.store.StopSandbox(sb.ID); err != nil {
		backupErr = errors.Join(backupErr, fmt.Errorf("record stopped sandbox %q after thaw failure: %w", sb.ID, err))
	}
	s.recordUncleanStop(*sb, reason)
	return backupErr
}

// cloneVolumeForBackup holds each attached sandbox through the clone in
// stable order: running guests quiesce; stopped guests cannot start mid-copy.
func (s *Server) cloneVolumeForBackup(ctx context.Context, vol *store.PersistentVolume) (tmpFile, mode string, err error) {
	unlockAttachments := s.store.LockPersistentVolumeAttachments(vol.UserID, vol.Name)
	defer unlockAttachments()
	// The lock covers both this read and the clone, not the S3 upload.
	current, err := s.store.GetPersistentVolume(vol.UserID, vol.Name)
	if err != nil {
		return "", "", fmt.Errorf("load volume attachments: %w", err)
	}
	if current.ID != vol.ID {
		return "", "", fmt.Errorf("volume %q changed before backup", vol.Name)
	}
	vol = current

	attachments := append([]store.VolumeAttachment(nil), vol.Attachments...)
	sort.Slice(attachments, func(i, j int) bool { return attachments[i].SandboxID < attachments[j].SandboxID })
	engineIDs := make([]string, 0, len(attachments))
	sandboxes := make([]*store.Sandbox, 0, len(attachments))
	for _, attachment := range attachments {
		sb, err := s.store.GetSandboxByID(attachment.SandboxID)
		if err != nil {
			return "", "", fmt.Errorf("load attached sandbox %q: %w", attachment.SandboxID, err)
		}
		if sb.CreatedBy != vol.UserID || sb.EngineID == "" {
			return "", "", fmt.Errorf("invalid attached sandbox %q for volume %q", attachment.SandboxID, vol.Name)
		}
		sandboxes = append(sandboxes, sb)
		engineIDs = append(engineIDs, sb.EngineID)
	}
	for _, engineID := range engineIDs {
		s.attachInteractive(engineID)
	}
	defer func() {
		for _, engineID := range engineIDs {
			s.detachInteractive(engineID)
		}
	}()

	running := make([]bool, len(attachments))
	for i, engineID := range engineIDs {
		state, err := s.engine.Status(ctx, engineID)
		if err != nil {
			return "", "", fmt.Errorf("status of attached sandbox %q: %w", engineID, err)
		}
		switch state.Status {
		case "stopped":
			if _, ok := s.engine.(stoppedVolumeBackupEngine); !ok {
				return "", "", fmt.Errorf("engine cannot hold stopped attached sandbox %q during backup", engineID)
			}
		case "running":
			if _, ok := s.engine.(volumeBackupEngine); !ok {
				return "", "", fmt.Errorf("engine cannot hold live attached sandbox %q during backup: %w", engineID, engine.ErrNotSupported)
			}
			running[i] = true
		default:
			return "", "", fmt.Errorf("attached sandbox %q status %q is not safe for backup", engineID, state.Status)
		}
	}
	for i, engineID := range engineIDs {
		if running[i] {
			if err := s.ensureHot(ctx, engineID); err != nil {
				return "", "", fmt.Errorf("wake attached sandbox %q for backup: %w", engineID, err)
			}
		}
	}

	tmp, err := os.CreateTemp(filepath.Dir(vol.FilePath), filepath.Base(vol.FilePath)+".backup-*")
	if err != nil {
		return "", "", fmt.Errorf("create temporary volume clone: %w", err)
	}
	tmpFile = tmp.Name()
	if err := tmp.Close(); err != nil {
		os.Remove(tmpFile)
		return "", "", fmt.Errorf("close temporary volume clone: %w", err)
	}
	clonePath := tmpFile
	defer func() {
		if err != nil {
			os.Remove(clonePath)
		}
	}()

	mode = "detached"
	type backupLease struct {
		release func(context.Context) error
		sandbox *store.Sandbox
	}
	var leases []backupLease
	defer func() {
		for i := len(leases) - 1; i >= 0; i-- {
			cleanupCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			releaseErr := leases[i].release(cleanupCtx)
			cancel()
			if releaseErr != nil {
				releaseErr = s.reconcileBackupUncleanStop(leases[i].sandbox, releaseErr)
				err = errors.Join(err, fmt.Errorf("release attached volume backup lease: %w", releaseErr))
			}
		}
	}()
	for i, attachment := range attachments {
		engineID := engineIDs[i]
		if !running[i] {
			release, leaseErr := s.engine.(stoppedVolumeBackupEngine).BeginStoppedVolumeBackup(ctx, engineID)
			if leaseErr != nil {
				return "", "", fmt.Errorf("hold stopped attached sandbox %q for backup: %w", engineID, leaseErr)
			}
			if release == nil {
				return "", "", fmt.Errorf("engine returned no stopped backup lease for sandbox %q", engineID)
			}
			leases = append(leases, backupLease{
				release: func(context.Context) error { release(); return nil },
				sandbox: sandboxes[i],
			})
			continue
		}
		var thaw func(context.Context) error
		var guestMode string
		thaw, guestMode, err = s.engine.(volumeBackupEngine).BeginVolumeBackup(ctx, engineID, filepath.Clean(attachment.Mount))
		if thaw != nil {
			leases = append(leases, backupLease{release: thaw, sandbox: sandboxes[i]})
		}
		if err != nil {
			err = s.reconcileBackupUncleanStop(sandboxes[i], err)
			return "", "", fmt.Errorf("quiesce sandbox %q volume mount %q: %w", engineID, attachment.Mount, err)
		}
		switch guestMode {
		case "frozen":
			if thaw == nil {
				return "", "", fmt.Errorf("sandbox %q reported frozen without a thaw function", engineID)
			}
			if mode == "detached" {
				mode = "frozen"
			}
		case "sync_only":
			if thaw == nil {
				return "", "", fmt.Errorf("sandbox %q reported sync_only without a lifecycle release", engineID)
			}
			mode = "sync_only"
		default:
			return "", "", fmt.Errorf("sandbox %q returned unknown backup mode %q", engineID, guestMode)
		}
	}
	if err := exec.CommandContext(ctx, "cp", "--reflink=auto", "--sparse=always", vol.FilePath, tmpFile).Run(); err != nil {
		return "", "", fmt.Errorf("clone volume: %w", err)
	}
	return tmpFile, mode, nil
}

func (s *Server) performVolumeRestore(ctx context.Context, vol *store.PersistentVolume, b *store.VolumeBackup) error {
	// Download from S3
	reader, err := s.backupBackend.Download(ctx, b.S3Key)
	if err != nil {
		return fmt.Errorf("download from s3: %w", err)
	}
	defer reader.Close()

	// Write compressed data to temp file
	compressedFile := vol.FilePath + ".restore-tmp.zst"
	defer os.Remove(compressedFile)
	out, err := os.Create(compressedFile)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, reader); err != nil {
		out.Close()
		return fmt.Errorf("download: %w", err)
	}
	out.Close()

	// Decompress over the volume file
	if err := exec.CommandContext(ctx, "zstd", "-d", "-q", "-f", compressedFile, "-o", vol.FilePath).Run(); err != nil {
		return fmt.Errorf("decompress: %w", err)
	}

	slog.Info("volume restored from backup",
		"volume", vol.Name, "backup_id", b.ID)

	return nil
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
	s.RecordEvent(store.Event{
		Type: "volume.resized", UserID: user.ID,
		Meta: map[string]any{"name": name, "old_mb": vol.SizeMB, "new_mb": req.SizeMB},
	})
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
	s.RecordEvent(store.Event{
		Type: "volume.snapshot", UserID: user.ID,
		Meta: map[string]any{"src": srcName, "dst": req.Name, "size_mb": src.SizeMB},
	})
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
