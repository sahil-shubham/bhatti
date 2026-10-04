//go:build krucible

package krucible

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sahil-shubham/bhatti/pkg/engine"
)

func checkpointMarker(t *testing.T, e *Engine, ctx context.Context, id, path, want string) {
	t.Helper()
	var b bytes.Buffer
	if _, _, err := e.FileRead(ctx, id, path, &b); err != nil || strings.TrimSpace(b.String()) != want {
		t.Fatalf("sandbox %s file %s: err=%v got=%q want=%q", id, path, err, b.String(), want)
	}
}

func checkpointJSON(t *testing.T, m any) []byte {
	t.Helper()
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// TestKrucibleRestoreAfterGap guards kvmclock/TSC rebase across elapsed host
// time. A frozen guest clock may survive an immediate restore yet stall for
// seconds after a delayed one; timers must also keep their real duration.
func TestKrucibleRestoreAfterGap(t *testing.T) {
	e := newCheckpointEngine(t).(*Engine)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	src, err := e.Create(ctx, engine.SandboxSpec{Name: "clock-src", CPUs: 1, MemoryMB: 512})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = e.Destroy(context.Background(), src.ID) })

	wallOffset := func(t *testing.T, ctx context.Context, id string) time.Duration {
		t.Helper()
		before := time.Now()
		r, err := e.Exec(ctx, id, []string{"date"})
		after := time.Now()
		if err != nil || r.ExitCode != 0 {
			t.Fatalf("guest date: err=%v result=%+v", err, r)
		}
		nanos, err := strconv.ParseInt(strings.TrimSpace(r.Stdout), 10, 64)
		if err != nil {
			t.Fatalf("guest date %q: %v", r.Stdout, err)
		}
		return time.Duration(nanos - before.Add(after.Sub(before)/2).UnixNano())
	}
	uptime := func(t *testing.T, ctx context.Context, id string) float64 {
		t.Helper()
		r, err := e.Exec(ctx, id, []string{"cat", "/proc/uptime"})
		if err != nil || r.ExitCode != 0 {
			t.Fatalf("guest uptime: err=%v result=%+v", err, r)
		}
		fields := strings.Fields(r.Stdout)
		if len(fields) == 0 {
			t.Fatalf("empty uptime: %q", r.Stdout)
		}
		seconds, err := strconv.ParseFloat(fields[0], 64)
		if err != nil {
			t.Fatal(err)
		}
		return seconds
	}
	baselineOffset := wallOffset(t, ctx, src.ID)
	baselineUptime := uptime(t, ctx, src.ID)
	parent := t.TempDir()
	m, err := e.Checkpoint(ctx, src.ID, "", 0, "clock", parent)
	if err != nil {
		t.Fatal(err)
	}
	savedAt := time.Now()
	dir, raw := filepath.Join(parent, "clock"), checkpointJSON(t, m)
	for _, gap := range []time.Duration{0, 3 * time.Second, 10 * time.Second, 65 * time.Second} {
		t.Run(gap.String(), func(t *testing.T) {
			if remaining := time.Until(savedAt.Add(gap)); remaining > 0 {
				time.Sleep(remaining)
			}
			// Include the restore in the 10s budget: a frozen guest cannot
			// respond to the readiness probe, not just to a later Exec.
			attemptCtx, attemptCancel := context.WithTimeout(ctx, 10*time.Second)
			defer attemptCancel()
			restored, err := e.ResumeFromManifestJSON(attemptCtx, dir, raw, "gap-"+strconv.Itoa(int(gap.Seconds())), "")
			if err != nil {
				t.Fatalf("restore after %s: %v", gap, err)
			}
			defer e.Destroy(context.Background(), restored.ID)
			if r, err := e.Exec(attemptCtx, restored.ID, []string{"echo", "clock-ready"}); err != nil || strings.TrimSpace(r.Stdout) != "clock-ready" {
				t.Fatalf("exec after %s: err=%v result=%+v", gap, err, r)
			}
			if delta := wallOffset(t, attemptCtx, restored.ID) - baselineOffset; math.Abs(float64(delta)) > float64(2*time.Second) {
				t.Fatalf("guest clock offset changed by %s after %s (clock did not advance across gap)", delta, gap)
			}
			if got := uptime(t, attemptCtx, restored.ID); got < baselineUptime {
				t.Fatalf("guest uptime went backwards: %.2fs < %.2fs", got, baselineUptime)
			}
			start := time.Now()
			r, err := e.Exec(attemptCtx, restored.ID, []string{"sleep", "1"})
			elapsed := time.Since(start)
			if err != nil || r.ExitCode != 0 || elapsed < 900*time.Millisecond || elapsed > 3*time.Second {
				t.Fatalf("guest sleep 1 after %s: err=%v result=%+v host elapsed=%s", gap, err, r, elapsed)
			}
		})
	}
}

func checkpointHashes(t *testing.T, dir string) map[string][sha256.Size]byte {
	t.Helper()
	hashes := make(map[string][sha256.Size]byte)
	if err := filepath.WalkDir(dir, func(path string, de os.DirEntry, err error) error {
		if err != nil || de.IsDir() {
			return err
		}
		f, err := os.Open(path)
		if err != nil {
			return err
		}
		defer f.Close()
		h := sha256.New()
		if _, err := io.Copy(h, f); err != nil {
			return err
		}
		rel, err := filepath.Rel(dir, path)
		if err == nil {
			var sum [sha256.Size]byte
			copy(sum[:], h.Sum(nil))
			hashes[rel] = sum
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if len(hashes) < 3 { // checkpoint.bin, memory.bin and rootfs.qcow2
		t.Fatalf("incomplete checkpoint file set: %v", hashes)
	}
	return hashes
}

// TestKrucibleCheckpointReusable proves two simultaneously live restores never
// consume or rewrite the checkpoint's RAM, metadata, or frozen disk.
func TestKrucibleCheckpointReusable(t *testing.T) {
	e := newCheckpointEngine(t).(*Engine)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	src, err := e.Create(ctx, engine.SandboxSpec{Name: "reuse-src", CPUs: 1, MemoryMB: 512})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = e.Destroy(context.Background(), src.ID) })
	const marker = "checkpoint-unmodified"
	if err := e.FileWrite(ctx, src.ID, "/tmp/from-source", "0644", int64(len(marker)), strings.NewReader(marker)); err != nil {
		t.Fatal(err)
	}
	parent := t.TempDir()
	m, err := e.Checkpoint(ctx, src.ID, "", 0, "reuse", parent)
	if err != nil {
		t.Fatal(err)
	}
	dir, raw := filepath.Join(parent, "reuse"), checkpointJSON(t, m)
	before := checkpointHashes(t, dir)
	var ids []string
	for i := range 2 {
		info, err := e.ResumeFromManifestJSON(ctx, dir, raw, fmt.Sprintf("reuse-%d", i), "")
		if err != nil {
			t.Fatalf("restore %d: %v", i, err)
		}
		ids = append(ids, info.ID)
		t.Cleanup(func() { _ = e.Destroy(context.Background(), info.ID) })
		checkpointMarker(t, e, ctx, info.ID, "/tmp/from-source", marker)
		for _, path := range []string{"/tmp/independent", "/workspace/independent"} {
			value := fmt.Sprintf("child-%d", i)
			if err := e.FileWrite(ctx, info.ID, path, "0644", int64(len(value)), strings.NewReader(value)); err != nil {
				t.Fatalf("write %s to child %d: %v", path, i, err)
			}
		}
	}
	if ids[0] == ids[1] {
		t.Fatal("two restores reused one sandbox")
	}
	for i, id := range ids {
		for _, path := range []string{"/tmp/independent", "/workspace/independent"} {
			checkpointMarker(t, e, ctx, id, path, fmt.Sprintf("child-%d", i))
		}
		if err := e.Destroy(ctx, id); err != nil {
			t.Fatal(err)
		}
	}
	after := checkpointHashes(t, dir)
	if len(before) != len(after) {
		t.Fatalf("checkpoint file set changed: before=%v after=%v", before, after)
	}
	for name, want := range before {
		if got, ok := after[name]; !ok || got != want {
			t.Fatalf("checkpoint file %s changed after two restores", name)
		}
	}
}

// TestKrucibleRestoreRefusals exercises actual builder validation and verifies
// that refusal neither leaks a helper nor poisons subsequent successful restores.
func TestKrucibleRestoreRefusals(t *testing.T) {
	e := newCheckpointEngine(t).(*Engine)
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	vol := makeVolume(t, "refusal", "/data")
	src, err := e.Create(ctx, engine.SandboxSpec{Name: "refusal-src", CPUs: 1, MemoryMB: 512, ResolvedVolumes: []engine.ResolvedVolume{vol}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = e.Destroy(context.Background(), src.ID) })
	parent := t.TempDir()
	m, err := e.Checkpoint(ctx, src.ID, "", 0, "good", parent)
	if err != nil {
		t.Fatal(err)
	}
	good := filepath.Join(parent, "good")
	manifest := m.(krucibleSnapManifest)
	type refusal struct {
		name, want string
		mutate     func(t *testing.T, dir string, m *krucibleSnapManifest)
	}
	cases := []refusal{
		{"missing-checkpoint", "incomplete checkpoint", func(t *testing.T, dir string, _ *krucibleSnapManifest) {
			if err := os.Remove(filepath.Join(dir, "checkpoint.bin")); err != nil {
				t.Fatal(err)
			}
		}},
		{"missing-memory", "incomplete checkpoint", func(t *testing.T, dir string, _ *krucibleSnapManifest) {
			if err := os.Remove(filepath.Join(dir, "memory.bin")); err != nil {
				t.Fatal(err)
			}
		}},
		{"truncated-checkpoint", "truncated", func(t *testing.T, dir string, _ *krucibleSnapManifest) {
			if err := os.Truncate(filepath.Join(dir, "checkpoint.bin"), 10); err != nil {
				t.Fatal(err)
			}
		}},
		{"future-version", "unsupported checkpoint version", func(t *testing.T, dir string, _ *krucibleSnapManifest) {
			tamperCheckpointHeader(t, dir, 8, 2)
		}},
		{"wrong-arch", "checkpoint was taken on", func(t *testing.T, dir string, _ *krucibleSnapManifest) {
			tamperCheckpointHeader(t, dir, 12, 2)
		}},
		{"short-memory", "memory.bin", func(t *testing.T, dir string, _ *krucibleSnapManifest) {
			path := filepath.Join(dir, "memory.bin")
			fi, err := os.Stat(path)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.Truncate(path, fi.Size()-4096); err != nil {
				t.Fatal(err)
			}
		}},
		{"vcpu-mismatch", "vCPU", func(_ *testing.T, _ string, m *krucibleSnapManifest) {
			m.Vcpus++
		}},
		{"device-mismatch", "devices differ", func(_ *testing.T, _ string, m *krucibleSnapManifest) {
			m.Volumes = nil
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			copyDir := filepath.Join(t.TempDir(), "tampered")
			if out, err := exec.Command("cp", "-a", good, copyDir).CombinedOutput(); err != nil {
				t.Fatalf("cp -a checkpoint: %v: %s", err, out)
			}
			badManifest := manifest
			tc.mutate(t, copyDir, &badManifest)
			rctx, rcancel := context.WithTimeout(ctx, 15*time.Second)
			defer rcancel()
			start := time.Now()
			badRestore, err := e.ResumeFromManifestJSON(rctx, copyDir, checkpointJSON(t, badManifest), "bad-"+tc.name, "")
			if err == nil {
				_ = e.Destroy(context.Background(), badRestore.ID)
				t.Fatalf("tampered checkpoint %q restored successfully", tc.name)
			}
			if !strings.Contains(err.Error(), tc.want) && (tc.name != "truncated-checkpoint" || !strings.Contains(err.Error(), "corrupt")) {
				t.Fatalf("refusal: want %q (or corrupt for truncation), got %v", tc.want, err)
			}
			if d := time.Since(start); d >= 15*time.Second {
				t.Fatalf("refusal took %s (>=15s): %v", d, err)
			}
			if n := checkpointHelperCount(t, e.cfg.DataDir); n != 1 {
				t.Fatalf("refused restore left %d helpers in engine (want only source)", n)
			}
			goodRestore, err := e.ResumeFromManifestJSON(ctx, good, checkpointJSON(t, manifest), "healthy-"+tc.name, "")
			if err != nil {
				t.Fatalf("healthy restore after refusal: %v", err)
			}
			if err := e.Destroy(ctx, goodRestore.ID); err != nil {
				t.Fatal(err)
			}
		})
	}
	fresh, err := e.Create(ctx, engine.SandboxSpec{Name: "fresh-after-refusals", CPUs: 1, MemoryMB: 512})
	if err != nil {
		t.Fatalf("fresh Create after refusals: %v", err)
	}
	if err := e.Destroy(ctx, fresh.ID); err != nil {
		t.Fatal(err)
	}
}

func tamperCheckpointHeader(t *testing.T, dir string, offset int64, value uint32) {
	t.Helper()
	f, err := os.OpenFile(filepath.Join(dir, "checkpoint.bin"), os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var word [4]byte
	binary.LittleEndian.PutUint32(word[:], value)
	if _, err := f.WriteAt(word[:], offset); err != nil {
		t.Fatal(err)
	}
}

func checkpointHelperCount(t *testing.T, dataDir string) int {
	t.Helper()
	out, err := exec.Command("ps", "ax", "-o", "command").Output()
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, line := range strings.Split(string(out), "\n") {
		if strings.Contains(line, "bhatti-vmm") && strings.Contains(line, dataDir+"/sandboxes/") {
			count++
		}
	}
	return count
}
