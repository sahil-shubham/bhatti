package server

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sahil-shubham/bhatti/pkg/store"
)

func TestCreateMountSourcePolicy(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "data")
	inside := filepath.Join(root, "project")
	outside := filepath.Join(base, "database")
	for _, dir := range []string{inside, outside, filepath.Join(base, "config")} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	file := filepath.Join(inside, "not-a-directory")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "escape")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(inside, filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(root, filepath.Join(base, "root-link")); err != nil {
		t.Fatal(err)
	}
	configFile := filepath.Join(base, "config", "local.yaml")
	if err := os.WriteFile(configFile, []byte(""), 0o600); err != nil {
		t.Fatal(err)
	}
	canonicalInside, err := filepath.EvalSymlinks(inside)
	if err != nil {
		t.Fatal(err)
	}
	canonicalRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name        string
		roots       []string
		source      string
		config      string
		status      int
		wantPath    string
		wantError   string
		useTemplate bool
	}{
		{name: "unset roots deny", source: inside, status: 403, wantError: "mount_roots"},
		{name: "unset roots names loaded config", source: inside, config: configFile, status: 403, wantError: configFile},
		{name: "inside root", roots: []string{root}, source: inside, status: 201, wantPath: canonicalInside},
		{name: "root itself", roots: []string{root}, source: root, status: 201, wantPath: canonicalRoot},
		{name: "symlink inside root canonicalized", roots: []string{root}, source: filepath.Join(root, "link"), status: 201, wantPath: canonicalInside},
		{name: "symlink root canonicalized", roots: []string{filepath.Join(base, "root-link")}, source: inside, status: 201, wantPath: canonicalInside},
		{name: "template create also confined", roots: []string{root}, source: inside, status: 201, wantPath: canonicalInside, useTemplate: true},
		{name: "sibling prefix refused", roots: []string{root}, source: outside, status: 403},
		{name: "dot dot escape refused", roots: []string{root}, source: filepath.Join(root, "..", "database"), status: 403},
		{name: "symlink escape refused", roots: []string{root}, source: filepath.Join(root, "escape"), status: 403},
		{name: "regular file refused", roots: []string{root}, source: file, status: 400},
		{name: "missing path refused", roots: []string{root}, source: filepath.Join(root, "missing"), status: 400},
		{name: "relative path refused", roots: []string{root}, source: "data/project", status: 400},
		{name: "slash source refused", roots: []string{root}, source: "/", status: 403},
		{name: "root slash refused", roots: []string{"/"}, source: inside, status: 403},
		{name: "relative root refused", roots: []string{"data"}, source: inside, status: 403},
		{name: "root containing config directory refused", roots: []string{base}, source: inside, config: configFile, status: 403},
		{name: "invalid root denies all", roots: []string{root, filepath.Join(base, "missing")}, source: inside, status: 403},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			srv, ts := setup(t)
			srv.mountRoots = tc.roots
			srv.configPath = tc.config
			body := map[string]any{"name": "mount-test", "mounts": []map[string]string{{"host_path": tc.source, "guest_path": "/host"}}}
			if tc.useTemplate {
				if err := srv.store.CreateTemplate(store.Template{ID: "tpl", Name: "tmpl", CPUs: 1, MemoryMB: 128}); err != nil {
					t.Fatal(err)
				}
				body["template_id"] = "tpl"
			}
			resp := doReq(t, ts, "POST", "/sandboxes", body)
			defer resp.Body.Close()
			if resp.StatusCode != tc.status {
				b, _ := io.ReadAll(resp.Body)
				t.Fatalf("status = %d, want %d: %s", resp.StatusCode, tc.status, b)
			}
			if tc.status != 201 {
				b, _ := io.ReadAll(resp.Body)
				if tc.wantError != "" && !strings.Contains(string(b), tc.wantError) {
					t.Errorf("error %q does not contain %q", b, tc.wantError)
				}
				if srv.engine.(*mockEngine).nextID.Load() != 0 {
					t.Fatal("rejected mount reached engine.Create")
				}
				return
			}
			mounts := srv.engine.(*mockEngine).LastCreateSpec.Mounts
			if len(mounts) != 1 || mounts[0].HostPath != tc.wantPath {
				t.Fatalf("engine mounts = %+v, want canonical host path %q", mounts, tc.wantPath)
			}
		})
	}

	t.Run("root containing data dir refused", func(t *testing.T) {
		srv, ts := setup(t)
		root := filepath.Dir(srv.dataDir)
		srv.mountRoots = []string{root}
		resp := doReq(t, ts, "POST", "/sandboxes", map[string]any{
			"name": "mount-test", "mounts": []map[string]string{{"host_path": root, "guest_path": "/host"}},
		})
		defer resp.Body.Close()
		if resp.StatusCode != 403 {
			b, _ := io.ReadAll(resp.Body)
			t.Fatalf("status = %d, want 403: %s", resp.StatusCode, b)
		}
	})
}
