// bench-load measures complete daemon API calls without a CLI process per operation.
package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

type options struct {
	api, token, tokenFile, out, tier, image, network, domain, url, levels, op, cli, dataDir, ownerID string
	iterations, lifecycle, n, c, k, fanIn, aliases, step, connectSamples, cpus, memory               int
	duration, think, timeout, netTimeout                                                             time.Duration
	sample                                                                                           bool
}

type run struct {
	cfg           options
	api           *apiClient
	prefix        string
	ctx           context.Context
	mu            sync.Mutex
	samples       []Sample
	ioErr         error
	sampleEncoder *json.Encoder
	owned         map[string]struct{}
	snapshots     map[string]struct{}
	extra         map[string]any
	windows       map[string]time.Duration
	sampler       *HostSampler
}

func main() {
	if err := execute(os.Args[1:]); err != nil {
		log.Print(err)
		os.Exit(1)
	}
}

func execute(args []string) error {
	if len(args) == 0 || args[0] == "help" || args[0] == "-h" || args[0] == "--help" {
		fmt.Fprintln(os.Stderr, "usage: bench-load <baseline|ramp|exec|swarm|proxy|net|density> [flags]\nflags follow the scenario name; see bench/load/README.md")
		return nil
	}
	scenario := args[0]
	cfg, err := parseFlags(scenario, args[1:])
	if err != nil {
		return err
	}
	if cfg.tokenFile != "" {
		b, err := os.ReadFile(cfg.tokenFile)
		if err != nil {
			return fmt.Errorf("read token file: %w", err)
		}
		cfg.token = strings.TrimSpace(string(b))
	}
	if cfg.token == "" {
		return errors.New("bench user token required (-token, -token-file or BHATTI_TOKEN); refusing to use admin credentials")
	}
	api, err := newAPI(cfg.api, cfg.token, cfg.timeout)
	if err != nil {
		return err
	}
	var id [6]byte
	if _, err := rand.Read(id[:]); err != nil {
		return err
	}
	prefix := "bench-" + strconv.FormatInt(time.Now().Unix(), 36) + "-" + hex.EncodeToString(id[:])
	started := time.Now().UTC()
	outDir := filepath.Join(cfg.out, started.Format("20060102T150405.000000000Z")+"-"+scenario)
	if err := os.MkdirAll(outDir, 0755); err != nil {
		return fmt.Errorf("make result dir: %w", err)
	}
	f, err := os.Create(filepath.Join(outDir, "samples.jsonl"))
	if err != nil {
		return err
	}
	defer f.Close()
	hf, err := os.Create(filepath.Join(outDir, "host.jsonl"))
	if err != nil {
		return err
	}
	defer hf.Close()
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	r := &run{cfg: cfg, api: api, prefix: prefix, ctx: ctx, owned: make(map[string]struct{}), snapshots: make(map[string]struct{}), sampleEncoder: json.NewEncoder(f), extra: make(map[string]any), windows: make(map[string]time.Duration)}
	defer func() {
		// Cleanup uses an independent context even when SIGINT cancelled the load.
		cleanCtx, cleanCancel := context.WithTimeout(context.Background(), 4*time.Minute)
		defer cleanCancel()
		if err := r.cleanup(cleanCtx); err != nil {
			log.Printf("CLEANUP FAILED (prefix %s): %v", r.prefix, err)
		}
	}()
	host := hostInfo(cfg.dataDir)
	meta := map[string]any{"started_at": started, "scenario": scenario, "parameters": cfg.meta(), "host": host, "api_mode": api.mode, "git_sha": gitSHA(), "server_version": api.version(ctx), "run_prefix": prefix, "go_version": runtime.Version()}
	if err := writeJSON(filepath.Join(outDir, "meta.json"), meta); err != nil {
		return err
	}
	if cfg.sample {
		r.sampler = newHostSampler(hf, cfg.dataDir, cfg.ownerID)
		samCtx, stopSampler := context.WithCancel(context.Background())
		var finished sync.WaitGroup
		finished.Add(1)
		go func() { defer finished.Done(); r.sampler.Run(samCtx) }()
		defer func() { stopSampler(); finished.Wait() }()
	} else {
		// Keep a parseable host.jsonl, even when sampling is explicitly disabled.
		_, _ = hf.WriteString("{\"supported\":false,\"reason\":\"sampling disabled\"}\n")
	}
	var runErr error
	switch scenario {
	case "baseline":
		runErr = r.baseline()
	case "ramp":
		runErr = r.ramp()
	case "exec":
		runErr = r.execLoad()
	case "swarm":
		runErr = r.swarm()
	case "proxy":
		runErr = r.proxy()
	case "net":
		runErr = r.net()
	case "density":
		runErr = r.density()
	}
	if r.sampler != nil {
		r.extra["host_end_of_workload"] = r.sampler.Sample(time.Now())
	}
	// Include cleanup failures in a completed run rather than silently leaving VMs.
	cleanCtx, cleanCancel := context.WithTimeout(context.Background(), 4*time.Minute)
	cleanupErr := r.cleanup(cleanCtx)
	cleanCancel()
	if cleanupErr != nil {
		runErr = errors.Join(runErr, cleanupErr)
	}
	if err := f.Sync(); err != nil {
		runErr = errors.Join(runErr, err)
	}
	r.mu.Lock()
	if r.ioErr != nil {
		runErr = errors.Join(runErr, r.ioErr)
	}
	ops := summarize(r.samples, time.Since(started))
	for name, window := range r.windows {
		if s, ok := ops[name]; ok && window > 0 {
			s.ThroughputOpsPerS = float64(s.Count) / window.Seconds()
			ops[name] = s
		}
	}
	summary := map[string]any{"scenario": scenario, "started_at": started, "ended_at": time.Now().UTC(), "elapsed_seconds": time.Since(started).Seconds(), "operations": ops, "details": r.extra, "run_prefix": prefix}
	r.mu.Unlock()
	if runErr != nil {
		summary["error"] = runErr.Error()
	}
	if err := writeJSON(filepath.Join(outDir, "summary.json"), summary); err != nil {
		runErr = errors.Join(runErr, err)
	}
	printSummary(outDir, summary["operations"].(map[string]OpSummary), r.extra)
	return runErr
}

func parseFlags(scenario string, args []string) (options, error) {
	switch scenario {
	case "baseline", "ramp", "exec", "swarm", "proxy", "net", "density":
	default:
		return options{}, fmt.Errorf("unknown scenario %q", scenario)
	}
	cfg := options{api: "unix:///var/lib/bhatti/api.sock", token: os.Getenv("BHATTI_TOKEN"), out: "results", tier: "minimal", network: "default", levels: "1,5,10,25,50", op: "create", iterations: 30, lifecycle: 15, n: 10, c: 4, k: 5, fanIn: 100, aliases: 10, step: 10, connectSamples: 30, duration: 60 * time.Second, timeout: 2 * time.Minute, netTimeout: 30 * time.Second, dataDir: "/var/lib/bhatti", url: "https://download.thinkbroadband.com/100MB.zip"}
	switch scenario {
	case "swarm":
		cfg.duration = 600 * time.Second
	case "proxy":
		cfg.c = 1
		cfg.duration = 15 * time.Second
		cfg.levels = "1,10,50"
	case "net":
		cfg.n = 5
		cfg.levels = "1,5,10"
	case "density":
		cfg.n = 30
	}
	if _, err := os.Stat("/proc/stat"); err == nil {
		cfg.sample = true
	}
	fs := flag.NewFlagSet(scenario, flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	fs.StringVar(&cfg.api, "api", cfg.api, "daemon URL or unix:///path/to/api.sock")
	fs.StringVar(&cfg.token, "token", cfg.token, "bench user's API key (prefer -token-file)")
	fs.StringVar(&cfg.tokenFile, "token-file", "", "file with bench user's API key")
	fs.StringVar(&cfg.out, "out", cfg.out, "result directory parent")
	fs.StringVar(&cfg.tier, "tier", cfg.tier, "rootfs tier")
	fs.StringVar(&cfg.image, "image", "", "create from image (optional)")
	fs.StringVar(&cfg.network, "network", cfg.network, "network policy (net sibling test uses allow-siblings)")
	fs.StringVar(&cfg.domain, "domain", "", "public proxy zone (e.g. agni-02.karkhana.dev)")
	fs.StringVar(&cfg.url, "url", cfg.url, "public download URL for net scenario")
	fs.StringVar(&cfg.levels, "levels", cfg.levels, "comma-separated concurrency levels (ramp; proxy hot counts; net guest counts)")
	fs.StringVar(&cfg.op, "op", cfg.op, "ramp operation: create|fork|resume|wake")
	fs.StringVar(&cfg.cli, "cli", "", "baseline only: local bhatti CLI executable for separately measured CLI overhead")
	fs.StringVar(&cfg.dataDir, "data-dir", cfg.dataDir, "daemon data dir, used for filesystem metadata")
	fs.StringVar(&cfg.ownerID, "owner-id", "", "bench user's daemon ID, for scoped host VMM/netd PSS when available")
	fs.IntVar(&cfg.iterations, "iterations", cfg.iterations, "baseline sequential cheap-op repetitions")
	fs.IntVar(&cfg.lifecycle, "lifecycle", cfg.lifecycle, "baseline sequential lifecycle repetitions")
	fs.IntVar(&cfg.n, "n", cfg.n, "sandbox/agent count")
	fs.IntVar(&cfg.c, "c", cfg.c, "concurrent exec loops per sandbox (exec scenario)")
	fs.IntVar(&cfg.k, "k", cfg.k, "execs per swarm loop")
	fs.IntVar(&cfg.fanIn, "fan-in", cfg.fanIn, "proxy cold fan-in requests")
	fs.IntVar(&cfg.aliases, "aliases", cfg.aliases, "proxy multi-cold alias count")
	fs.IntVar(&cfg.step, "step", cfg.step, "density capture interval")
	fs.IntVar(&cfg.connectSamples, "connect-samples", cfg.connectSamples, "TCP connect samples inside guest")
	fs.IntVar(&cfg.cpus, "cpus", 1, "vCPUs per sandbox")
	fs.IntVar(&cfg.memory, "memory", 512, "guest memory MB")
	fs.DurationVar(&cfg.duration, "duration", cfg.duration, "exec/swarm steady-state window, or proxy hot window per concurrency level")
	fs.DurationVar(&cfg.think, "think", 200*time.Millisecond, "maximum randomized swarm think time")
	fs.DurationVar(&cfg.timeout, "timeout", cfg.timeout, "per API operation timeout")
	fs.DurationVar(&cfg.netTimeout, "net-timeout", cfg.netTimeout, "per guest download duration cap")
	fs.BoolVar(&cfg.sample, "sample", cfg.sample, "sample Linux host /proc every second")
	if err := fs.Parse(args); err != nil {
		return cfg, err
	}
	if len(fs.Args()) != 0 {
		return cfg, fmt.Errorf("unexpected arguments: %s", strings.Join(fs.Args(), " "))
	}
	if cfg.n < 1 || cfg.c < 1 || cfg.k < 1 || cfg.step < 1 || cfg.iterations < 1 || cfg.lifecycle < 1 || cfg.fanIn < 1 || cfg.aliases < 1 || cfg.connectSamples < 1 || cfg.cpus < 1 || cfg.memory < 128 || cfg.duration <= 0 || cfg.timeout <= 0 || cfg.netTimeout <= 0 || cfg.think < 0 {
		return cfg, errors.New("counts and durations must be positive (think time may be zero; memory >=128)")
	}
	return cfg, nil
}

func (o options) meta() map[string]any {
	// Explicit whitelist: never persist the token, token file path or CLI environment.
	return map[string]any{"api": o.api, "out": o.out, "tier": o.tier, "image": o.image, "network": o.network, "domain": o.domain, "url": o.url, "levels": o.levels, "op": o.op, "owner_id": o.ownerID, "iterations": o.iterations, "lifecycle": o.lifecycle, "n": o.n, "c": o.c, "k": o.k, "fan_in": o.fanIn, "aliases": o.aliases, "step": o.step, "connect_samples": o.connectSamples, "cpus": o.cpus, "memory_mb": o.memory, "duration": o.duration.String(), "think": o.think.String(), "timeout": o.timeout.String(), "net_timeout": o.netTimeout.String(), "sample": o.sample, "cli": o.cli != ""}
}

func (r *run) nextName() string {
	var b [4]byte
	_, _ = rand.Read(b[:])
	return r.prefix + "-" + hex.EncodeToString(b[:])
}

func (r *run) record(op string, start time.Time, err error, values map[string]float64) {
	r.recordAt(op, start, float64(time.Since(start))/float64(time.Millisecond), err, values)
}

// recordAt preserves guest-measured durations (TCP connect) separately from
// the host API exec round trip that transported those samples.
func (r *run) recordAt(op string, start time.Time, latencyMS float64, err error, values map[string]float64) {
	outcome := "ok"
	if err != nil {
		outcome = "err"
		var ae *apiError
		if errors.As(err, &ae) && ae.Status == 429 {
			outcome = "429"
		}
		var ne net.Error
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, os.ErrDeadlineExceeded) || errors.As(err, &ne) && ne.Timeout() {
			outcome = "timeout"
		}
	}
	s := Sample{Op: op, StartedAt: start.UTC(), LatencyMS: latencyMS, Outcome: outcome, Values: values}
	if err != nil {
		s.Error = err.Error()
	}
	r.mu.Lock()
	r.samples = append(r.samples, s)
	if encErr := r.sampleEncoder.Encode(s); encErr != nil {
		if r.ioErr == nil {
			r.ioErr = fmt.Errorf("write samples: %w", encErr)
		}
	}
	r.mu.Unlock()
}

func (r *run) measure(ctx context.Context, op string, fn func(context.Context) error) error {
	start := time.Now()
	err := fn(ctx)
	r.record(op, start, err, nil)
	return err
}

func (r *run) setWindow(op string, d time.Duration) {
	r.mu.Lock()
	r.windows[op] = d
	r.mu.Unlock()
}

func (r *run) track(id string)         { r.mu.Lock(); r.owned[id] = struct{}{}; r.mu.Unlock() }
func (r *run) untrack(id string)       { r.mu.Lock(); delete(r.owned, id); r.mu.Unlock() }
func (r *run) trackSnapshot(id string) { r.mu.Lock(); r.snapshots[id] = struct{}{}; r.mu.Unlock() }

func (r *run) cleanup(ctx context.Context) error {
	// List by user and exact unique prefix to recover even creates whose response timed out.
	list, listErr := r.api.list(ctx)
	if listErr == nil {
		for _, sb := range list {
			if strings.HasPrefix(sb.Name, r.prefix+"-") {
				r.track(sb.ID)
			}
		}
	}
	snapList, snapListErr := r.api.listSnapshots(ctx)
	if snapListErr == nil {
		for _, snap := range snapList {
			if strings.HasPrefix(snap.Name, r.prefix+"-") {
				r.trackSnapshot(snap.Name)
			}
		}
	}
	r.mu.Lock()
	ids := make([]string, 0, len(r.owned))
	for id := range r.owned {
		ids = append(ids, id)
	}
	snaps := make([]string, 0, len(r.snapshots))
	for id := range r.snapshots {
		snaps = append(snaps, id)
	}
	r.mu.Unlock()
	var errs []error
	for _, id := range ids {
		if err := r.api.destroy(ctx, id); err != nil {
			errs = append(errs, fmt.Errorf("destroy %s: %w", id, err))
		} else {
			r.untrack(id)
		}
	}
	for _, id := range snaps {
		if err := r.api.deleteSnapshot(ctx, id); err != nil {
			errs = append(errs, fmt.Errorf("delete snapshot %s: %w", id, err))
		} else {
			r.mu.Lock()
			delete(r.snapshots, id)
			r.mu.Unlock()
		}
	}
	if listErr != nil {
		errs = append(errs, fmt.Errorf("list owned sandboxes for cleanup: %w", listErr))
	}
	if snapListErr != nil {
		errs = append(errs, fmt.Errorf("list owned snapshots for cleanup: %w", snapListErr))
	}
	return errors.Join(errs...)
}

var buildSHA string

func gitSHA() string {
	out, err := exec.Command("git", "rev-parse", "HEAD").Output()
	if err != nil {
		if buildSHA != "" {
			return buildSHA
		}
		return "unavailable"
	}
	return strings.TrimSpace(string(out))
}

func writeJSON(path string, v any) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	encErr := json.NewEncoder(f).Encode(v)
	return errors.Join(encErr, f.Close())
}

func printSummary(dir string, summaries map[string]OpSummary, extra map[string]any) {
	fmt.Printf("\nresults: %s\n%-32s %7s %7s %8s %8s %8s %8s %8s %8s %8s\n", dir, "operation", "count", "err%", "p50 ms", "p90 ms", "p95 ms", "p99 ms", "max ms", "mean ms", "ops/s")
	keys := make([]string, 0, len(summaries))
	for k := range summaries {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		s := summaries[k]
		fmt.Printf("%-32s %7d %6.1f%% %8.2f %8.2f %8.2f %8.2f %8.2f %8.2f %8.2f\n", k, s.Count, s.ErrRate*100, s.P50MS, s.P90MS, s.P95MS, s.P99MS, s.MaxMS, s.MeanMS, s.ThroughputOpsPerS)
	}
	if len(extra) > 0 {
		b, _ := json.MarshalIndent(extra, "", "  ")
		fmt.Printf("details: %s\n", b)
	}
}
