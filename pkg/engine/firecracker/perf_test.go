//go:build linux

package firecracker

import (
	"bytes"
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sahil-shubham/bhatti/pkg/agent"
	"github.com/sahil-shubham/bhatti/pkg/engine"
)

// --- Percentile helpers ---

type latencies []time.Duration

func (l latencies) p(pct float64) time.Duration {
	if len(l) == 0 {
		return 0
	}
	sort.Slice(l, func(i, j int) bool { return l[i] < l[j] })
	idx := int(float64(len(l)) * pct / 100)
	if idx >= len(l) {
		idx = len(l) - 1
	}
	return l[idx]
}

func (l latencies) report(name string, t *testing.T) {
	t.Helper()
	sort.Slice(l, func(i, j int) bool { return l[i] < l[j] })
	t.Logf("⏱ %s (n=%d): p50=%v  p95=%v  p99=%v  max=%v",
		name, len(l),
		l.p(50).Round(time.Microsecond),
		l.p(95).Round(time.Microsecond),
		l.p(99).Round(time.Microsecond),
		l[len(l)-1].Round(time.Microsecond))
}

// --- User-facing perf tests ---
// These measure what a consumer of bhatti actually experiences.

// TestPerfExecCommand measures end-to-end exec latency.
// 100 sequential `echo hello` commands on a hot sandbox.
func TestPerfExecCommand(t *testing.T) {
	eng := testEngine(t)
	ctx := context.Background()

	info, err := eng.Create(ctx, testSpec("perf-exec"))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	defer eng.Destroy(ctx, info.ID)

	// Warmup — first few execs have cold TCP connection overhead
	for i := 0; i < 5; i++ {
		execWithTimeout(t, eng, info.ID, []string{"echo", "warmup"})
	}

	// 100 sequential execs of a realistic minimal command
	var lats latencies
	for i := 0; i < 100; i++ {
		start := time.Now()
		r, err := execWithTimeout(t, eng, info.ID, []string{"echo", "hello"})
		elapsed := time.Since(start)
		if err != nil || r.ExitCode != 0 {
			t.Fatalf("exec %d failed: err=%v exit=%d", i, err, r.ExitCode)
		}
		if !strings.Contains(r.Stdout, "hello") {
			t.Fatalf("exec %d: unexpected output: %q", i, r.Stdout)
		}
		lats = append(lats, elapsed)
	}
	lats.report("exec `echo hello`", t)

	if lats.p(99) > 50*time.Millisecond {
		t.Errorf("p99 exec latency too high: %v (want <50ms)", lats.p(99))
	}
}

// TestPerfFileReadWrite measures file I/O through the wire protocol.
// 50 cycles of 1KB write + read.
func TestPerfFileReadWrite(t *testing.T) {
	eng := testEngine(t)
	ctx := context.Background()

	info, err := eng.Create(ctx, testSpec("perf-file"))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	defer eng.Destroy(ctx, info.ID)

	content := []byte(strings.Repeat("x", 1024)) // 1KB

	var writeLats, readLats latencies
	for i := 0; i < 50; i++ {
		path := fmt.Sprintf("/workspace/perf-%d.txt", i)

		start := time.Now()
		err := eng.FileWrite(ctx, info.ID, path, "0644", int64(len(content)), bytes.NewReader(content))
		writeLats = append(writeLats, time.Since(start))
		if err != nil {
			t.Fatalf("write %d: %v", i, err)
		}

		start = time.Now()
		var buf bytes.Buffer
		_, _, err = eng.FileRead(ctx, info.ID, path, &buf)
		readLats = append(readLats, time.Since(start))
		if err != nil {
			t.Fatalf("read %d: %v", i, err)
		}
		if buf.Len() != 1024 {
			t.Fatalf("read %d: got %d bytes, want 1024", i, buf.Len())
		}
	}
	writeLats.report("1KB file write", t)
	readLats.report("1KB file read", t)
}

// TestPerfWarmResumeExec measures the latency of executing a command on a
// paused (warm) sandbox. This is the most common path in production — the
// thermal manager pauses idle VMs after 30s, and the next API call
// transparently resumes before executing.
// 15 cycles of: pause → ensureHot → exec.
func TestPerfWarmResumeExec(t *testing.T) {
	eng := testEngine(t)
	ctx := context.Background()

	info, err := eng.Create(ctx, testSpec("perf-warm"))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	defer eng.Destroy(ctx, info.ID)

	// Warmup exec
	execWithTimeout(t, eng, info.ID, []string{"true"})

	var lats latencies
	for i := 0; i < 15; i++ {
		// Pause → warm state
		if err := eng.Pause(ctx, info.ID); err != nil {
			t.Fatalf("Pause %d: %v", i, err)
		}
		time.Sleep(50 * time.Millisecond) // let FC socket pool drain

		// Measure: resume + exec (what the user experiences)
		start := time.Now()
		if err := eng.EnsureHot(ctx, info.ID); err != nil {
			t.Fatalf("EnsureHot %d: %v", i, err)
		}
		r, err := execWithTimeout(t, eng, info.ID, []string{"echo", "warm"})
		elapsed := time.Since(start)
		if err != nil || r.ExitCode != 0 {
			t.Fatalf("exec %d: err=%v exit=%d", i, err, r.ExitCode)
		}
		lats = append(lats, elapsed)
	}
	lats.report("warm resume + exec", t)
}

// TestPerfColdResumeExec measures the latency of executing a command on a
// cold sandbox (snapshotted to disk, FC process killed, RAM freed).
// This is the slow path — happens after 30min idle.
// 7 cycles of: stop → start → exec.
func TestPerfColdResumeExec(t *testing.T) {
	eng := testEngine(t)
	ctx := context.Background()

	info, err := eng.Create(ctx, testSpec("perf-cold"))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	defer eng.Destroy(ctx, info.ID)

	// Write something so we can verify state survives
	execWithTimeout(t, eng, info.ID, []string{"sh", "-c", "echo alive > /tmp/state"})

	var lats latencies
	for i := 0; i < 7; i++ {
		// Stop → snapshot to disk, kill FC, free RAM
		if err := eng.Stop(ctx, info.ID); err != nil {
			t.Fatalf("Stop %d: %v", i, err)
		}

		// Measure: restore from snapshot + exec
		start := time.Now()
		if err := eng.Start(ctx, info.ID); err != nil {
			t.Fatalf("Start %d: %v", i, err)
		}
		r, err := execWithTimeout(t, eng, info.ID, []string{"echo", "cold"})
		elapsed := time.Since(start)
		if err != nil || r.ExitCode != 0 {
			t.Fatalf("exec %d: err=%v exit=%d", i, err, r.ExitCode)
		}
		lats = append(lats, elapsed)
	}
	lats.report("cold resume + exec", t)

	// Verify state survived all cycles
	r, _ := execWithTimeout(t, eng, info.ID, []string{"cat", "/tmp/state"})
	if !strings.Contains(r.Stdout, "alive") {
		t.Errorf("state lost after snapshot cycles: %q", r.Stdout)
	}
}

// TestPerfPauseResume measures pause and resume as separate operations.
// 20 cycles each.
func TestPerfPauseResume(t *testing.T) {
	eng := testEngine(t)
	ctx := context.Background()

	info, err := eng.Create(ctx, testSpec("perf-pr"))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	defer eng.Destroy(ctx, info.ID)

	// Warmup
	execWithTimeout(t, eng, info.ID, []string{"true"})

	var pauseLats, resumeLats latencies
	for i := 0; i < 20; i++ {
		start := time.Now()
		if err := eng.Pause(ctx, info.ID); err != nil {
			t.Fatalf("Pause %d: %v", i, err)
		}
		pauseLats = append(pauseLats, time.Since(start))

		time.Sleep(20 * time.Millisecond) // brief settle

		start = time.Now()
		if err := eng.Resume(ctx, info.ID); err != nil {
			t.Fatalf("Resume %d: %v", i, err)
		}
		resumeLats = append(resumeLats, time.Since(start))

		time.Sleep(20 * time.Millisecond)
	}
	pauseLats.report("pause (hot→warm)", t)
	resumeLats.report("resume (warm→hot)", t)

	// Verify VM still works
	r, err := execWithTimeout(t, eng, info.ID, []string{"echo", "ok"})
	if err != nil || !strings.Contains(r.Stdout, "ok") {
		t.Fatalf("VM broken after 20 pause/resume cycles: err=%v out=%q", err, r.Stdout)
	}
}

// TestPerfSnapshot measures full and diff snapshot latency with percentiles.
// 5 full snapshots (each requires a fresh VM), then 10 diff snapshots on one VM.
func TestPerfSnapshot(t *testing.T) {
	eng := testEngine(t)
	ctx := context.Background()

	// --- Full snapshots: 5 fresh VMs, first stop is always full ---
	var fullLats latencies
	for i := 0; i < 5; i++ {
		info, err := eng.Create(ctx, testSpec(fmt.Sprintf("perf-fsnap-%d", i)))
		if err != nil {
			t.Fatalf("Create (full %d): %v", i, err)
		}
		// Write some data so memory isn't trivially empty
		execWithTimeout(t, eng, info.ID, []string{"sh", "-c", "dd if=/dev/urandom of=/tmp/data bs=64K count=16 2>/dev/null"})

		start := time.Now()
		if err := eng.Stop(ctx, info.ID); err != nil {
			t.Fatalf("Stop (full %d): %v", i, err)
		}
		fullLats = append(fullLats, time.Since(start))
		eng.Destroy(ctx, info.ID)
	}
	fullLats.report("full snapshot (512MB VM)", t)

	// --- Diff snapshots: one VM, 10 cycles ---
	info, err := eng.Create(ctx, testSpec("perf-dsnap"))
	if err != nil {
		t.Fatalf("Create (diff): %v", err)
	}
	defer eng.Destroy(ctx, info.ID)

	execWithTimeout(t, eng, info.ID, []string{"sh", "-c", "echo init > /tmp/snap"})

	// First stop is full (establishes base)
	eng.Stop(ctx, info.ID)
	eng.Start(ctx, info.ID)

	var diffLats latencies
	for i := 0; i < 10; i++ {
		execWithTimeout(t, eng, info.ID, []string{"sh", "-c", fmt.Sprintf("echo diff-%d > /tmp/snap", i)})

		start := time.Now()
		if err := eng.Stop(ctx, info.ID); err != nil {
			t.Fatalf("Stop (diff %d): %v", i, err)
		}
		diffLats = append(diffLats, time.Since(start))
		eng.Start(ctx, info.ID)
	}
	diffLats.report("diff snapshot", t)

	// Verify state survived all cycles
	r, _ := execWithTimeout(t, eng, info.ID, []string{"cat", "/tmp/snap"})
	if !strings.Contains(r.Stdout, "diff-9") {
		t.Errorf("state lost: %q", r.Stdout)
	}
}

// TestPerfVMBoot measures end-to-end sandbox creation time.
// 5 VMs created and destroyed sequentially.
func TestPerfVMBoot(t *testing.T) {
	eng := testEngine(t)
	ctx := context.Background()

	var lats latencies
	for i := 0; i < 5; i++ {
		start := time.Now()
		info, err := eng.Create(ctx, testSpec(fmt.Sprintf("perf-boot-%d", i)))
		if err != nil {
			t.Fatalf("Create %d: %v", i, err)
		}
		// First exec confirms the VM is fully ready
		r, err := execWithTimeout(t, eng, info.ID, []string{"echo", "booted"})
		elapsed := time.Since(start)
		if err != nil || r.ExitCode != 0 {
			t.Fatalf("boot exec %d: err=%v exit=%d", i, err, r.ExitCode)
		}
		lats = append(lats, elapsed)
		eng.Destroy(ctx, info.ID)
	}
	lats.report("VM boot (create + first exec)", t)
}

// TestPerfConcurrentExec measures 10 concurrent execs — common when an LLM
// returns multiple tool calls in one message. Reports both per-exec latency
// and total wall clock time.
func TestPerfConcurrentExec(t *testing.T) {
	eng := testEngine(t)
	ctx := context.Background()

	info, err := eng.Create(ctx, testSpec("perf-conc"))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	defer eng.Destroy(ctx, info.ID)

	// Warmup
	execWithTimeout(t, eng, info.ID, []string{"true"})

	// Run 3 rounds of 10 concurrent execs for better percentiles
	var perExecLats latencies
	var wallClockLats latencies

	for round := 0; round < 3; round++ {
		var mu sync.Mutex
		var roundLats latencies
		var wg sync.WaitGroup
		errors := 0

		wallStart := time.Now()
		for i := 0; i < 10; i++ {
			wg.Add(1)
			go func(idx int) {
				defer wg.Done()
				start := time.Now()
				r, err := execWithTimeout(t, eng, info.ID, []string{"echo", fmt.Sprintf("c-%d-%d", round, idx)})
				elapsed := time.Since(start)
				mu.Lock()
				defer mu.Unlock()
				if err != nil || r.ExitCode != 0 {
					errors++
					return
				}
				roundLats = append(roundLats, elapsed)
			}(i)
		}
		wg.Wait()
		wallClock := time.Since(wallStart)

		if errors > 0 {
			t.Errorf("round %d: %d/10 concurrent execs failed", round, errors)
		}

		perExecLats = append(perExecLats, roundLats...)
		wallClockLats = append(wallClockLats, wallClock)

		time.Sleep(100 * time.Millisecond) // brief settle between rounds
	}

	perExecLats.report("10 concurrent execs (per-exec)", t)
	wallClockLats.report("10 concurrent execs (wall clock)", t)
}

// --- Supporting perf tests (not for the website, but useful) ---

func TestPerfStreamExecLatency(t *testing.T) {
	eng := testEngine(t)
	ctx := context.Background()

	info, err := eng.Create(ctx, testSpec("perf-stream"))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	defer eng.Destroy(ctx, info.ID)

	var ttfbLats, totalLats latencies
	for i := 0; i < 20; i++ {
		start := time.Now()
		var firstByte time.Duration
		var gotFirst bool
		eng.ExecStream(ctx, info.ID, []string{"echo", "perf-stream"}, func(e engine.StreamEvent) {
			if !gotFirst && e.Type == "stdout" {
				firstByte = time.Since(start)
				gotFirst = true
			}
		})
		total := time.Since(start)

		if gotFirst {
			ttfbLats = append(ttfbLats, firstByte)
		}
		totalLats = append(totalLats, total)
	}
	ttfbLats.report("stream exec TTFB", t)
	totalLats.report("stream exec total", t)
}

func TestPerfParallelFileReads(t *testing.T) {
	eng := testEngine(t)
	ctx := context.Background()

	info, err := eng.Create(ctx, testSpec("perf-pread"))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	defer eng.Destroy(ctx, info.ID)

	for i := 0; i < 5; i++ {
		path := fmt.Sprintf("/workspace/file-%d.txt", i)
		content := []byte(fmt.Sprintf("content-%d %s", i, strings.Repeat("x", 1024)))
		eng.FileWrite(ctx, info.ID, path, "0644", int64(len(content)), bytes.NewReader(content))
	}

	var mu sync.Mutex
	var lats latencies
	var wg sync.WaitGroup
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			path := fmt.Sprintf("/workspace/file-%d.txt", idx)
			start := time.Now()
			var buf bytes.Buffer
			_, _, err := eng.FileRead(ctx, info.ID, path, &buf)
			elapsed := time.Since(start)
			if err != nil {
				t.Errorf("parallel read %d: %v", idx, err)
				return
			}
			mu.Lock()
			lats = append(lats, elapsed)
			mu.Unlock()
		}(i)
	}
	wg.Wait()

	if len(lats) != 5 {
		t.Fatalf("expected 5 reads, got %d", len(lats))
	}
	lats.report("5 parallel 1KB reads", t)
}

func TestPerfTruncatedFileRead(t *testing.T) {
	eng := testEngine(t)
	ctx := context.Background()

	info, err := eng.Create(ctx, testSpec("perf-trunc"))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	defer eng.Destroy(ctx, info.ID)

	execWithTimeout(t, eng, info.ID, []string{"sh", "-c",
		"for i in $(seq 1 10000); do echo \"line $i of the log file with some padding to make it realistic\"; done > /workspace/big.log"})

	var fullLats latencies
	for i := 0; i < 10; i++ {
		var buf bytes.Buffer
		start := time.Now()
		eng.FileRead(ctx, info.ID, "/workspace/big.log", &buf)
		fullLats = append(fullLats, time.Since(start))
	}
	fullLats.report("10K-line full read", t)

	var truncLats latencies
	for i := 0; i < 10; i++ {
		var buf bytes.Buffer
		start := time.Now()
		eng.FileRead(ctx, info.ID, "/workspace/big.log", &buf,
			agent.FileReadOpts{Limit: 100})
		truncLats = append(truncLats, time.Since(start))
	}
	truncLats.report("10K-line truncated read (limit=100)", t)

	speedup := float64(fullLats.p(50)) / float64(truncLats.p(50))
	t.Logf("  truncation speedup: %.1fx at p50", speedup)
}
