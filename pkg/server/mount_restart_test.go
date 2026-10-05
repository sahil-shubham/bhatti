package server

import (
	"context"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sahil-shubham/bhatti/pkg/engine"
	"github.com/sahil-shubham/bhatti/pkg/store"
)

// Tracks the mounts as the engine would persist them, rather than trusting
// store metadata (which does not contain the host bind paths).
type mountRecoveryEngine struct {
	*mockEngine
	mounts map[string][]engine.FsMount
}

func (e *mountRecoveryEngine) Create(ctx context.Context, spec engine.SandboxSpec) (engine.SandboxInfo, error) {
	info, err := e.mockEngine.Create(ctx, spec)
	if err == nil {
		e.mounts[info.ID] = append([]engine.FsMount(nil), spec.Mounts...)
	}
	return info, err
}

func (e *mountRecoveryEngine) Mounts(id string) ([]engine.FsMount, bool) {
	mounts, ok := e.mounts[id]
	return append([]engine.FsMount(nil), mounts...), ok
}

func TestMountPolicyRefusesManualRestartAndColdWake(t *testing.T) {
	for _, action := range []string{"start", "wake"} {
		t.Run(action, func(t *testing.T) {
			srv, ts := setup(t)
			eng := &mountRecoveryEngine{mockEngine: srv.engine.(*mockEngine), mounts: make(map[string][]engine.FsMount)}
			srv.engine = eng
			base := t.TempDir()
			allowed := filepath.Join(base, "allowed")
			revoked := filepath.Join(base, "revoked")
			for _, dir := range []string{allowed, revoked} {
				if err := os.Mkdir(dir, 0700); err != nil {
					t.Fatal(err)
				}
			}
			srv.mountRoots = []string{allowed}
			resp := doReq(t, ts, http.MethodPost, "/sandboxes", map[string]any{
				"name": "bound", "mounts": []map[string]string{{"host_path": allowed, "guest_path": "/workspace"}},
			})
			if resp.StatusCode != http.StatusCreated {
				body, _ := io.ReadAll(resp.Body)
				t.Fatalf("initial create: %d %s", resp.StatusCode, body)
			}
			var sb store.Sandbox
			decodeJSON(t, resp, &sb)
			if resp := doReq(t, ts, http.MethodPost, "/sandboxes/"+sb.ID+"/stop", nil); resp.StatusCode != http.StatusOK {
				resp.Body.Close()
				t.Fatalf("stop: %d", resp.StatusCode)
			} else {
				resp.Body.Close()
			}
			srv.mountRoots = []string{revoked}
			switch action {
			case "start":
				resp := doReq(t, ts, http.MethodPost, "/sandboxes/"+sb.ID+"/start", nil)
				defer resp.Body.Close()
				b, _ := io.ReadAll(resp.Body)
				if resp.StatusCode != http.StatusForbidden || !strings.Contains(string(b), "mount_roots") {
					t.Fatalf("restart = %d %s, want mount policy 403", resp.StatusCode, b)
				}
			case "wake":
				if err := srv.EnsureHot(context.Background(), sb.EngineID); err == nil || !strings.Contains(err.Error(), "mount_roots") {
					t.Fatalf("cold wake = %v, want mount policy error", err)
				}
			}
			status, err := eng.Status(context.Background(), sb.EngineID)
			if err != nil || status.Status != "stopped" {
				t.Fatalf("revoked sandbox booted: status=%+v err=%v", status, err)
			}
		})
	}
}

func TestMountPolicyAdoptedRunningSandboxWarnsAndRecordsEvent(t *testing.T) {
	srv, ts := setup(t)
	eng := &mountRecoveryEngine{mockEngine: srv.engine.(*mockEngine), mounts: make(map[string][]engine.FsMount)}
	srv.engine = eng
	root := filepath.Join(t.TempDir(), "allowed")
	if err := os.Mkdir(root, 0700); err != nil {
		t.Fatal(err)
	}
	srv.mountRoots = []string{root}
	resp := doReq(t, ts, http.MethodPost, "/sandboxes", map[string]any{
		"name": "adopted", "mounts": []map[string]string{{"host_path": root, "guest_path": "/workspace"}},
	})
	if resp.StatusCode != http.StatusCreated {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("initial create: %d %s", resp.StatusCode, b)
	}
	var sb store.Sandbox
	decodeJSON(t, resp, &sb)
	srv.mountRoots = nil
	srv.StartEventRecorder()
	srv.RecoverSandboxes(context.Background())
	status, err := eng.Status(context.Background(), sb.EngineID)
	if err != nil || status.Status != "running" {
		t.Fatalf("adopted helper killed: status=%+v err=%v", status, err)
	}
	// Close drains the asynchronous recorder, making the event query deterministic.
	srv.events.Close()
	srv.events = nil
	events, err := srv.store.QueryEvents(store.EventFilter{SandboxID: sb.ID, Type: "sandbox.mount_policy_violation", Since: time.Now().Add(-time.Minute)})
	if err != nil || len(events) != 1 {
		t.Fatalf("mount violation events = %+v, err=%v", events, err)
	}
}

func TestMountPolicyProtectsAllLayeredConfigDirectories(t *testing.T) {
	srv, ts := setup(t)
	base := t.TempDir()
	systemDir := filepath.Join(base, "system")
	userDir := filepath.Join(base, "user")
	for _, dir := range []string{systemDir, userDir} {
		if err := os.Mkdir(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}
	systemConfig := filepath.Join(systemDir, "config.yaml")
	userConfig := filepath.Join(userDir, "config.yaml")
	for _, path := range []string{systemConfig, userConfig} {
		if err := os.WriteFile(path, []byte("auth_token: secret"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	WithMountRoots([]string{userDir}, []string{systemConfig, userConfig})(srv)
	resp := doReq(t, ts, http.MethodPost, "/sandboxes", map[string]any{
		"name": "config-exposure", "mounts": []map[string]string{{"host_path": userDir, "guest_path": "/host"}},
	})
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusForbidden || !strings.Contains(string(body), "data/config directory") {
		t.Fatalf("layered user-config mount = %d %s, want forbidden", resp.StatusCode, body)
	}
}
