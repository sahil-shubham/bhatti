package server

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sahil-shubham/bhatti/pkg/backup"
	"github.com/sahil-shubham/bhatti/pkg/engine"
	"github.com/sahil-shubham/bhatti/pkg/store"
)

type backupTestEngine struct {
	*mockEngine
	trace      string
	guestModes map[string]string
	beginError map[string]error
	thawError  error
	badThawCtx bool
}

type fakeBackupUncleanStop struct{ reason string }

func (e *fakeBackupUncleanStop) Error() string             { return e.reason }
func (e *fakeBackupUncleanStop) UncleanStopReason() string { return e.reason }

func (e *backupTestEngine) EnsureHot(ctx context.Context, id string) error {
	appendBackupTrace(e.trace, "wake "+id)
	return e.mockEngine.EnsureHot(ctx, id)
}

func (e *backupTestEngine) BeginVolumeBackup(_ context.Context, id, mount string) (func(context.Context) error, string, error) {
	if e.ThermalState(id) != "hot" {
		return nil, "", errors.New("backup started before waking guest")
	}
	if err := e.beginError[id]; err != nil {
		if _, unclean := uncleanStopReason(err); unclean {
			_ = e.mockEngine.Stop(context.Background(), id)
		}
		return nil, "", err
	}
	mode := e.guestModes[id]
	if mode == "sync_only" {
		appendBackupTrace(e.trace, "sync "+id)
		return func(context.Context) error {
			appendBackupTrace(e.trace, "release "+id)
			return nil
		}, mode, nil
	}
	appendBackupTrace(e.trace, "freeze "+id+" "+mount)
	return func(ctx context.Context) error {
		_, bounded := ctx.Deadline()
		if ctx.Err() != nil || !bounded {
			e.badThawCtx = true
		}
		appendBackupTrace(e.trace, "thaw "+id+" "+mount)
		if _, unclean := uncleanStopReason(e.thawError); unclean {
			_ = e.mockEngine.Stop(context.Background(), id)
		}
		return e.thawError
	}, "frozen", nil
}

func (e *backupTestEngine) BeginStoppedVolumeBackup(ctx context.Context, id string) (func(), error) {
	status, err := e.Status(ctx, id)
	if err != nil {
		return nil, err
	}
	if status.Status != "stopped" {
		return nil, errors.New("stopped lease granted to running guest")
	}
	appendBackupTrace(e.trace, "lease "+id)
	return func() { appendBackupTrace(e.trace, "release "+id) }, nil
}

type backupTestBackend struct {
	trace   string
	uploads int
	keys    []string
}

func (b *backupTestBackend) Upload(_ context.Context, key string, r io.ReadSeeker, size int64) error {
	content, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	if int64(len(content)) != size {
		return errors.New("upload size mismatch")
	}
	b.uploads++
	b.keys = append(b.keys, key)
	appendBackupTrace(b.trace, "upload")
	return nil
}
func (*backupTestBackend) Download(context.Context, string) (io.ReadCloser, error) {
	return nil, errors.New("unexpected download")
}
func (*backupTestBackend) List(context.Context, string) ([]backup.Entry, error) {
	return nil, errors.New("unexpected list")
}
func (*backupTestBackend) Delete(context.Context, string) error {
	return errors.New("unexpected delete")
}

func appendBackupTrace(path, event string) {
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0600)
	if err != nil {
		panic(err)
	}
	defer f.Close()
	if _, err := f.WriteString(event + "\n"); err != nil {
		panic(err)
	}
}

func backupTrace(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func setupBackupTest(t *testing.T) (*Server, *backupTestEngine, *backupTestBackend, *store.User, *store.PersistentVolume, string) {
	t.Helper()
	dir := t.TempDir()
	trace := filepath.Join(dir, "trace")
	st, err := store.New(filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	file := filepath.Join(dir, "data.ext4")
	if err := os.WriteFile(file, []byte("ext4 image for backup"), 0600); err != nil {
		t.Fatal(err)
	}
	vol := &store.PersistentVolume{
		ID: "v1", UserID: "usr", Name: "data", SizeMB: 1,
		FilePath: file, Status: "ready", CreatedAt: time.Now(),
	}
	if err := st.CreatePersistentVolume(*vol); err != nil {
		t.Fatal(err)
	}
	backend := &backupTestBackend{trace: trace}
	eng := &backupTestEngine{mockEngine: newMockEngine(), trace: trace, guestModes: make(map[string]string), beginError: make(map[string]error)}
	s := &Server{store: st, engine: eng, backupBackend: backend, interactiveAttach: make(map[string]int)}
	commands := filepath.Join(dir, "commands")
	if err := os.Mkdir(commands, 0700); err != nil {
		t.Fatal(err)
	}
	cpScript := "#!/bin/sh\nprintf 'clone\\n' >> \"$BHATTI_BACKUP_TEST_TRACE\"\n" +
		"if [ \"$BHATTI_BACKUP_FAIL_COPY\" = 1 ]; then exit 7; fi\n" +
		"if [ \"$BHATTI_BACKUP_HOLD_COPY\" = 1 ]; then while [ -z \"$BHATTI_BACKUP_RELEASE_COPY\" ] || [ ! -e \"$BHATTI_BACKUP_RELEASE_COPY\" ]; do /bin/sleep 0.05; done; fi\n" +
		"/bin/cp \"$3\" \"$4\"\n"
	zstdScript := "#!/bin/sh\nprintf 'compress\\n' >> \"$BHATTI_BACKUP_TEST_TRACE\"\n/bin/cp \"$3\" \"$5\"\n"
	for name, script := range map[string]string{"cp": cpScript, "zstd": zstdScript} {
		if err := os.WriteFile(filepath.Join(commands, name), []byte(script), 0700); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", commands+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("BHATTI_BACKUP_TEST_TRACE", trace)
	return s, eng, backend, &store.User{ID: "usr", Name: "owner"}, vol, trace
}

func addBackupTestAttachment(t *testing.T, s *Server, eng *backupTestEngine, sbID, mount string, readOnly bool) string {
	t.Helper()
	info, err := eng.Create(context.Background(), engine.SandboxSpec{Name: sbID})
	if err != nil {
		t.Fatal(err)
	}
	eng.thermal[info.EngineID] = "hot"
	if err := s.store.CreateSandbox(store.Sandbox{
		ID: sbID, Name: sbID, EngineID: info.EngineID,
		Status: "running", CreatedBy: "usr", CreatedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.store.AttachPersistentVolume("usr", "data", sbID, mount, readOnly); err != nil {
		t.Fatal(err)
	}
	return info.EngineID
}

func TestVolumeBackupWakesFreezesClonesThawsThenUploads(t *testing.T) {
	s, eng, backend, user, vol, trace := setupBackupTest(t)
	id := addBackupTestAttachment(t, s, eng, "sb1", "/workspace", false)
	eng.thermal[id] = "warm"
	rec := NewEventRecorder(s.store)
	s.events = rec
	recClosed := false
	defer func() {
		if !recClosed {
			rec.Close()
		}
	}()

	result, err := s.performVolumeBackup(context.Background(), user, vol) // vol has no cached attachments
	if err != nil {
		t.Fatal(err)
	}
	want := "wake " + id + "\nfreeze " + id + " /workspace\nclone\nthaw " + id + " /workspace\ncompress\nupload\n"
	if got := backupTrace(t, trace); got != want {
		t.Fatalf("backup order:\n%s\nwant:\n%s", got, want)
	}
	if result.ConsistencyMode != "frozen" || backend.uploads != 1 || eng.badThawCtx {
		t.Fatalf("backup not thawed safely: record=%+v uploads=%d badThawCtx=%v", result, backend.uploads, eng.badThawCtx)
	}
	stored, err := s.store.GetVolumeBackup("usr", result.ID)
	if err != nil || stored.ConsistencyMode != "frozen" {
		t.Fatalf("record mode: %+v, err=%v", stored, err)
	}
	if s.hasInteractiveAttach(id) {
		t.Fatal("backup left guest pinned after thaw")
	}
	rec.Close()
	recClosed = true
	events, err := s.store.QueryEvents(store.EventFilter{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range events {
		if event.Type == "volume.backup_created" && event.Meta["backup_id"] == result.ID && event.Meta["consistency_mode"] == "frozen" {
			return
		}
	}
	t.Fatalf("backup event lost consistency mode: events=%+v", events)
}

func TestVolumeBackupCanonicalizesStoredGuestMountBeforeFreeze(t *testing.T) {
	s, eng, backend, user, vol, trace := setupBackupTest(t)
	id := addBackupTestAttachment(t, s, eng, "sb1", "/workspace/.", false)
	result, err := s.performVolumeBackup(context.Background(), user, vol)
	if err != nil || result == nil || result.ConsistencyMode != "frozen" {
		t.Fatalf("canonical mount backup failed: result=%+v err=%v", result, err)
	}
	want := "freeze " + id + " /workspace\nclone\nthaw " + id + " /workspace\ncompress\nupload\n"
	if got := backupTrace(t, trace); got != want || backend.uploads != 1 {
		t.Fatalf("noncanonical mount passed to guest freeze: trace=%s uploads=%d", got, backend.uploads)
	}
}

func TestVolumeBackupStoppedAttachmentRemainsStoppedAndDetached(t *testing.T) {
	s, eng, backend, user, vol, trace := setupBackupTest(t)
	id := addBackupTestAttachment(t, s, eng, "sb1", "/workspace", false)
	if err := eng.Stop(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	// Store still says running; only the engine knows the guest is stopped.
	result, err := s.performVolumeBackup(context.Background(), user, vol)
	if err != nil || result == nil || result.ConsistencyMode != "detached" {
		t.Fatalf("stopped attachment must back up as detached: record=%+v err=%v", result, err)
	}
	status, err := eng.Status(context.Background(), id)
	if err != nil || status.Status != "stopped" {
		t.Fatalf("backup woke intentionally stopped sandbox: status=%+v err=%v", status, err)
	}
	if got := backupTrace(t, trace); got != "lease "+id+"\nclone\nrelease "+id+"\ncompress\nupload\n" || backend.uploads != 1 || s.hasInteractiveAttach(id) {
		t.Fatalf("stopped guest lacked clone lease or was woken/pinned: trace=%s uploads=%d", got, backend.uploads)
	}
}

func TestVolumeBackupStoppedAttachmentWithoutLeaseFailsClosed(t *testing.T) {
	s, eng, backend, user, vol, trace := setupBackupTest(t)
	id := addBackupTestAttachment(t, s, eng, "sb1", "/workspace", false)
	if err := eng.Stop(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	s.engine = &legacyBackupTestEngine{mockEngine: eng.mockEngine, trace: trace}
	result, err := s.performVolumeBackup(context.Background(), user, vol)
	if result != nil || err == nil || !strings.Contains(err.Error(), "cannot hold stopped") || backend.uploads != 0 {
		t.Fatalf("stopped attachment without lease copied unsafely: record=%+v err=%v uploads=%d", result, err, backend.uploads)
	}
	if data, readErr := os.ReadFile(trace); readErr == nil && strings.Contains(string(data), "clone\n") {
		t.Fatalf("stopped attachment without lease was cloned: %s", data)
	}
}

func TestVolumeBackupEngineRunningOverridesStaleStoppedStoreStatus(t *testing.T) {
	s, eng, _, user, vol, trace := setupBackupTest(t)
	id := addBackupTestAttachment(t, s, eng, "sb1", "/workspace", false)
	if err := s.store.UpdateSandboxStatus("sb1", "stopped"); err != nil {
		t.Fatal(err)
	}
	result, err := s.performVolumeBackup(context.Background(), user, vol)
	if err != nil || result == nil || result.ConsistencyMode != "frozen" {
		t.Fatalf("running guest with stale stopped row must be quiesced: record=%+v err=%v", result, err)
	}
	if got := backupTrace(t, trace); !strings.Contains(got, "freeze "+id+" /workspace\nclone\nthaw "+id+" /workspace\n") {
		t.Fatalf("stale store status skipped live quiesce: %s", got)
	}
}

func TestVolumeBackupRejectsUnknownAttachedSandboxState(t *testing.T) {
	s, eng, backend, user, vol, trace := setupBackupTest(t)
	id := addBackupTestAttachment(t, s, eng, "sb1", "/workspace", false)
	eng.mu.Lock()
	eng.sandboxes[id].Status = "unknown"
	eng.mu.Unlock()
	result, err := s.performVolumeBackup(context.Background(), user, vol)
	if result != nil || err == nil || !strings.Contains(err.Error(), "not safe for backup") || backend.uploads != 0 {
		t.Fatalf("unknown guest state must fail closed: record=%+v err=%v uploads=%d", result, err, backend.uploads)
	}
	if data, readErr := os.ReadFile(trace); readErr == nil && strings.Contains(string(data), "clone\n") {
		t.Fatalf("unknown guest state allowed live clone: %s", data)
	}
}

func TestVolumeBackupRejectsUnqueryableAttachedSandbox(t *testing.T) {
	s, eng, backend, user, vol, trace := setupBackupTest(t)
	id := addBackupTestAttachment(t, s, eng, "sb1", "/workspace", false)
	if err := eng.Destroy(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	result, err := s.performVolumeBackup(context.Background(), user, vol)
	if result != nil || err == nil || !strings.Contains(err.Error(), "status of attached sandbox") || backend.uploads != 0 {
		t.Fatalf("failed status query must not imply a detached guest: result=%+v err=%v uploads=%d", result, err, backend.uploads)
	}
	if data, readErr := os.ReadFile(trace); readErr == nil && strings.Contains(string(data), "clone\n") {
		t.Fatalf("failed status query allowed a clone: %s", data)
	}
}

func TestVolumeBackupCloningFailureStillThaws(t *testing.T) {
	s, eng, backend, user, vol, trace := setupBackupTest(t)
	id := addBackupTestAttachment(t, s, eng, "sb1", "/workspace", false)
	t.Setenv("BHATTI_BACKUP_FAIL_COPY", "1")
	result, err := s.performVolumeBackup(context.Background(), user, vol)
	if err == nil || result != nil || !strings.Contains(err.Error(), "clone volume") {
		t.Fatalf("copy failure must fail backup: result=%+v err=%v", result, err)
	}
	if got := backupTrace(t, trace); got != "freeze "+id+" /workspace\nclone\nthaw "+id+" /workspace\n" {
		t.Fatalf("thaw must follow failed copy: %s", got)
	}
	list, err := s.store.ListVolumeBackups("usr", "data")
	if err != nil || len(list) != 0 || backend.uploads != 0 {
		t.Fatalf("failed clone was recorded/uploaded: list=%+v uploads=%d err=%v", list, backend.uploads, err)
	}
}

func TestVolumeBackupCanceledDuringCloneStillThaws(t *testing.T) {
	s, eng, backend, user, vol, trace := setupBackupTest(t)
	id := addBackupTestAttachment(t, s, eng, "sb1", "/workspace", false)
	t.Setenv("BHATTI_BACKUP_HOLD_COPY", "1")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	result := make(chan error, 1)
	go func() {
		_, err := s.performVolumeBackup(ctx, user, vol)
		result <- err
	}()
	timer := time.NewTimer(3 * time.Second)
	defer timer.Stop()
	for {
		data, _ := os.ReadFile(trace)
		if strings.Contains(string(data), "clone\n") {
			break
		}
		select {
		case <-timer.C:
			t.Fatal("copy never began")
		case <-time.After(5 * time.Millisecond):
		}
	}
	cancel()
	select {
	case err := <-result:
		if err == nil {
			t.Fatal("canceled copy returned a successful backup")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("canceled copy never returned")
	}
	if got := backupTrace(t, trace); !strings.Contains(got, "thaw "+id+" /workspace\n") || backend.uploads != 0 || eng.badThawCtx {
		t.Fatalf("cancellation lost bounded thaw: trace=%s uploads=%d badThawCtx=%v", got, backend.uploads, eng.badThawCtx)
	}
}

func TestVolumeBackupBlocksNewReadOnlyAttachmentUntilCloneAndThaw(t *testing.T) {
	s, eng, backend, user, vol, trace := setupBackupTest(t)
	id := addBackupTestAttachment(t, s, eng, "sb1", "/workspace", true)
	second, err := eng.Create(context.Background(), engine.SandboxSpec{Name: "sb2"})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.store.CreateSandbox(store.Sandbox{
		ID: "sb2", Name: "sb2", EngineID: second.EngineID,
		Status: "running", CreatedBy: "usr", CreatedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}

	t.Setenv("BHATTI_BACKUP_HOLD_COPY", "1")
	releaseCopy := filepath.Join(t.TempDir(), "continue-copy")
	t.Setenv("BHATTI_BACKUP_RELEASE_COPY", releaseCopy)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	type backupResult struct {
		record *store.VolumeBackup
		err    error
	}
	completed := make(chan backupResult, 1)
	go func() {
		record, err := s.performVolumeBackup(ctx, user, vol)
		completed <- backupResult{record: record, err: err}
	}()
	timer := time.NewTimer(3 * time.Second)
	defer timer.Stop()
	for {
		data, _ := os.ReadFile(trace)
		if strings.Contains(string(data), "clone\n") {
			break
		}
		select {
		case <-timer.C:
			t.Fatal("backup copy did not begin")
		case <-time.After(5 * time.Millisecond):
		}
	}
	started := make(chan struct{})
	attached := make(chan error, 1)
	go func() {
		close(started)
		attached <- s.store.AttachPersistentVolume("usr", "data", "sb2", "/read-b", true)
	}()
	<-started
	select {
	case err := <-attached:
		t.Fatalf("new guest attached to volume while clone was in progress: %v", err)
	case <-time.After(30 * time.Millisecond):
	}
	if err := os.WriteFile(releaseCopy, []byte("go"), 0600); err != nil {
		t.Fatal(err)
	}
	select {
	case result := <-completed:
		if result.err != nil || result.record == nil || result.record.ConsistencyMode != "frozen" {
			t.Fatalf("backup did not finish with frozen mode: result=%+v err=%v", result.record, result.err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("backup did not finish after releasing copy")
	}
	select {
	case err := <-attached:
		if err != nil {
			t.Fatalf("new guest could not attach after clone: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("new guest stayed blocked after clone")
	}
	current, err := s.store.GetPersistentVolume("usr", "data")
	if err != nil || len(current.Attachments) != 2 || backend.uploads != 1 {
		t.Fatalf("attachment/backup result incorrect: volume=%+v uploads=%d err=%v", current, backend.uploads, err)
	}
	if got := backupTrace(t, trace); !strings.Contains(got, "freeze "+id+" /workspace\nclone\nthaw "+id+" /workspace\n") {
		t.Fatalf("original guest was not frozen through clone: %s", got)
	}
}

func TestVolumeBackupThawFailureDoesNotPublish(t *testing.T) {
	s, eng, backend, user, vol, trace := setupBackupTest(t)
	addBackupTestAttachment(t, s, eng, "sb1", "/workspace", false)
	eng.thawError = errors.New("thaw refused")
	result, err := s.performVolumeBackup(context.Background(), user, vol)
	if result != nil || err == nil || !strings.Contains(err.Error(), "thaw refused") {
		t.Fatalf("thaw failure must fail backup: result=%+v err=%v", result, err)
	}
	if backend.uploads != 0 || strings.Contains(backupTrace(t, trace), "upload") {
		t.Fatal("thaw failure uploaded an unconfirmed backup")
	}
	list, err := s.store.ListVolumeBackups("usr", "data")
	if err != nil || len(list) != 0 {
		t.Fatalf("thaw failure recorded a backup: list=%+v err=%v", list, err)
	}
}

func TestVolumeBackupUnconfirmedThawPersistsStoppedSandboxAndEvent(t *testing.T) {
	s, eng, backend, user, vol, trace := setupBackupTest(t)
	id := addBackupTestAttachment(t, s, eng, "sb1", "/workspace", false)
	rec := NewEventRecorder(s.store)
	s.events = rec
	closed := false
	defer func() {
		if !closed {
			rec.Close()
		}
	}()
	eng.thawError = &fakeBackupUncleanStop{reason: "guest thaw ACK lost"}
	result, err := s.performVolumeBackup(context.Background(), user, vol)
	if result != nil || err == nil || !strings.Contains(err.Error(), "guest thaw ACK lost") {
		t.Fatalf("unconfirmed thaw was hidden: result=%+v err=%v", result, err)
	}
	sb, err := s.store.GetSandboxByID("sb1")
	if err != nil || sb.Status != "stopped" || sb.StoppedAt == nil {
		t.Fatalf("powered-off VM persisted as running: sandbox=%+v err=%v", sb, err)
	}
	state, err := eng.Status(context.Background(), id)
	if err != nil || state.Status != "stopped" || backend.uploads != 0 {
		t.Fatalf("unconfirmed thaw left running guest or uploaded: state=%+v uploads=%d err=%v", state, backend.uploads, err)
	}
	if strings.Contains(backupTrace(t, trace), "upload") {
		t.Fatal("unconfirmed thaw uploaded backup")
	}
	rec.Close()
	closed = true
	events, err := s.store.QueryEvents(store.EventFilter{Limit: 10})
	if err != nil || len(events) != 1 || events[0].Type != "sandbox.unclean_stop" || events[0].Meta["reason"] != "guest thaw ACK lost" {
		t.Fatalf("missing unclean-stop event: events=%+v err=%v", events, err)
	}
}

func TestVolumeBackupAmbiguousFreezeAndFailedCleanupPersistsStoppedState(t *testing.T) {
	s, eng, backend, user, vol, trace := setupBackupTest(t)
	id := addBackupTestAttachment(t, s, eng, "sb1", "/workspace", false)
	eng.beginError[id] = &fakeBackupUncleanStop{reason: "freeze ACK lost and thaw unconfirmed"}
	result, err := s.performVolumeBackup(context.Background(), user, vol)
	if result != nil || err == nil || !strings.Contains(err.Error(), "thaw unconfirmed") || backend.uploads != 0 {
		t.Fatalf("ambiguous freeze cleanup was not surfaced: result=%+v err=%v uploads=%d", result, err, backend.uploads)
	}
	sb, err := s.store.GetSandboxByID("sb1")
	if err != nil || sb.Status != "stopped" || sb.StoppedAt == nil {
		t.Fatalf("ambiguous freeze did not persist powered-off VM: sandbox=%+v err=%v", sb, err)
	}
	if data, readErr := os.ReadFile(trace); readErr == nil && strings.Contains(string(data), "clone\n") {
		t.Fatalf("ambiguous freeze copied without quiesce: %s", data)
	}
}

func TestVolumeBackupSecondGuestQuiesceFailureThawsFirstWithoutCopy(t *testing.T) {
	s, eng, backend, user, vol, trace := setupBackupTest(t)
	idA := addBackupTestAttachment(t, s, eng, "sbA", "/read-a", true)
	idB := addBackupTestAttachment(t, s, eng, "sbB", "/read-b", true)
	eng.beginError[idB] = errors.New("agent unavailable")
	result, err := s.performVolumeBackup(context.Background(), user, vol)
	if result != nil || err == nil || !strings.Contains(err.Error(), "agent unavailable") {
		t.Fatalf("second guest failure must abort backup: result=%+v err=%v", result, err)
	}
	if got := backupTrace(t, trace); got != "freeze "+idA+" /read-a\nthaw "+idA+" /read-a\n" {
		t.Fatalf("partial freeze was not cleaned before any copy: %s", got)
	}
	if backend.uploads != 0 || s.hasInteractiveAttach(idA) || s.hasInteractiveAttach(idB) {
		t.Fatal("failed multi-guest quiesce left a published backup or pinned guest")
	}
}

func TestVolumeBackupFreezesEveryReadOnlyGuestBeforeCopy(t *testing.T) {
	s, eng, _, user, vol, trace := setupBackupTest(t)
	idB := addBackupTestAttachment(t, s, eng, "sbB", "/read-b", true)
	idA := addBackupTestAttachment(t, s, eng, "sbA", "/read-a", true)
	result, err := s.performVolumeBackup(context.Background(), user, vol)
	if err != nil {
		t.Fatal(err)
	}
	want := "freeze " + idA + " /read-a\nfreeze " + idB + " /read-b\nclone\n" +
		"thaw " + idB + " /read-b\nthaw " + idA + " /read-a\ncompress\nupload\n"
	if got := backupTrace(t, trace); got != want || result.ConsistencyMode != "frozen" {
		t.Fatalf("each read-only guest must freeze before the copy and thaw afterward: trace=%s mode=%s", got, result.ConsistencyMode)
	}
}

func TestVolumeBackupMultipleReadOnlyGuestsAndSyncOnlyMode(t *testing.T) {
	s, eng, backend, user, vol, trace := setupBackupTest(t)
	idB := addBackupTestAttachment(t, s, eng, "sbB", "/read-b", true)
	idA := addBackupTestAttachment(t, s, eng, "sbA", "/read-a", true)
	eng.guestModes[idB] = "sync_only"
	result, err := s.performVolumeBackup(context.Background(), user, vol)
	if err != nil {
		t.Fatal(err)
	}
	want := "freeze " + idA + " /read-a\nsync " + idB + "\nclone\nrelease " + idB + "\nthaw " + idA + " /read-a\ncompress\nupload\n"
	if got := backupTrace(t, trace); got != want {
		t.Fatalf("both read-only guests must quiesce before clone: got\n%s\nwant\n%s", got, want)
	}
	if result.ConsistencyMode != "sync_only" || backend.uploads != 1 {
		t.Fatalf("least strong mode missing: %+v uploads=%d", result, backend.uploads)
	}
	list, err := s.store.ListVolumeBackups("usr", "data")
	if err != nil || len(list) != 1 || list[0].ConsistencyMode != "sync_only" {
		t.Fatalf("persisted backup misstated quiesce mode: list=%+v err=%v", list, err)
	}
}

func TestVolumeBackupDetachedAndUnsupportedLiveEngine(t *testing.T) {
	s, eng, backend, user, vol, trace := setupBackupTest(t)
	result, err := s.performVolumeBackup(context.Background(), user, vol)
	if err != nil || result == nil || result.ConsistencyMode != "detached" || backend.uploads != 1 {
		t.Fatalf("detached mode: backup=%+v uploads=%d err=%v", result, backend.uploads, err)
	}
	result, err = s.performVolumeBackup(context.Background(), user, vol)
	if err != nil || result == nil || result.ConsistencyMode != "detached" || backend.uploads != 2 {
		t.Fatalf("second detached backup: backup=%+v uploads=%d err=%v", result, backend.uploads, err)
	}
	if backend.keys[0] == backend.keys[1] {
		t.Fatal("backups can overwrite the same S3 key")
	}
	id := addBackupTestAttachment(t, s, eng, "sb1", "/workspace", false)
	s.engine = &legacyBackupTestEngine{mockEngine: eng.mockEngine, trace: trace}
	result, err = s.performVolumeBackup(context.Background(), user, vol)
	if result != nil || !errors.Is(err, engine.ErrNotSupported) || backend.uploads != 2 {
		t.Fatalf("engine without lifecycle lease must refuse live copy: result=%+v err=%v uploads=%d", result, err, backend.uploads)
	}
	if got := backupTrace(t, trace); strings.Count(got, "clone\n") != 2 || strings.Contains(got, "sync "+id) {
		t.Fatalf("unsupported engine copied or synced without a lease: %s", got)
	}
}

type legacyBackupTestEngine struct {
	*mockEngine
	trace string
}

func (e *legacyBackupTestEngine) Exec(ctx context.Context, id string, argv []string) (engine.ExecResult, error) {
	if len(argv) != 1 || argv[0] != "sync" {
		return engine.ExecResult{}, errors.New("unexpected legacy guest command")
	}
	appendBackupTrace(e.trace, "sync "+id)
	return e.mockEngine.Exec(ctx, id, argv)
}
