//go:build krucible

package krucible

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/sahil-shubham/bhatti/pkg/engine"
	"github.com/sahil-shubham/bhatti/pkg/gateway"
)

// pauseRecords collects the engine's krucible.checkpoint.paused records.
type pauseRecords struct {
	mu   sync.Mutex
	recs []map[string]int64
}

func (p *pauseRecords) Enabled(context.Context, slog.Level) bool { return true }
func (p *pauseRecords) WithAttrs([]slog.Attr) slog.Handler       { return p }
func (p *pauseRecords) WithGroup(string) slog.Handler            { return p }
func (p *pauseRecords) Handle(_ context.Context, r slog.Record) error {
	if r.Message != "krucible.checkpoint.paused" {
		return nil
	}
	rec := map[string]int64{}
	r.Attrs(func(a slog.Attr) bool {
		if a.Value.Kind() == slog.KindInt64 {
			rec[a.Key] = a.Value.Int64()
		}
		return true
	})
	p.mu.Lock()
	p.recs = append(p.recs, rec)
	p.mu.Unlock()
	return nil
}

func (p *pauseRecords) take() []map[string]int64 {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := p.recs
	p.recs = nil
	return out
}

// TestKrucibleCheckpointPauseWindow measures how long a snapshot holds its
// source paused, and what the time is made of (the engine logs it at each
// snapshot): the memory save, the flush or sync of what is copied, the copy.
// Off unless asked for, like the resume measurement:
//
//	KRUCIBLE_BENCH_PAUSE=1             run it
//	KRUCIBLE_BENCH_DELTA_MB=512        what the guest wrote before the snapshots
//	KRUCIBLE_BENCH_LOAD_MB=1024        dirty pages other writers on the host hold
//	KRUCIBLE_BENCH_RUNS=5              snapshots to take
//	KRUCIBLE_BENCH_TYPE=memory         snapshot RAM too (where the VMM can)
func TestKrucibleCheckpointPauseWindow(t *testing.T) {
	if os.Getenv("KRUCIBLE_BENCH_PAUSE") == "" {
		t.Skip("set KRUCIBLE_BENCH_PAUSE=1 to run the snapshot pause measurement")
	}
	atoi := func(key string, def int) int {
		if v, err := strconv.Atoi(os.Getenv(key)); err == nil && v >= 0 {
			return v
		}
		return def
	}
	deltaMB, loadMB, runs := atoi("KRUCIBLE_BENCH_DELTA_MB", 512), atoi("KRUCIBLE_BENCH_LOAD_MB", 0), atoi("KRUCIBLE_BENCH_RUNS", 5)
	ke := newPauseEngine(t).(*Engine)
	snapType := "filesystem"
	if os.Getenv("KRUCIBLE_BENCH_TYPE") == "memory" {
		if !ke.caps.Checkpoint {
			t.Skip("this VMM build can't save guest memory")
		}
		snapType = "memory"
	}
	records := &pauseRecords{}
	prev := slog.Default()
	slog.SetDefault(slog.New(records))
	t.Cleanup(func() { slog.SetDefault(prev) })

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()
	src, err := ke.Create(ctx, engine.SandboxSpec{Name: "pausesrc", CPUs: 1, MemoryMB: 512, DiskSizeMB: deltaMB + 1024,
		NetPolicy: &gateway.NetPolicyWire{Default: gateway.PostureNone}})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	t.Cleanup(func() { ke.Destroy(context.Background(), src.ID) })
	if deltaMB > 0 {
		if err := ke.FileWrite(ctx, src.ID, "/workspace/delta", "0644", int64(deltaMB)<<20, &patternReader{left: int64(deltaMB) << 20}); err != nil {
			t.Fatalf("write the delta: %v", err)
		}
	}
	if _, err := ke.Exec(ctx, src.ID, []string{"sync"}); err != nil {
		t.Fatal(err)
	}

	// Other writers on the host: pages dirtied faster than the disk takes
	// them, which a host-wide sync has to wait for.
	stop := make(chan struct{})
	var load sync.WaitGroup
	if loadMB > 0 {
		load.Add(1)
		go func() {
			defer load.Done()
			f, err := os.Create(filepath.Join(ke.cfg.DataDir, "host-load.bin"))
			if err != nil {
				return
			}
			defer f.Close()
			block := make([]byte, 1<<20)
			for i := range block {
				block[i] = byte(i*7 + 1)
			}
			for written := 0; ; written++ {
				select {
				case <-stop:
					return
				default:
				}
				if _, err := f.WriteAt(block, int64(written%loadMB)<<20); err != nil {
					return
				}
			}
		}()
	}
	snaps := filepath.Join(ke.cfg.DataDir, "snapshots", "bench")
	for i := range runs {
		t0 := time.Now()
		if _, err := ke.CheckpointTyped(ctx, src.ID, "bench", 0, "p"+strconv.Itoa(i), snaps, snapType); err != nil {
			t.Fatalf("snapshot %d: %v", i, err)
		}
		total := time.Since(t0)
		for _, r := range records.take() {
			t.Logf("bench.pause type=%s delta_mb=%d load_mb=%d run=%d paused_ms=%d save_ms=%d sync_ms=%d flush_ms=%d copy_ms=%d total_ms=%d",
				snapType, deltaMB, loadMB, i, r["paused_ms"], r["save_ms"], r["sync_ms"], r["flush_ms"], r["copy_ms"], total.Milliseconds())
		}
	}
	close(stop)
	load.Wait()
}
