package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

func (r *run) spec(tier, from string) createSpec {
	return createSpec{Name: r.nextName(), Tier: tier, Image: r.cfg.image, From: from, Network: r.cfg.network, CPUs: r.cfg.cpus, Memory: r.cfg.memory}
}
func (r *run) createMeasured(ctx context.Context, op string, spec createSpec) (sandbox, error) {
	var sb sandbox
	err := r.measure(ctx, op, func(ctx context.Context) error {
		var err error
		sb, err = r.api.create(ctx, spec)
		return err
	})
	if err == nil {
		if sb.ID == "" {
			return sb, fmt.Errorf("%s returned no sandbox id", op)
		}
		r.track(sb.ID)
		if r.sampler != nil && sb.CreatedBy != "" && r.cfg.ownerID == "" {
			r.sampler.SetBenchOwner(sb.CreatedBy)
		}
	}
	return sb, err
}
func (r *run) destroyMeasured(ctx context.Context, op, id string) error {
	err := r.measure(ctx, op, func(ctx context.Context) error { return r.api.destroy(ctx, id) })
	if err == nil {
		r.untrack(id)
	}
	return err
}
func (r *run) execMeasured(ctx context.Context, op, id string, args ...string) (execResult, error) {
	var result execResult
	err := r.measure(ctx, op, func(ctx context.Context) error { var e error; result, e = r.api.exec(ctx, id, args...); return e })
	return result, err
}
func (r *run) waitWarm(ctx context.Context, ids []string) error {
	timer := time.NewTicker(time.Second)
	defer timer.Stop()
	for {
		list, err := r.api.list(ctx)
		if err != nil {
			return err
		}
		ready := make(map[string]bool, len(ids))
		for _, sb := range list {
			if sb.Thermal == "warm" {
				ready[sb.ID] = true
			}
		}
		all := true
		for _, id := range ids {
			if !ready[id] {
				all = false
				break
			}
		}
		if all {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timer.C:
		}
	}
}

func waitPace(ctx context.Context, ticker *time.Ticker) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-ticker.C:
		return nil
	}
}

// A rejected request is an observed result, not a request to retry. Pausing
// before the next new attempt keeps a saturated per-user bucket from turning
// into a busy-loop of thousands of 429s per second.
func pauseAfter429(ctx context.Context, err error, delay time.Duration) {
	var apiErr *apiError
	if !errors.As(err, &apiErr) || apiErr.Status != 429 {
		return
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
	case <-timer.C:
	}
}

func (r *run) baseline() error {
	ctx := r.ctx
	var errs []error
	createPace := time.NewTicker(2100 * time.Millisecond)
	defer createPace.Stop()
	execPace := time.NewTicker(110 * time.Millisecond)
	defer execPace.Stop()
	base, err := r.createMeasured(ctx, "baseline_source_create", r.spec(r.cfg.tier, ""))
	if err != nil {
		return err
	}
	browser := r.spec("browser", "")
	browser.Image = "browser"
	if b, err := r.createMeasured(ctx, "browser_create", browser); err != nil {
		errs = append(errs, err)
	} else {
		errs = append(errs, r.destroyMeasured(ctx, "browser_destroy", b.ID))
	}
	for range r.cfg.lifecycle {
		if ctx.Err() != nil {
			break
		}
		if e := waitPace(ctx, createPace); e != nil {
			errs = append(errs, e)
			break
		}
		sb, e := r.createMeasured(ctx, "create", r.spec(r.cfg.tier, ""))
		if e != nil {
			errs = append(errs, e)
			continue
		}
		errs = append(errs, r.destroyMeasured(ctx, "destroy", sb.ID))
	}
	_, err = r.execMeasured(ctx, "setup_hot_exec", base.ID, "/bin/true")
	if err != nil {
		return errors.Join(append(errs, err)...)
	}
	for range r.cfg.iterations {
		if ctx.Err() != nil {
			break
		}
		if e := waitPace(ctx, execPace); e != nil {
			errs = append(errs, e)
			break
		}
		_, e := r.execMeasured(ctx, "exec_true", base.ID, "/bin/true")
		errs = append(errs, e)
		if e := waitPace(ctx, execPace); e != nil {
			errs = append(errs, e)
			break
		}
		errs = append(errs, r.measure(ctx, "exec_small_output", func(ctx context.Context) error {
			result, e := r.api.exec(ctx, base.ID, "/bin/sh", "-c", "printf bench-load")
			if e == nil && result.Stdout != "bench-load" {
				return fmt.Errorf("small output mismatch: got %q", result.Stdout)
			}
			return e
		}))
	}
	for _, size := range []struct {
		name string
		n    int
	}{{"1k", 1024}, {"100k", 100 * 1024}, {"1m", 1024 * 1024}} {
		payload := bytes.Repeat([]byte("bench-load-0123456789"), (size.n/21)+1)[:size.n]
		file := "/tmp/" + r.prefix + "-" + size.name
		for range r.cfg.iterations {
			if ctx.Err() != nil {
				break
			}
			if e := waitPace(ctx, execPace); e != nil {
				errs = append(errs, e)
				break
			}
			errs = append(errs, r.measure(ctx, "write_"+size.name, func(ctx context.Context) error { return r.api.fileWrite(ctx, base.ID, file, payload) }))
			errs = append(errs, r.measure(ctx, "read_"+size.name, func(ctx context.Context) error {
				read, e := r.api.fileRead(ctx, base.ID, file)
				if e == nil && !bytes.Equal(read, payload) {
					return fmt.Errorf("file read %s: content mismatch, got %d bytes, want %d", size.name, len(read), len(payload))
				}
				return e
			}))
		}
	}
	for range r.cfg.lifecycle {
		if ctx.Err() != nil {
			break
		}
		if e := r.measure(ctx, "stop", func(ctx context.Context) error { return r.api.stop(ctx, base.ID) }); e != nil {
			errs = append(errs, e)
			break
		}
		start := time.Now()
		e := r.api.start(ctx, base.ID)
		r.record("start", start, e, nil)
		if e != nil {
			errs = append(errs, e)
			break
		}
		_, e = r.execMeasured(ctx, "cold_first_exec", base.ID, "/bin/true")
		r.record("cold_start_first_exec", start, e, nil)
		errs = append(errs, e)
	}
	for range r.cfg.lifecycle {
		if ctx.Err() != nil {
			break
		}
		warmCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
		e := r.measure(warmCtx, "warm_wait", func(ctx context.Context) error { return r.waitWarm(ctx, []string{base.ID}) })
		cancel()
		if e != nil {
			errs = append(errs, fmt.Errorf("thermal warm not reached: %w", e))
			break
		}
		_, e = r.execMeasured(ctx, "warm_first_exec", base.ID, "/bin/true")
		errs = append(errs, e)
	}
	for range r.cfg.lifecycle {
		if ctx.Err() != nil {
			break
		}
		if e := waitPace(ctx, createPace); e != nil {
			errs = append(errs, e)
			break
		}
		_, e := r.execMeasured(ctx, "fork_source_keep_hot", base.ID, "/bin/true")
		if e != nil {
			errs = append(errs, e)
			break
		}
		fork := r.spec(r.cfg.tier, base.ID)
		fork.Network = "default"
		sb, e := r.createMeasured(ctx, "fork", fork)
		if e != nil {
			errs = append(errs, e)
			continue
		}
		errs = append(errs, r.destroyMeasured(ctx, "fork_destroy", sb.ID))
	}
	for range r.cfg.lifecycle {
		if ctx.Err() != nil {
			break
		}
		name := r.nextName()
		var snap snapshot
		e := r.measure(ctx, "snapshot_create", func(ctx context.Context) error { var e error; snap, e = r.api.snapshot(ctx, base.ID, name); return e })
		if e != nil {
			errs = append(errs, e)
			continue
		}
		if snap.Name == "" {
			errs = append(errs, errors.New("snapshot returned no name"))
			continue
		}
		r.trackSnapshot(snap.Name)
		var resumed sandbox
		e = r.measure(ctx, "snapshot_resume", func(ctx context.Context) error {
			var e error
			resumed, e = r.api.resume(ctx, snap.Name, r.nextName())
			return e
		})
		if e == nil {
			r.track(resumed.ID)
			_, e = r.execMeasured(ctx, "snapshot_first_exec", resumed.ID, "/bin/true")
			errs = append(errs, r.destroyMeasured(ctx, "snapshot_resumed_destroy", resumed.ID))
		}
		errs = append(errs, e)
		errs = append(errs, r.deleteSnapshotMeasured(ctx, snap.Name))
	}
	if r.cfg.cli != "" {
		errs = append(errs, r.baselineCLI(ctx, base.ID, createPace))
	}
	return errors.Join(errs...)
}
func (r *run) deleteSnapshotMeasured(ctx context.Context, name string) error {
	err := r.measure(ctx, "snapshot_delete", func(ctx context.Context) error { return r.api.deleteSnapshot(ctx, name) })
	if err == nil {
		r.mu.Lock()
		delete(r.snapshots, name)
		r.mu.Unlock()
	}
	return err
}
func (r *run) baselineCLI(ctx context.Context, source string, createPace *time.Ticker) error {
	if r.api.mode == "unix" && r.cfg.api != "unix:///var/lib/bhatti/api.sock" {
		return errors.New("CLI comparison requires the default local socket (CLI does not accept a custom unix:// URL)")
	}
	var errs []error
	call := func(args ...string) error {
		callCtx, cancel := context.WithTimeout(ctx, r.cfg.timeout)
		defer cancel()
		cmd := exec.CommandContext(callCtx, r.cfg.cli, args...)
		// Never expose the bench token in the process command line or results.
		env := append(os.Environ(), "BHATTI_TOKEN="+r.cfg.token)
		if r.api.mode != "unix" {
			env = append(env, "BHATTI_URL="+r.cfg.api)
		} else {
			env = append(env, "BHATTI_URL=")
		}
		cmd.Env = env
		output, err := cmd.CombinedOutput()
		if errors.Is(callCtx.Err(), context.DeadlineExceeded) {
			return fmt.Errorf("cli %s: %w", args[0], context.DeadlineExceeded)
		}
		if err != nil {
			if strings.Contains(string(output), "429 Too Many Requests") {
				return &apiError{Status: 429, Message: "CLI create/exec rate limited"}
			}
			return fmt.Errorf("cli %s: %w: %s", args[0], err, strings.TrimSpace(string(output)))
		}
		return nil
	}
	for range r.cfg.iterations {
		if ctx.Err() != nil {
			break
		}
		errs = append(errs, r.measure(ctx, "cli_exec_true", func(context.Context) error { return call("exec", source, "--", "/bin/true") }))
	}
	for range r.cfg.lifecycle {
		if ctx.Err() != nil {
			break
		}
		if e := waitPace(ctx, createPace); e != nil {
			errs = append(errs, e)
			break
		}
		name := r.nextName()
		image := r.cfg.tier
		if r.cfg.image != "" {
			image = r.cfg.image
		}
		spec := []string{"create", "--name", name, "--image", image, "--cpus", strconv.Itoa(r.cfg.cpus), "--memory", strconv.Itoa(r.cfg.memory)}
		switch r.cfg.network {
		case "public":
			spec = append(spec, "--net")
		case "none", "deny":
			spec = append(spec, "--egress", r.cfg.network)
		case "allow-siblings":
			spec = append(spec, "--allow-siblings")
		case "public-siblings":
			spec = append(spec, "--net", "--allow-siblings")
		}
		e := r.measure(ctx, "cli_create", func(context.Context) error { return call(spec...) })
		if e != nil {
			errs = append(errs, e)
			continue
		}
		// The CLI name resolves through the same owner-scoped API as an ID.
		r.track(name)
		errs = append(errs, r.destroyMeasured(ctx, "cli_created_destroy", name))
	}
	return errors.Join(errs...)
}

// burst simultaneously releases all workers after they are ready. The returned
// errors correspond to worker indexes; callers record each operation separately.
func burst(n int, fn func(int) error) ([]error, time.Time, time.Duration) {
	errs := make([]error, n)
	gate := make(chan struct{})
	var ready, done sync.WaitGroup
	ready.Add(n)
	done.Add(n)
	for i := range n {
		go func(i int) { defer done.Done(); ready.Done(); <-gate; errs[i] = fn(i) }(i)
	}
	ready.Wait()
	start := time.Now()
	close(gate)
	done.Wait()
	return errs, start, time.Since(start)
}
func (r *run) ramp() error {
	switch r.cfg.op {
	case "create", "fork", "resume", "wake":
	default:
		return fmt.Errorf("-op must be create|fork|resume|wake, got %q", r.cfg.op)
	}
	levels, err := parsePositiveLevels(r.cfg.levels)
	if err != nil {
		return err
	}
	ctx := r.ctx
	var wakePace *time.Ticker
	if r.cfg.op == "wake" {
		wakePace = time.NewTicker(2100 * time.Millisecond)
		defer wakePace.Stop()
	}
	var source sandbox
	var snap snapshot
	if r.cfg.op == "fork" || r.cfg.op == "resume" {
		spec := r.spec(r.cfg.tier, "")
		spec.KeepHot = true
		source, err = r.createMeasured(ctx, "setup_source_create", spec)
		if err != nil {
			return err
		}
	}
	if r.cfg.op == "resume" {
		name := r.nextName()
		err = r.measure(ctx, "setup_snapshot_create", func(ctx context.Context) error { var e error; snap, e = r.api.snapshot(ctx, source.ID, name); return e })
		if err != nil {
			return err
		}
		r.trackSnapshot(snap.Name)
	}
	var allErrs []error
	batches := make([]map[string]any, 0, len(levels))
	for _, level := range levels {
		if ctx.Err() != nil {
			break
		}
		ids := make([]string, level)
		if r.cfg.op == "wake" {
			for i := range ids {
				if e := waitPace(ctx, wakePace); e != nil {
					return errors.Join(append(allErrs, e)...)
				}
				sb, e := r.createMeasured(ctx, "setup_wake_create", r.spec(r.cfg.tier, ""))
				if e != nil {
					return errors.Join(append(allErrs, e)...)
				}
				ids[i] = sb.ID
				if e := r.measure(ctx, "setup_wake_stop", func(ctx context.Context) error { return r.api.stop(ctx, sb.ID) }); e != nil {
					return errors.Join(append(allErrs, e)...)
				}
			}
		}
		opName := fmt.Sprintf("%s_L%d", r.cfg.op, level)
		results, batchStart, wall := burst(level, func(i int) error {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			switch r.cfg.op {
			case "create", "fork":
				spec := r.spec(r.cfg.tier, "")
				if r.cfg.op == "fork" {
					spec.From = source.ID
					spec.Network = "default"
				}
				sb, e := r.createMeasured(ctx, opName, spec)
				if e == nil {
					ids[i] = sb.ID
				}
				return e
			case "resume":
				start := time.Now()
				sb, e := r.api.resume(ctx, snap.Name, r.nextName())
				r.record(opName, start, e, nil)
				if e == nil {
					ids[i] = sb.ID
					r.track(sb.ID)
				}
				return e
			default:
				_, e := r.execMeasured(ctx, opName, ids[i], "/bin/true")
				return e
			}
		})
		var batchErr error
		success := 0
		for _, e := range results {
			if e == nil {
				success++
			} else {
				allErrs = append(allErrs, e)
				batchErr = errors.Join(batchErr, e)
			}
		}
		// Batch wall includes the barrier release through completion of the slowest op.
		r.recordAt("batch_"+opName, batchStart, float64(wall)/float64(time.Millisecond), batchErr, map[string]float64{"requested": float64(level), "completed": float64(success), "ops_per_s": float64(success) / wall.Seconds()})
		batches = append(batches, map[string]any{"op": r.cfg.op, "level": level, "wall_ms": float64(wall) / float64(time.Millisecond), "ok": success, "errors": level - success, "ops_per_s": float64(success) / wall.Seconds()})
		for _, id := range ids {
			if id != "" {
				if e := r.destroyMeasured(ctx, "level_cleanup_destroy", id); e != nil {
					allErrs = append(allErrs, e)
				}
			}
		}
	}
	r.extra["batches"] = batches
	return errors.Join(allErrs...)
}
func (r *run) execLoad() error {
	ctx := r.ctx
	ids := make([]string, 0, r.cfg.n)
	for range r.cfg.n {
		sb, err := r.createMeasured(ctx, "setup_create", r.spec(r.cfg.tier, ""))
		if err != nil {
			return err
		}
		ids = append(ids, sb.ID)
	}
	var completed, failed atomic.Int64
	start := time.Now()
	end := start.Add(r.cfg.duration)
	var wg sync.WaitGroup
	for _, id := range ids {
		for range r.cfg.c {
			wg.Add(1)
			go func(id string) {
				defer wg.Done()
				for time.Now().Before(end) && ctx.Err() == nil {
					_, err := r.execMeasured(ctx, "exec_hot_true", id, "/bin/true")
					if err != nil {
						failed.Add(1)
					} else {
						completed.Add(1)
					}
					pauseAfter429(ctx, err, 200*time.Millisecond)
				}
			}(id)
		}
	}
	wg.Wait()
	elapsed := time.Since(start)
	r.setWindow("exec_hot_true", elapsed)
	r.extra["window_seconds"] = elapsed.Seconds()
	r.extra["completed_ops"] = completed.Load()
	r.extra["failed_ops"] = failed.Load()
	r.extra["successful_ops_per_s"] = float64(completed.Load()) / elapsed.Seconds()
	return ctx.Err()
}
func (r *run) swarm() error {
	ctx := r.ctx
	payload := bytes.Repeat([]byte("bench-agent-"), 10000)[:100*1024]
	var loops, errorsSeen, successfulLoops atomic.Int64
	start := time.Now()
	end := start.Add(r.cfg.duration)
	var wg sync.WaitGroup
	for worker := range r.cfg.n {
		wg.Add(1)
		go func(worker int) {
			defer wg.Done()
			rng := rand.New(rand.NewPCG(uint64(time.Now().UnixNano()), uint64(worker)))
			iteration := 0
			for time.Now().Before(end) && ctx.Err() == nil {
				loopStart := time.Now()
				var loopErr error
				sb, e := r.createMeasured(ctx, "swarm_create", r.spec(r.cfg.tier, ""))
				createErr := e
				if e != nil {
					loopErr = e
				} else {
					iteration++
					for range r.cfg.k {
						if ctx.Err() != nil {
							break
						}
						_, e = r.execMeasured(ctx, "swarm_exec", sb.ID, "/bin/true")
						loopErr = errors.Join(loopErr, e)
					}
					path := "/tmp/" + r.prefix + "-agent"
					e = r.measure(ctx, "swarm_write_100k", func(ctx context.Context) error { return r.api.fileWrite(ctx, sb.ID, path, payload) })
					loopErr = errors.Join(loopErr, e)
					if e == nil {
						e = r.measure(ctx, "swarm_read_100k", func(ctx context.Context) error {
							got, e := r.api.fileRead(ctx, sb.ID, path)
							if e == nil && !bytes.Equal(got, payload) {
								return fmt.Errorf("swarm file read mismatch: got %d bytes", len(got))
							}
							return e
						})
						loopErr = errors.Join(loopErr, e)
					}
					if iteration%3 == 0 && ctx.Err() == nil {
						fork := r.spec(r.cfg.tier, sb.ID)
						fork.Network = "default"
						child, e := r.createMeasured(ctx, "swarm_fork", fork)
						loopErr = errors.Join(loopErr, e)
						if e == nil {
							_, e = r.execMeasured(ctx, "swarm_fork_exec", child.ID, "/bin/true")
							loopErr = errors.Join(loopErr, e)
							loopErr = errors.Join(loopErr, r.destroyMeasured(ctx, "swarm_fork_destroy", child.ID))
						}
					}
					loopErr = errors.Join(loopErr, r.destroyMeasured(ctx, "swarm_destroy", sb.ID))
				}
				r.record("swarm_loop", loopStart, loopErr, nil)
				loops.Add(1)
				if loopErr != nil {
					errorsSeen.Add(1)
				} else {
					successfulLoops.Add(1)
				}
				pauseAfter429(ctx, createErr, 2*time.Second)
				if r.cfg.think > 0 {
					delay := time.Duration(rng.Int64N(int64(r.cfg.think)))
					timer := time.NewTimer(delay)
					select {
					case <-timer.C:
					case <-ctx.Done():
						timer.Stop()
						return
					}
				}
			}
		}(worker)
	}
	wg.Wait()
	elapsed := time.Since(start)
	for _, name := range []string{"swarm_create", "swarm_exec", "swarm_write_100k", "swarm_read_100k", "swarm_fork", "swarm_fork_exec", "swarm_fork_destroy", "swarm_destroy", "swarm_loop"} {
		r.setWindow(name, elapsed)
	}
	r.extra["window_seconds"] = elapsed.Seconds()
	r.extra["loops"] = loops.Load()
	r.extra["failed_loops"] = errorsSeen.Load()
	r.extra["successful_loops"] = successfulLoops.Load()
	r.extra["successful_loops_per_s"] = float64(successfulLoops.Load()) / elapsed.Seconds()
	r.extra["loops_per_s"] = float64(loops.Load()) / elapsed.Seconds()
	return ctx.Err()
}
func densityPoint(start, at HostSample, state string, n int) map[string]any {
	var perSandbox any
	if n > 0 && at.PSSComplete && at.Scope == "bench_only" && at.VMMCount == n {
		perSandbox = at.VMMPSSMB / float64(n)
	}
	return map[string]any{
		"state": state, "sandboxes": n,
		"host_mem_available_mb": at.MemAvailableMB,
		"host_mem_delta_mb":     start.MemAvailableMB - at.MemAvailableMB,
		"vmm_count":             at.VMMCount, "vmm_pss_mb": at.VMMPSSMB,
		"vmm_pss_per_sandbox_mb": perSandbox,
		"vmm_pss_values_mb":      at.VMMPSSValuesMB,
		"pss_complete":           at.PSSComplete, "scope": at.Scope,
		"scope_reason": at.ScopeReason,
	}
}

func (r *run) density() error {
	if r.sampler == nil {
		return errors.New("density requires Linux host sampling; enable -sample")
	}
	var points []map[string]any
	defer func() { r.extra["density_points"] = points }()
	start := r.sampler.Sample(time.Now())
	pacer := time.NewTicker(2100 * time.Millisecond)
	defer pacer.Stop()
	ids := make([]string, 0, r.cfg.n)
	for i := 1; i <= r.cfg.n; i++ {
		if r.ctx.Err() != nil {
			return r.ctx.Err()
		}
		group := (i-1)/r.cfg.step + 1
		if err := waitPace(r.ctx, pacer); err != nil {
			return err
		}
		sb, err := r.createMeasured(r.ctx, "density_create_"+strconv.Itoa(group), r.spec(r.cfg.tier, ""))
		if err != nil {
			return err
		}
		ids = append(ids, sb.ID)
		if i%r.cfg.step == 0 || i == r.cfg.n {
			// Pacing exceeds the 30-second thermal window at larger N. Reheat
			// earlier VMs before each capture so a "hot" point is all-hot.
			for _, id := range ids {
				if _, err := r.execMeasured(r.ctx, "density_hot_refresh", id, "/bin/true"); err != nil {
					return err
				}
			}
			at := r.sampler.Sample(time.Now())
			points = append(points, densityPoint(start, at, "hot", i))
		}
	}
	warmCtx, cancel := context.WithTimeout(r.ctx, 2*time.Minute)
	err := r.measure(warmCtx, "density_wait_warm", func(ctx context.Context) error { return r.waitWarm(ctx, ids) })
	cancel()
	if err == nil {
		at := r.sampler.Sample(time.Now())
		points = append(points, densityPoint(start, at, "warm", len(ids)))
	}
	return err
}
