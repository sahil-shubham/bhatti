//go:build krucible

package krucible

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/sahil-shubham/bhatti/pkg/engine"
	"github.com/sahil-shubham/bhatti/pkg/gateway"
)

// TestKrucibleResumeScaling measures N parallel resumes of one snapshot whose
// disk holds a large delta: the wall time of the batch, of each resume, the
// bytes the clones' roots take, and the bytes the file system actually gave up.
// It is how the batch-restore question is settled by measurement (the roadmap's
// storage step), so it stays in the tree, off unless asked for:
//
//	KRUCIBLE_BENCH_RESUME=1                        run it
//	KRUCIBLE_BENCH_DELTA_MB=2048                   what the guest writes before the snapshot
//	KRUCIBLE_BENCH_N=1,5,10,20                     the batch sizes
//	KRUCIBLE_BENCH_TYPE=memory                     snapshot RAM too (where the VMM can)
//
// One line per batch, "bench.resume key=value ...". It uses only what an
// engine without layered disks has too, so the same file measures a commit
// from before them.
func TestKrucibleResumeScaling(t *testing.T) {
	if os.Getenv("KRUCIBLE_BENCH_RESUME") == "" {
		t.Skip("set KRUCIBLE_BENCH_RESUME=1 to run the resume scaling measurement")
	}
	deltaMB := 2048
	if v, err := strconv.Atoi(os.Getenv("KRUCIBLE_BENCH_DELTA_MB")); err == nil && v > 0 {
		deltaMB = v
	}
	sizes := []int{1, 5, 10, 20}
	if v := os.Getenv("KRUCIBLE_BENCH_N"); v != "" {
		sizes = nil
		for _, f := range strings.Split(v, ",") {
			n, err := strconv.Atoi(strings.TrimSpace(f))
			if err != nil || n < 1 {
				t.Fatalf("KRUCIBLE_BENCH_N: %q", v)
			}
			sizes = append(sizes, n)
		}
	}
	ke := newPauseEngine(t).(*Engine)
	records := &pauseRecords{}
	previousLogger := slog.Default()
	slog.SetDefault(slog.New(records))
	defer slog.SetDefault(previousLogger)
	snapType := "filesystem"
	if os.Getenv("KRUCIBLE_BENCH_TYPE") == "memory" {
		if !ke.caps.Checkpoint {
			t.Skip("this VMM build can't save guest memory")
		}
		snapType = "memory"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Minute)
	defer cancel()

	src, err := ke.Create(ctx, engine.SandboxSpec{Name: "benchsrc", CPUs: 1, MemoryMB: 512, DiskSizeMB: deltaMB + 1024,
		NetPolicy: &gateway.NetPolicyWire{Default: gateway.PostureNone}})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	t.Cleanup(func() { ke.Destroy(context.Background(), src.ID) })
	t0 := time.Now()
	sum := sha256.New()
	if err := ke.FileWrite(ctx, src.ID, "/workspace/delta", "0644", int64(deltaMB)<<20, io.TeeReader(&patternReader{left: int64(deltaMB) << 20}, sum)); err != nil {
		t.Fatalf("write the delta: %v", err)
	}
	if _, err := ke.Exec(ctx, src.ID, []string{"sync"}); err != nil {
		t.Fatalf("sync: %v", err)
	}
	t.Logf("bench.setup delta_mb=%d write_ms=%d", deltaMB, time.Since(t0).Milliseconds())

	snaps := filepath.Join(ke.cfg.DataDir, "snapshots", "bench")
	t0 = time.Now()
	manifest, err := ke.CheckpointTyped(ctx, src.ID, "bench", 0, "golden", snaps, snapType)
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	golden := filepath.Join(snaps, "golden")
	manifestJSON, _ := json.Marshal(manifest)
	disk := filepath.Join(golden, "rootfs.qcow2")
	t.Logf("bench.snapshot type=%s snapshot_ms=%d disk_mb=%d", snapType, time.Since(t0).Milliseconds(), blocksOf(t, disk)>>20)
	records.take()
	if ke.caps.Checkpoint {
		started := time.Now()
		fork, err := ke.Fork(ctx, src.ID, "benchfork")
		if err != nil {
			t.Fatalf("fork: %v", err)
		}
		t.Cleanup(func() { _ = ke.Destroy(context.Background(), fork.ID) })
		for _, record := range records.take() {
			t.Logf("bench.fork delta_mb=%d wall_ms=%d source_paused_ms=%d save_ms=%d flush_ms=%d copy_ms=%d",
				deltaMB, time.Since(started).Milliseconds(), record["paused_ms"], record["save_ms"], record["flush_ms"], record["copy_ms"])
		}
		if result, err := ke.Exec(ctx, src.ID, []string{"true"}); err != nil || result.ExitCode != 0 {
			t.Fatalf("source after fork: %+v, %v", result, err)
		}
		if err := ke.Destroy(ctx, fork.ID); err != nil {
			t.Fatal(err)
		}
	} else {
		t.Log("bench.fork unsupported: this helper cannot checkpoint memory; no source pause measurement")
	}
	// The source isn't needed any more, and its memory is the clones'.
	if err := ke.Destroy(ctx, src.ID); err != nil {
		t.Fatal(err)
	}

	// run starts n sandboxes at once and says how long each, and all of them,
	// took to come up.
	run := func(n int, start func(i int) (engine.SandboxInfo, error)) (ids []string, took []time.Duration, wall time.Duration) {
		ids, took = make([]string, n), make([]time.Duration, n)
		errs := make([]error, n)
		var wg sync.WaitGroup
		t0 := time.Now()
		for i := range ids {
			wg.Add(1)
			go func() {
				defer wg.Done()
				t1 := time.Now()
				info, err := start(i)
				took[i], errs[i], ids[i] = time.Since(t1), err, info.ID
				if info.ID != "" {
					t.Cleanup(func() { _ = ke.Destroy(context.Background(), info.ID) })
				}
			}()
		}
		wg.Wait()
		wall = time.Since(t0)
		for i, err := range errs {
			if err != nil {
				t.Fatalf("sandbox %d of %d: %v", i, n, err)
			}
		}
		return ids, took, wall
	}
	destroy := func(ids []string) {
		var wg sync.WaitGroup
		for _, id := range ids {
			wg.Add(1)
			go func() { defer wg.Done(); ke.Destroy(context.Background(), id) }()
		}
		wg.Wait()
	}
	mean := func(d []time.Duration) time.Duration {
		var total time.Duration
		for _, x := range d {
			total += x
		}
		return total / time.Duration(len(d))
	}

	for _, n := range sizes {
		// The control: n fresh sandboxes from the base image, which pay for
		// everything a resume does but its disk, so what a resume costs beyond
		// this is what its disk costs.
		ids, took, wall := run(n, func(i int) (engine.SandboxInfo, error) {
			return ke.Create(ctx, engine.SandboxSpec{Name: "ctl" + strconv.Itoa(n) + "-" + strconv.Itoa(i), CPUs: 1, MemoryMB: 512,
				NetPolicy: &gateway.NetPolicyWire{Default: gateway.PostureNone}})
		})
		slices.Sort(took)
		t.Logf("bench.control n=%d wall_ms=%d mean_ms=%d max_ms=%d", n, wall.Milliseconds(), mean(took).Milliseconds(), took[n-1].Milliseconds())
		destroy(ids)

		free0 := freeBytes(t, ke.cfg.DataDir)
		ids, took, wall = run(n, func(i int) (engine.SandboxInfo, error) {
			return ke.ResumeFromManifestJSON(ctx, golden, manifestJSON, "bench"+strconv.Itoa(n)+"-"+strconv.Itoa(i), "bench")
		})
		used := free0 - freeBytes(t, ke.cfg.DataDir)
		var rootBytes int64
		for _, id := range ids {
			dir := filepath.Join(ke.cfg.DataDir, "sandboxes", id)
			for _, f := range []string{"root.qcow2", "root.img", "restorevol0.img"} {
				if fi, err := os.Stat(filepath.Join(dir, f)); err == nil {
					rootBytes += fi.Sys().(*syscall.Stat_t).Blocks * 512
				}
			}
		}
		// One clone proves the delta is there to be read.
		got := sha256.New()
		if _, _, err := ke.FileRead(ctx, ids[0], "/workspace/delta", got); err != nil || string(got.Sum(nil)) != string(sum.Sum(nil)) {
			t.Fatalf("a clone's copy of the delta is wrong (%v)", err)
		}
		slices.Sort(took)
		t.Logf("bench.resume type=%s delta_mb=%d n=%d wall_ms=%d mean_ms=%d p50_ms=%d max_ms=%d roots_kb=%d fs_used_mb=%d",
			snapType, deltaMB, n, wall.Milliseconds(), mean(took).Milliseconds(),
			took[n/2].Milliseconds(), took[n-1].Milliseconds(), rootBytes>>10, used>>20)
		// A snapshot of a clone, once: what taking the next generation costs
		// (and holds the clone paused for) when the clone's disk is a copy of
		// the golden one, and when it is a thin layer over it.
		if n == sizes[0] {
			t1 := time.Now()
			if _, err := ke.CheckpointTyped(ctx, ids[0], "bench", 0, "gen2", snaps, snapType); err != nil {
				t.Fatalf("snapshot of a clone: %v", err)
			}
			t.Logf("bench.snapshot_of_clone type=%s delta_mb=%d snapshot_ms=%d own_disk_mb=%d", snapType, deltaMB, time.Since(t1).Milliseconds(),
				blocksOf(t, filepath.Join(snaps, "gen2", "rootfs.qcow2"))>>20)
		}
		destroy(ids)
	}
}

func blocksOf(t *testing.T, path string) int64 {
	t.Helper()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return fi.Sys().(*syscall.Stat_t).Blocks * 512
}

// freeBytes is what the file system holding dir has free for an unprivileged
// writer.
func freeBytes(t *testing.T, dir string) int64 {
	t.Helper()
	var st syscall.Statfs_t
	if err := syscall.Statfs(dir, &st); err != nil {
		t.Fatal(err)
	}
	return int64(st.Bavail) * int64(st.Bsize)
}
