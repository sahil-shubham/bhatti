//go:build krucible

package krucible

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sahil-shubham/bhatti/pkg/engine"
)

func TestKruciblePersistentVolumeDoesNotInterpretGuestHeader(t *testing.T) {
	ke := newPauseEngine(t).(*Engine)
	dir := t.TempDir()
	seed := filepath.Join(dir, "seed")
	if err := os.Mkdir(seed, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(seed, "marker"), []byte("host-backing-must-stay-private"), 0o644); err != nil {
		t.Fatal(err)
	}
	backing := filepath.Join(dir, "host-private.ext4")
	if out, err := exec.Command("mke2fs", "-t", "ext4", "-F", "-q", "-d", seed, backing, "8M").CombinedOutput(); err != nil {
		t.Fatalf("mke2fs backing: %v: %s", err, out)
	}
	volume := filepath.Join(dir, "guest-owned.ext4")
	if err := createQcow2Overlay(volume, backing, 64<<20); err != nil {
		t.Fatal(err)
	}
	if err := os.Truncate(volume, 96<<20); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	info, err := ke.Create(ctx, engine.SandboxSpec{Name: "opaque-volume", CPUs: 1, MemoryMB: 512,
		ResolvedVolumes: []engine.ResolvedVolume{{FilePath: volume, Mount: "/data"}}})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	t.Cleanup(func() { _ = ke.Destroy(context.Background(), info.ID) })
	result, err := ke.Exec(ctx, info.ID, []string{"cat", "/data/marker"})
	if err != nil {
		t.Fatalf("inspect guest marker: %v", err)
	}
	if strings.Contains(result.Stdout, "host-backing-must-stay-private") {
		t.Fatal("guest interpreted its raw volume's header and read the host backing file")
	}
	result, err = ke.Exec(ctx, info.ID, []string{"cat", "/proc/partitions"})
	if err != nil || result.ExitCode != 0 {
		t.Fatalf("inspect guest disk size: %+v, %v", result, err)
	}
	found := false
	for _, line := range strings.Split(result.Stdout, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 4 && fields[3] == "vdb" {
			found = true
			if fields[2] != "98304" {
				t.Fatalf("guest data disk is %s KiB, want raw file's 98304 rather than header's 65536", fields[2])
			}
		}
	}
	if !found {
		t.Fatalf("guest data disk missing: %s", result.Stdout)
	}
}
