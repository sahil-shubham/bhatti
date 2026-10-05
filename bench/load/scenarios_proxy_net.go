package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	benchServerPort = 3000
	// The peer serves 32 MiB; the curl limit also bounds user-supplied download URLs.
	peerBytes    = 32 << 20
	maxCurlBytes = 120_000_000
)

const benchServerJS = `const http = require('node:http');
const {Readable} = require('node:stream');
const bytes = 32 * 1024 * 1024;
const chunk = Buffer.alloc(64 * 1024, 0x61);
http.createServer((req, res) => {
  if (req.url === '/payload') {
    res.writeHead(200, {'Content-Type': 'application/octet-stream', 'Content-Length': String(bytes)});
    Readable.from((function* () {
      for (let n = 0; n < bytes; n += chunk.length) yield chunk;
    })()).pipe(res);
    return;
  }
  res.writeHead(200, {'Content-Type': 'text/plain', 'Content-Length': '3'});
  res.end('ok\n');
}).listen(3000, '0.0.0.0');`

func benchServerInit() string {
	// The init session is re-launched on guest boot. A one-shot detached exec
	// runs only until explicit Stop powers off the VM; it is absent on cold wake.
	return "exec node -e '" + strings.ReplaceAll(benchServerJS, "'", "'\\''") + "'"
}

func parsePositiveLevels(raw string) ([]int, error) {
	parts := strings.Split(raw, ",")
	levels := make([]int, 0, len(parts))
	for _, part := range parts {
		part = strings.TrimSpace(part)
		level, err := strconv.Atoi(part)
		if err != nil || level <= 0 {
			return nil, fmt.Errorf("invalid positive level %q in -levels %q", part, raw)
		}
		levels = append(levels, level)
	}
	return levels, nil
}

func (r *run) scenarioCreate(op, tier, networkPolicy, init string) (sandbox, error) {
	var sb sandbox
	err := r.measure(r.ctx, op, func(ctx context.Context) error {
		var createErr error
		sb, createErr = r.api.create(ctx, createSpec{
			Name: r.nextName(), Tier: tier, Image: r.cfg.image,
			Network: networkPolicy, CPUs: r.cfg.cpus, Memory: r.cfg.memory, Init: init,
		})
		if createErr != nil {
			return createErr
		}
		if sb.ID == "" {
			return errors.New("create response omitted sandbox ID; prefix cleanup will find it")
		}
		r.track(sb.ID)
		if r.sampler != nil && sb.CreatedBy != "" && r.cfg.ownerID == "" {
			r.sampler.SetBenchOwner(sb.CreatedBy)
		}
		return nil
	})
	return sb, err
}

func (r *run) scenarioDestroy(op, id string) error {
	// Cleanup must remain possible after the load context was cancelled.
	ctx, cancel := context.WithTimeout(context.Background(), r.cfg.timeout)
	defer cancel()
	return r.measure(ctx, op, func(ctx context.Context) error {
		err := r.api.destroy(ctx, id)
		if err == nil {
			r.untrack(id)
		}
		return err
	})
}

func (r *run) startBenchServer(id string, launch bool) error {
	return r.measure(r.ctx, "server.launch", func(ctx context.Context) error {
		// The sibling-only test launches a detached server; the public proxy
		// server instead starts from the guest's boot init session after cold wake.
		if launch {
			if err := r.api.execDetached(ctx, id, "node", "-e", benchServerJS); err != nil {
				return err
			}
		}
		readyCtx, cancel := context.WithTimeout(ctx, min(r.cfg.timeout, 15*time.Second))
		defer cancel()
		tick := time.NewTicker(150 * time.Millisecond)
		defer tick.Stop()
		var lastErr error
		for {
			_, lastErr = r.api.exec(readyCtx, id, "curl", "-fsS", "--max-time", "2", "-o", "/dev/null", "http://127.0.0.1:3000/")
			if lastErr == nil {
				return nil
			}
			select {
			case <-readyCtx.Done():
				return fmt.Errorf("guest server on :3000 not ready: %w", errors.Join(readyCtx.Err(), lastErr))
			case <-tick.C:
			}
		}
	})
}

func (r *run) publishBenchServer(id, alias, domain string) (string, error) {
	var publicURL string
	err := r.measure(r.ctx, "proxy.publish", func(ctx context.Context) error {
		var publishErr error
		publicURL, publishErr = r.api.publish(ctx, id, alias, benchServerPort)
		if publishErr != nil {
			return publishErr
		}
		u, parseErr := url.Parse(publicURL)
		if parseErr != nil || u.Scheme != "https" || !strings.EqualFold(u.Hostname(), alias+"."+domain) || u.User != nil || u.Fragment != "" {
			return fmt.Errorf("publish returned %q, expected a valid HTTPS URL at %s.%s", publicURL, alias, domain)
		}
		return nil
	})
	return publicURL, err
}

func newProxyClient(maxClients int) *http.Client {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.MaxIdleConns = max(100, maxClients+8)
	tr.MaxIdleConnsPerHost = max(100, maxClients+8)
	return &http.Client{
		Transport: tr,
		// A redirect is a response, not a successful visit to the published app.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

func proxyGET(ctx context.Context, client *http.Client, publicURL string) (int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, publicURL, nil)
	if err != nil {
		return 0, err
	}
	// Never use the authenticated daemon API client for public traffic.
	resp, err := client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	_, err = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	if err != nil {
		return resp.StatusCode, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return resp.StatusCode, &apiError{Status: resp.StatusCode, Message: "public proxy returned " + resp.Status}
	}
	return resp.StatusCode, nil
}

type proxyTotals struct {
	attempts, ok, failed, rateLimited, timeouts int
	firstErr                                    error
}

func (t *proxyTotals) add(err error) {
	t.attempts++
	if err == nil {
		t.ok++
		return
	}
	t.failed++
	if t.firstErr == nil {
		t.firstErr = err
	}
	var apiErr *apiError
	if errors.As(err, &apiErr) && apiErr.Status == http.StatusTooManyRequests {
		t.rateLimited++
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) || errors.Is(err, http.ErrHandlerTimeout) {
		t.timeouts++
	}
}

func (t *proxyTotals) merge(other proxyTotals) {
	t.attempts += other.attempts
	t.ok += other.ok
	t.failed += other.failed
	t.rateLimited += other.rateLimited
	t.timeouts += other.timeouts
	if t.firstErr == nil {
		t.firstErr = other.firstErr
	}
}

func (t proxyTotals) result(phase string) error {
	if t.attempts == 0 {
		return fmt.Errorf("%s: no requests attempted", phase)
	}
	if t.failed != 0 {
		return fmt.Errorf("%s: %d/%d requests failed (first: %w)", phase, t.failed, t.attempts, t.firstErr)
	}
	return nil
}

func (r *run) proxyHot(client *http.Client, publicURL string, level int) error {
	op := fmt.Sprintf("proxy.hot.C%d", level)
	gate := make(chan struct{})
	results := make([]proxyTotals, level)
	var wg sync.WaitGroup
	wg.Add(level)
	var end time.Time
	for i := range results {
		go func(i int) {
			defer wg.Done()
			<-gate
			for r.ctx.Err() == nil {
				started := time.Now()
				if !started.Before(end) {
					return
				}
				// Reaching the end of the measurement window stops new requests,
				// but must not turn already in-flight requests into fake timeouts.
				requestCtx, cancel := context.WithTimeout(r.ctx, r.cfg.timeout)
				status, err := proxyGET(requestCtx, client, publicURL)
				cancel()
				r.record(op, started, err, map[string]float64{
					"clients": float64(level), "http_status": float64(status),
				})
				results[i].add(err)
				pauseAfter429(r.ctx, err, 200*time.Millisecond)
			}
		}(i)
	}
	start := time.Now()
	end = start.Add(r.cfg.duration)
	close(gate)
	wg.Wait()
	elapsed := time.Since(start)
	window := min(elapsed, r.cfg.duration)
	r.setWindow(op, window)
	var totals proxyTotals
	for _, result := range results {
		totals.merge(result)
	}
	r.extra["proxy_hot"] = appendProxyGroup(r.extra["proxy_hot"], map[string]any{
		"clients": level, "window_seconds": window.Seconds(), "configured_window_seconds": r.cfg.duration.Seconds(),
		"elapsed_seconds": elapsed.Seconds(), "requests": totals.attempts, "ok": totals.ok, "errors": totals.failed,
		"rate_limited": totals.rateLimited, "timeouts": totals.timeouts,
		"requests_per_second":            float64(totals.attempts) / window.Seconds(),
		"successful_requests_per_second": float64(totals.ok) / window.Seconds(),
	})
	return totals.result(fmt.Sprintf("proxy hot clients=%d", level))
}

func appendProxyGroup(existing any, group map[string]any) []map[string]any {
	if groups, ok := existing.([]map[string]any); ok {
		return append(groups, group)
	}
	return []map[string]any{group}
}

func wakePercentile(latencies []float64, percentile float64) float64 {
	rank := int(math.Ceil(percentile*float64(len(latencies)))) - 1
	return latencies[rank]
}

func (r *run) proxyBurst(client *http.Client, publicURLs []string, requests int, op, extraKey string) error {
	type result struct {
		latencyMS float64
		err       error
	}
	results := make([]result, requests)
	gate := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(requests)
	for i := range results {
		go func(i int) {
			defer wg.Done()
			<-gate
			ctx, cancel := context.WithTimeout(r.ctx, r.cfg.timeout)
			defer cancel()
			started := time.Now()
			status, err := proxyGET(ctx, client, publicURLs[i%len(publicURLs)])
			results[i] = result{latencyMS: float64(time.Since(started)) / float64(time.Millisecond), err: err}
			r.record(op, started, err, map[string]float64{
				"http_status": float64(status), "alias_index": float64(i % len(publicURLs)),
			})
		}(i)
	}
	start := time.Now()
	close(gate)
	wg.Wait()
	elapsed := time.Since(start)
	var totals proxyTotals
	latencies := make([]float64, 0, requests)
	for _, result := range results {
		totals.add(result.err)
		if result.err == nil {
			latencies = append(latencies, result.latencyMS)
		}
	}
	sort.Float64s(latencies)
	group := map[string]any{
		"aliases": len(publicURLs), "requests": totals.attempts, "ok": totals.ok,
		"errors": totals.failed, "rate_limited": totals.rateLimited, "timeouts": totals.timeouts,
		"elapsed_seconds": elapsed.Seconds(), "requests_per_second": float64(totals.attempts) / elapsed.Seconds(),
		"successful_requests_per_second": float64(totals.ok) / elapsed.Seconds(),
		"wake_samples":                   len(latencies),
	}
	if len(latencies) > 0 {
		group["wake_p50_ms"] = wakePercentile(latencies, 0.50)
		group["wake_p95_ms"] = wakePercentile(latencies, 0.95)
		group["wake_p99_ms"] = wakePercentile(latencies, 0.99)
		group["wake_max_ms"] = wakePercentile(latencies, 1)
	}
	r.extra[extraKey] = group
	return totals.result(op)
}

func (r *run) proxySingle(client *http.Client, domain string, levels []int) (err error) {
	policy := r.cfg.network
	if policy == "default" || policy == "" {
		policy = "deny" // A proxy guest needs a network device, not public egress.
	}
	sb, err := r.scenarioCreate("proxy.create", "browser", policy, benchServerInit())
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, r.scenarioDestroy("proxy.destroy", sb.ID)) }()
	if err = r.startBenchServer(sb.ID, false); err != nil {
		return err
	}
	publicURL, err := r.publishBenchServer(sb.ID, r.prefix+"-hot", domain)
	if err != nil {
		return err
	}
	for _, level := range levels {
		if r.ctx.Err() != nil {
			return errors.Join(err, r.ctx.Err())
		}
		err = errors.Join(err, r.proxyHot(client, publicURL, level))
	}
	if r.ctx.Err() != nil {
		return errors.Join(err, r.ctx.Err())
	}
	if stopErr := r.measure(r.ctx, "proxy.stop", func(ctx context.Context) error { return r.api.stop(ctx, sb.ID) }); stopErr != nil {
		return errors.Join(err, stopErr)
	}
	return errors.Join(err, r.proxyBurst(client, []string{publicURL}, r.cfg.fanIn, "proxy.cold.request", "proxy_cold"))
}

func (r *run) proxyMultiCold(client *http.Client, domain string) (err error) {
	policy := r.cfg.network
	if policy == "default" || policy == "" {
		policy = "deny"
	}
	var ids []string
	defer func() {
		for _, id := range ids {
			err = errors.Join(err, r.scenarioDestroy("proxy.destroy", id))
		}
	}()
	urls := make([]string, 0, r.cfg.aliases)
	for i := range r.cfg.aliases {
		if r.ctx.Err() != nil {
			return errors.Join(err, r.ctx.Err())
		}
		sb, createErr := r.scenarioCreate("proxy.create", "browser", policy, benchServerInit())
		if createErr != nil {
			return errors.Join(err, createErr)
		}
		ids = append(ids, sb.ID)
		if launchErr := r.startBenchServer(sb.ID, false); launchErr != nil {
			return errors.Join(err, launchErr)
		}
		publicURL, publishErr := r.publishBenchServer(sb.ID, fmt.Sprintf("%s-cold-%d", r.prefix, i), domain)
		if publishErr != nil {
			return errors.Join(err, publishErr)
		}
		urls = append(urls, publicURL)
	}
	for _, id := range ids {
		if stopErr := r.measure(r.ctx, "proxy.stop", func(ctx context.Context) error { return r.api.stop(ctx, id) }); stopErr != nil {
			return errors.Join(err, stopErr)
		}
	}
	return r.proxyBurst(client, urls, len(urls), "proxy.multi_cold.request", "proxy_multi_cold")
}

func (r *run) proxy() error {
	levels, err := parsePositiveLevels(r.cfg.levels)
	if err != nil {
		return err
	}
	domain := strings.TrimSuffix(strings.ToLower(strings.TrimSpace(r.cfg.domain)), ".")
	if domain == "" {
		return errors.New("proxy requires -domain for verified public HTTPS requests")
	}
	maxClients := max(r.cfg.fanIn, r.cfg.aliases)
	for _, level := range levels {
		maxClients = max(maxClients, level)
	}
	client := newProxyClient(maxClients)
	defer client.CloseIdleConnections()
	singleErr := r.proxySingle(client, domain, levels)
	if r.ctx.Err() != nil {
		return errors.Join(singleErr, r.ctx.Err())
	}
	return errors.Join(singleErr, r.proxyMultiCold(client, domain))
}

// parseCurlMetrics accepts only the explicit numeric line printed by curl -w.
// Guest stderr and API execution duration are never mistaken for throughput.
func parseCurlMetrics(stdout, marker string, fields ...string) (map[string]float64, error) {
	at := strings.LastIndex(stdout, marker+" ")
	if at < 0 {
		return nil, fmt.Errorf("curl omitted %s numeric marker", marker)
	}
	line := stdout[at:]
	if end := strings.IndexByte(line, '\n'); end >= 0 {
		line = line[:end]
	}
	values := make(map[string]float64, len(fields))
	for _, field := range strings.Fields(line)[1:] {
		key, raw, found := strings.Cut(field, "=")
		if !found {
			continue
		}
		v, err := strconv.ParseFloat(raw, 64)
		if err != nil || math.IsNaN(v) || math.IsInf(v, 0) || v < 0 {
			return nil, fmt.Errorf("invalid curl %s value %q", key, raw)
		}
		values[key] = v
	}
	for _, field := range fields {
		if _, ok := values[field]; !ok {
			return nil, fmt.Errorf("curl marker missing numeric %s", field)
		}
	}
	return values, nil
}

type downloadMetrics struct {
	bytes, bytesPerSecond, seconds float64
}

func (r *run) download(ctx context.Context, id, target, op string, guests, expectedBytes int) (downloadMetrics, error) {
	started := time.Now()
	curlTimeout := strconv.FormatFloat(r.cfg.netTimeout.Seconds(), 'f', 9, 64)
	callCtx, cancel := context.WithTimeout(ctx, r.cfg.netTimeout+10*time.Second)
	defer cancel()
	output, execErr := r.api.exec(callCtx, id,
		"curl", "-f", "-L", "-sS", "--max-time", curlTimeout, "--max-filesize", strconv.Itoa(maxCurlBytes),
		"-o", "/dev/null", "-w", "BHT_DL size_download=%{size_download} speed_download=%{speed_download} time_total=%{time_total}\n", target)
	parsed, parseErr := parseCurlMetrics(output.Stdout, "BHT_DL", "size_download", "speed_download", "time_total")
	var metrics downloadMetrics
	var values map[string]float64
	if parseErr == nil {
		metrics = downloadMetrics{bytes: parsed["size_download"], bytesPerSecond: parsed["speed_download"], seconds: parsed["time_total"]}
		values = map[string]float64{
			"guests": float64(guests), "bytes": metrics.bytes, "seconds": metrics.seconds,
			"bytes_per_second": metrics.bytesPerSecond, "mb_per_second": metrics.bytesPerSecond / 1e6,
		}
	}
	if parseErr == nil && (metrics.bytes <= 0 || metrics.seconds <= 0) {
		parseErr = fmt.Errorf("download returned no positive bytes or elapsed time (%.0f bytes, %.6f s)", metrics.bytes, metrics.seconds)
	}
	if expectedBytes > 0 && parseErr == nil && metrics.bytes != float64(expectedBytes) {
		parseErr = fmt.Errorf("downloaded %.0f bytes, expected %d", metrics.bytes, expectedBytes)
	}
	err := errors.Join(execErr, parseErr)
	r.record(op, started, err, values)
	return metrics, err
}

// Each guest emits its own wall-clock start and curl TCP-connect time. The
// HTTP/TLS transfer can fail after a successful TCP handshake without making
// that connect-time sample invalid.
const connectLoopScript = `i=0
while [ "$i" -lt "$1" ]; do
  started=$(date +%s%N) || exit 1
  printf 'BHT_CONNECT start_ns=%s ' "$started"
  curl -sS -o /dev/null --connect-timeout 2 --max-time 3 -w 'time_connect=%{time_connect} ' https://1.1.1.1:443/ 2>/dev/null
  curl_exit=$?
  printf 'curl_exit=%s\n' "$curl_exit"
  i=$((i + 1))
done`

type connectProbe struct {
	start    time.Time
	ms       float64
	curlExit int
}

func parseConnectProbe(line string) (connectProbe, error) {
	fields := strings.Fields(line)
	if len(fields) != 4 || fields[0] != "BHT_CONNECT" {
		return connectProbe{}, fmt.Errorf("invalid guest TCP sample %q", line)
	}
	startRaw, hasStart := strings.CutPrefix(fields[1], "start_ns=")
	connectRaw, hasConnect := strings.CutPrefix(fields[2], "time_connect=")
	exitRaw, hasExit := strings.CutPrefix(fields[3], "curl_exit=")
	if !hasStart || !hasConnect || !hasExit {
		return connectProbe{}, fmt.Errorf("incomplete guest TCP sample %q", line)
	}
	startNS, startErr := strconv.ParseInt(startRaw, 10, 64)
	seconds, connectErr := strconv.ParseFloat(connectRaw, 64)
	curlExit, exitErr := strconv.Atoi(exitRaw)
	if startErr != nil || connectErr != nil || exitErr != nil || startNS <= 0 ||
		seconds < 0 || math.IsNaN(seconds) || math.IsInf(seconds, 0) || curlExit < 0 || curlExit > 255 {
		return connectProbe{}, fmt.Errorf("invalid guest TCP numeric sample %q", line)
	}
	return connectProbe{start: time.Unix(0, startNS), ms: seconds * 1e3, curlExit: curlExit}, nil
}

func (r *run) connectRTTSeries(id string) (latencies []float64, failed, missed, postConnectErrors int, err error) {
	latencies = make([]float64, 0, r.cfg.connectSamples)
	// Bound each API exec, including worst-case failed curl attempts, below the
	// API client's own timeout. Normal 30 samples run in one guest shell loop.
	maxBatch := max(1, int((r.cfg.timeout-5*time.Second)/(3*time.Second)))
	remaining := r.cfg.connectSamples
	var firstErr error
	for remaining > 0 && r.ctx.Err() == nil {
		batch := min(remaining, maxBatch)
		callCtx, cancel := context.WithTimeout(r.ctx, min(r.cfg.timeout, time.Duration(batch)*3*time.Second+5*time.Second))
		output, execErr := r.api.exec(callCtx, id, "/bin/sh", "-c", connectLoopScript, "bench-connect", strconv.Itoa(batch))
		cancel()
		seen := 0
		for _, line := range strings.Split(strings.TrimSpace(output.Stdout), "\n") {
			if line == "" {
				continue
			}
			probe, parseErr := parseConnectProbe(line)
			if parseErr != nil {
				err = errors.Join(err, parseErr)
				continue
			}
			if seen == batch {
				err = errors.Join(err, fmt.Errorf("guest emitted more than %d TCP samples in a batch", batch))
				break
			}
			seen++
			values := map[string]float64{
				"connect_ms": probe.ms, "curl_exit_code": float64(probe.curlExit),
			}
			var sampleErr error
			if probe.ms == 0 {
				if probe.curlExit == 28 {
					sampleErr = fmt.Errorf("guest TCP connect timeout: %w", context.DeadlineExceeded)
				} else {
					sampleErr = fmt.Errorf("guest TCP connect failed (curl exit %d)", probe.curlExit)
				}
				failed++
				if firstErr == nil {
					firstErr = sampleErr
				}
			} else {
				latencies = append(latencies, probe.ms)
				if probe.curlExit != 0 {
					postConnectErrors++
				}
			}
			r.recordAt("net.connect", probe.start, probe.ms, sampleErr, values)
		}
		remaining -= batch
		if seen != batch {
			missed += batch - seen
			err = errors.Join(err, fmt.Errorf("guest reported %d/%d TCP samples in a batch", seen, batch))
		}
		if execErr != nil {
			err = errors.Join(err, fmt.Errorf("guest TCP sample loop: %w", execErr))
		}
		if err != nil {
			break
		}
	}
	missed += remaining
	if failed > 0 {
		err = errors.Join(err, fmt.Errorf("%d guest TCP connections failed (first: %w)", failed, firstErr))
	}
	return latencies, failed, missed, postConnectErrors, errors.Join(err, r.ctx.Err())
}

// Pace setup creates independently of the download windows. Otherwise a
// 1,5,10 guest sweep exceeds the owner's 30/min create limit before level 10.
func (r *run) pacedNetCreate(pacer *time.Ticker, tier, policy string) (sandbox, error) {
	if err := waitPace(r.ctx, pacer); err != nil {
		return sandbox{}, err
	}
	return r.scenarioCreate("net.create", tier, policy, "")
}

func (r *run) netSingle(target, policy string, pacer *time.Ticker) (err error) {
	sb, err := r.pacedNetCreate(pacer, "minimal", policy)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, r.scenarioDestroy("net.destroy", sb.ID)) }()
	metrics, downloadErr := r.download(r.ctx, sb.ID, target, "net.single.download", 1, 0)
	r.extra["net_single"] = map[string]any{
		"bytes": metrics.bytes, "mb_per_second": metrics.bytesPerSecond / 1e6,
		"seconds": metrics.seconds, "ok": downloadErr == nil,
	}
	err = errors.Join(err, downloadErr)
	latencies, failed, missed, postConnectErrors, connectErr := r.connectRTTSeries(sb.ID)
	err = errors.Join(err, connectErr)
	sort.Float64s(latencies)
	connectGroup := map[string]any{
		"target": "1.1.1.1:443", "requested_samples": r.cfg.connectSamples,
		"samples": len(latencies) + failed, "ok": len(latencies), "errors": failed + missed,
		"missing_samples": missed, "post_connect_curl_errors": postConnectErrors,
	}
	if len(latencies) > 0 {
		connectGroup["p50_ms"] = wakePercentile(latencies, 0.50)
		connectGroup["p95_ms"] = wakePercentile(latencies, 0.95)
		connectGroup["p99_ms"] = wakePercentile(latencies, 0.99)
	}
	r.extra["net_connect"] = connectGroup
	return err
}

func (r *run) netLevel(target, policy string, level int, pacer *time.Ticker) (err error) {
	ids := make([]string, 0, level)
	defer func() {
		for _, id := range ids {
			err = errors.Join(err, r.scenarioDestroy("net.destroy", id))
		}
	}()
	for range level {
		if r.ctx.Err() != nil {
			return errors.Join(err, r.ctx.Err())
		}
		sb, createErr := r.pacedNetCreate(pacer, "minimal", policy)
		if createErr != nil {
			return errors.Join(err, createErr)
		}
		ids = append(ids, sb.ID)
	}
	type result struct {
		metrics downloadMetrics
		err     error
	}
	results := make([]result, level)
	gate := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(level)
	for i := range ids {
		go func(i int) {
			defer wg.Done()
			<-gate
			results[i].metrics, results[i].err = r.download(r.ctx, ids[i], target, "net.level.download", level, 0)
		}(i)
	}
	start := time.Now()
	close(gate)
	wg.Wait()
	elapsed := time.Since(start)
	var bytes, speeds float64
	guestRates := make([]map[string]any, level)
	failures := 0
	for i, result := range results {
		guestRates[i] = map[string]any{
			"guest_index": i, "ok": result.err == nil,
			"bytes": result.metrics.bytes, "mb_per_second": result.metrics.bytesPerSecond / 1e6,
		}
		if result.err == nil {
			bytes += result.metrics.bytes
			speeds += result.metrics.bytesPerSecond
		} else {
			failures++
			if err == nil {
				err = result.err
			}
		}
	}
	r.extra["net_levels"] = appendProxyGroup(r.extra["net_levels"], map[string]any{
		"guests": level, "ok": level - failures, "errors": failures, "guest_rates": guestRates,
		"bytes": bytes, "elapsed_seconds": elapsed.Seconds(),
		"aggregate_mb_per_second": speeds / 1e6,
		"window_mb_per_second":    bytes / elapsed.Seconds() / 1e6,
	})
	if failures != 0 {
		err = fmt.Errorf("net guests=%d: %d downloads failed (first: %w)", level, failures, err)
	}
	return err
}

func (r *run) netSibling(pacer *time.Ticker) (err error) {
	ids := make([]string, 0, 2)
	defer func() {
		for _, id := range ids {
			err = errors.Join(err, r.scenarioDestroy("net.destroy", id))
		}
	}()
	for range 2 {
		sb, createErr := r.pacedNetCreate(pacer, "browser", "allow-siblings")
		if createErr != nil {
			return createErr
		}
		ids = append(ids, sb.ID)
	}
	if err = r.startBenchServer(ids[0], true); err != nil {
		return err
	}
	var peer sandbox
	if err = r.measure(r.ctx, "net.inspect", func(ctx context.Context) error {
		var inspectErr error
		peer, inspectErr = r.api.inspect(ctx, ids[0])
		return inspectErr
	}); err != nil {
		return err
	}
	if net.ParseIP(peer.IP) == nil || peer.IP == "0.0.0.0" || peer.IP == "::" {
		return fmt.Errorf("net sibling source %s has no routable guest IP: %q", ids[0], peer.IP)
	}
	peerURL := "http://" + net.JoinHostPort(peer.IP, strconv.Itoa(benchServerPort)) + "/payload"
	metrics, downloadErr := r.download(r.ctx, ids[1], peerURL, "net.sibling.download", 1, peerBytes)
	r.extra["net_sibling"] = map[string]any{
		"source_bytes": peerBytes, "downloaded_bytes": metrics.bytes,
		"mb_per_second": metrics.bytesPerSecond / 1e6, "seconds": metrics.seconds,
		"network": "allow-siblings", "ok": downloadErr == nil,
	}
	if downloadErr != nil {
		return fmt.Errorf("sibling guest download: %w", downloadErr)
	}
	return nil
}

func (r *run) net() error {
	levels, err := parsePositiveLevels(r.cfg.levels)
	if err != nil {
		return err
	}
	u, err := url.Parse(r.cfg.url)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" || u.User != nil {
		return fmt.Errorf("-url must be an http(s) download URL without credentials: %q", r.cfg.url)
	}
	policy := r.cfg.network
	if policy == "default" || policy == "" {
		policy = "public"
	}
	pacer := time.NewTicker(2100 * time.Millisecond)
	defer pacer.Stop()
	singleErr := r.netSingle(r.cfg.url, policy, pacer)
	if r.ctx.Err() != nil {
		return errors.Join(singleErr, r.ctx.Err())
	}
	for _, level := range levels {
		singleErr = errors.Join(singleErr, r.netLevel(r.cfg.url, policy, level, pacer))
		if r.ctx.Err() != nil {
			return errors.Join(singleErr, r.ctx.Err())
		}
	}
	return errors.Join(singleErr, r.netSibling(pacer))
}
