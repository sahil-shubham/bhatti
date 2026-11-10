package krucible

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/sahil-shubham/bhatti/pkg/configdrive"
	"github.com/sahil-shubham/bhatti/pkg/engine"
)

func bootConfig(t *testing.T, e *Engine, id string) configdrive.SandboxConfig {
	t.Helper()
	vm, err := e.getVM(id)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(vm.SandboxDir, "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	var cfg configdrive.SandboxConfig
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatal(err)
	}
	return cfg
}

// coldBoot stops and starts the sandbox: a fresh boot, which reads its boot
// config afresh, unlike a resume from memory.
func coldBoot(t *testing.T, ctx context.Context, e *Engine, id string) configdrive.SandboxConfig {
	t.Helper()
	if err := e.Stop(ctx, id); err != nil {
		t.Fatal(err)
	}
	if err := e.Start(ctx, id); err != nil {
		t.Fatal(err)
	}
	return bootConfig(t, e, id)
}

// TestMemoryCopiesColdBootWithTheirSourcesGuestSettings: a fork or a memory
// resume comes back running its source's guest, env (secret placeholders
// included), files and --init with it, and trusting its source's CA. lohar
// reapplies its boot config at every cold boot and removes a CA the config
// doesn't name, so a copy's first stop and start used to boot it without
// them. The copy keeps the settings, and the CA the server gives it; its
// identity stays its own. The record stays the daemon's: an export leaves it
// out, like the manifest.
func TestMemoryCopiesColdBootWithTheirSourcesGuestSettings(t *testing.T) {
	e := fakeReseedEngine(t, "supported")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	const ca = "-----BEGIN CERTIFICATE-----\nsource CA\n-----END CERTIFICATE-----\n"
	const placeholder = "bhatti_sec_aaaaaaaaaaaaaaaaaaaaaaaa"
	spec := noNetworkSpec("source")
	spec.CACert = ca
	spec.Env = map[string]string{"GH_TOKEN": placeholder, "MODE": "demo"}
	spec.Init = "serve-app"
	spec.Files = map[string]engine.FileSpec{"/etc/app.conf": {Content: []byte("port=8080\n"), Mode: "0644"}}
	source, err := e.Create(ctx, spec)
	if err != nil {
		t.Fatal(err)
	}
	src := bootConfig(t, e, source.ID)

	fork, err := e.Fork(ctx, source.ID, "fork")
	if err != nil {
		t.Fatal(err)
	}
	snapDir := t.TempDir()
	manifest, err := e.Checkpoint(ctx, source.ID, "", 0, "memory", snapDir)
	if err != nil {
		t.Fatal(err)
	}
	manifestJSON, _ := json.Marshal(manifest)
	resumed, err := e.ResumeFromManifestJSON(ctx, filepath.Join(snapDir, "memory"), manifestJSON, "resumed", "")
	if err != nil {
		t.Fatal(err)
	}

	for _, copy := range []engine.SandboxInfo{fork, resumed} {
		if err := e.SetCACert(ctx, copy.ID, ca); err != nil {
			t.Fatal(err)
		}
		got := coldBoot(t, ctx, e, copy.ID)
		if got.CACert != ca || !reflect.DeepEqual(got.Env, src.Env) || !reflect.DeepEqual(got.Files, src.Files) || got.Init != src.Init {
			t.Fatalf("%s cold-boots with ca=%q env=%v files=%v init=%q; want its source's", copy.Name, got.CACert, got.Env, got.Files, got.Init)
		}
		if got.SandboxID != copy.ID || got.Hostname != copy.Name || got.SandboxID == src.SandboxID {
			t.Fatalf("%s boots as %q/%q; want its own identity", copy.Name, got.SandboxID, got.Hostname)
		}
		if has, known := e.HasInit(copy.ID); !known || !has {
			t.Fatalf("%s: the engine says it has no --init", copy.Name)
		}
	}

	var archive bytes.Buffer
	if err := e.ExportSnapshot(ctx, &archive, filepath.Join(snapDir, "memory"), manifestJSON, engine.SnapshotExport{Name: "memory"}); err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(archive.Bytes(), []byte(snapshotBootFile)) {
		t.Fatal("an exported snapshot carries the daemon's boot settings record")
	}

	if err := e.SetCACert(ctx, "missing", ca); err == nil {
		t.Fatal("an unknown sandbox's CA was set")
	}
}
