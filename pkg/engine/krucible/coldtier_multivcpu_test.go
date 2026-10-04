//go:build krucible

package krucible

import (
	"bytes"
	"context"
	"encoding/json"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sahil-shubham/bhatti/pkg/engine"
)

// TestKrucibleCheckpointMultiVcpu restores two online, usable vCPUs and guest
// RAM into a new VM rather than relying on power-off Stop/Start.
func TestKrucibleCheckpointMultiVcpu(t *testing.T) {
	eng := newCheckpointEngine(t).(*Engine)
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()
	src, err := eng.Create(ctx, engine.SandboxSpec{Name: "cpumv", CPUs: 2, MemoryMB: 512})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	t.Cleanup(func() { eng.Destroy(context.Background(), src.ID) })
	const marker = "both-cpus-checkpointed"
	if err := eng.FileWrite(ctx, src.ID, "/tmp/cpus", "0644", int64(len(marker)), strings.NewReader(marker)); err != nil {
		t.Fatalf("write tmpfs marker: %v", err)
	}
	parent := t.TempDir()
	manifest, err := eng.Checkpoint(ctx, src.ID, "", 0, "cpus", parent)
	if err != nil {
		t.Fatalf("Checkpoint: %v", err)
	}
	raw, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	restored, err := eng.ResumeFromManifestJSON(ctx, filepath.Join(parent, "cpus"), raw, "cpus-restored", "")
	if err != nil {
		t.Fatalf("restore: %v", err)
	}
	t.Cleanup(func() { eng.Destroy(context.Background(), restored.ID) })
	r, err := eng.Exec(ctx, restored.ID, []string{"cat", "/proc/cpuinfo"})
	if err != nil || strings.Count(r.Stdout, "processor") != 2 {
		t.Fatalf("online CPUs after restore: err=%v cpuinfo=%q", err, r.Stdout)
	}
	for cpu := range 2 {
		want := strconv.Itoa(cpu)
		r, err := eng.Exec(ctx, restored.ID, []string{"oncpu", want})
		if err != nil || r.ExitCode != 0 || strings.TrimSpace(r.Stdout) != want {
			t.Fatalf("work on vCPU %d: err=%v result=%+v", cpu, err, r)
		}
	}
	var got bytes.Buffer
	if _, _, err := eng.FileRead(ctx, restored.ID, "/tmp/cpus", &got); err != nil || got.String() != marker {
		t.Fatalf("RAM marker after restore: err=%v got=%q want=%q", err, got.String(), marker)
	}
}
