package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptrace"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/sahil-shubham/bhatti/pkg"
	"github.com/spf13/cobra"
	"golang.org/x/term"
)

var (
	apiURL   = "http://localhost:8080"
	apiToken = ""
	// unixSocketPath, when set, routes the CLI's HTTP + websocket traffic over the
	// daemon's local control socket instead of TCP (apiURL becomes http://unix).
	unixSocketPath = ""
)

// rootCmd is the top-level cobra command. All subcommands attach here.
// The serve command is added in main() since it's defined alongside
// the daemon code in main.go.
var rootCmd = &cobra.Command{
	Use:   "bhatti",
	Short: "Firecracker microVM orchestrator",
	Long: `bhatti creates isolated Linux VMs in seconds. Each sandbox has its own
kernel, filesystem, and network. Paused sandboxes resume in under 3ms.

Quick start:
  bhatti setup                         # configure endpoint + API key
  bhatti create --name dev             # create a sandbox
  bhatti exec dev -- echo hello        # run a command
  bhatti shell dev                     # interactive shell (Ctrl+\ to detach)
  bhatti destroy dev                   # clean up`,
	SilenceUsage: true,
	PersistentPreRun: func(cmd *cobra.Command, args []string) {
		loadConfig(cmd)
	},
}

func init() {
	rootCmd.PersistentFlags().String("url", "", "API endpoint (overrides config)")
	rootCmd.PersistentFlags().String("token", "", "API key (overrides config)")
	rootCmd.PersistentFlags().String("data-dir", "", "Data directory containing state.db (for user commands)")
	rootCmd.PersistentFlags().Bool("json", false, "Output as JSON")
	rootCmd.PersistentFlags().Bool("timing", false, "Show request timing breakdown")

	// Command groups
	rootCmd.AddGroup(
		&cobra.Group{ID: "core", Title: "Core:"},
		&cobra.Group{ID: "resource", Title: "Resources:"},
		&cobra.Group{ID: "admin", Title: "Setup & Admin:"},
	)

	createCmd.GroupID = "core"
	editCmd.GroupID = "core"
	listCmd.GroupID = "core"
	destroyCmd.GroupID = "core"
	stopCmd.GroupID = "core"
	startCmd.GroupID = "core"
	execCmd.GroupID = "core"
	shellCmd.GroupID = "core"
	shareCmd.GroupID = "core"

	imageCmd.GroupID = "resource"
	volumeCmd.GroupID = "resource"
	secretCmd.GroupID = "resource"
	snapshotCmd.GroupID = "resource"
	publishCmd.GroupID = "resource"
	unpublishCmd.GroupID = "resource"

	setupCmd.GroupID = "admin"
	userCmd.GroupID = "admin"
	updateCmd.GroupID = "admin"
	adminCmd.GroupID = "admin"

	// inspect, ps, file, version, completion have no GroupID →
	// fall into "Additional Commands"

	rootCmd.AddCommand(createCmd)
	rootCmd.AddCommand(editCmd)
	listCmd.Flags().StringP("output", "o", "", "Output format (wide)")
	rootCmd.AddCommand(listCmd)
	rootCmd.AddCommand(destroyCmd)
	rootCmd.AddCommand(stopCmd)
	rootCmd.AddCommand(startCmd)
	rootCmd.AddCommand(inspectCmd)
	rootCmd.AddCommand(execCmd)
	rootCmd.AddCommand(shellCmd)
	shellCmd.Flags().Bool("new", false, "Force a new session (don't reattach)")
	rootCmd.AddCommand(psCmd)
	rootCmd.AddCommand(portsCmd)
	rootCmd.AddCommand(forwardCmd)
	rootCmd.AddCommand(fileCmd)
	rootCmd.AddCommand(secretCmd)
	rootCmd.AddCommand(volumeCmd)
	rootCmd.AddCommand(imageCmd)
	rootCmd.AddCommand(snapshotCmd)
	rootCmd.AddCommand(userCmd)
	rootCmd.AddCommand(adminCmd)
	rootCmd.AddCommand(setupCmd)
	updateCmd.Flags().Bool("cli-only", false, "Update only the CLI binary, even on a server")
	updateCmd.Flags().String("tiers", "", "Install additional rootfs tiers (comma-separated or \"all\")")
	rootCmd.AddCommand(updateCmd)
	rootCmd.AddCommand(versionCmd)
	rootCmd.AddCommand(completionCmd)

	publishCmd.Flags().IntP("port", "p", 0, "Port to publish (required)")
	publishCmd.MarkFlagRequired("port")
	publishCmd.Flags().StringP("alias", "a", "", "Custom alias (auto-generated if omitted)")
	publishCmd.Flags().Bool("shell", false, "Also generate a web shell URL")
	unpublishCmd.Flags().IntP("port", "p", 0, "Port to unpublish (required)")
	unpublishCmd.MarkFlagRequired("port")
	rootCmd.AddCommand(publishCmd)
	rootCmd.AddCommand(unpublishCmd)

	shareCmd.Flags().Bool("revoke", false, "Revoke shell access")
	rootCmd.AddCommand(shareCmd)
}

// runCLI is called from main() for any subcommand other than "serve".
func runCLI() {
	if err := rootCmd.Execute(); err != nil {
		os.Exit(1)
	}
}

// loadConfig sets apiURL and apiToken with precedence:
//
//	flag → config file → env var → default
//
// This means `bhatti setup` writes the config and it just works.
// Env vars are the fallback for CI/scripts, not the override.
func loadConfig(cmd *cobra.Command) {
	cfg, _ := pkg.LoadConfig()

	// URL: flag wins, then config, then env, then default
	if v, _ := cmd.Flags().GetString("url"); v != "" {
		apiURL = v
	} else if cfg != nil && cfg.APIURL != "" {
		apiURL = cfg.APIURL
	} else if v := os.Getenv("BHATTI_URL"); v != "" {
		apiURL = v
	} else if cfg != nil {
		// No explicit remote endpoint: prefer the daemon's local unix control
		// socket (not reachable from a sandbox). Fall back to the default TCP URL if
		// the socket isn't there (no daemon / older daemon).
		if sock := cfg.APISocketPath(); sock != "" {
			if _, err := os.Stat(sock); err == nil {
				unixSocketPath = sock
				apiURL = "http://unix"
			}
		}
	}

	// Token: same order
	if v, _ := cmd.Flags().GetString("token"); v != "" {
		apiToken = v
	} else if cfg != nil && cfg.AuthToken != "" {
		apiToken = cfg.AuthToken
	} else if v := os.Getenv("BHATTI_TOKEN"); v != "" {
		apiToken = v
	}
}

// --- HTTP helpers ---

func apiRequest(method, path string, body any) (*http.Response, error) {
	var r io.Reader
	if body != nil {
		data, _ := json.Marshal(body)
		r = bytes.NewReader(data)
	}
	req, err := http.NewRequest(method, apiURL+path, r)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if apiToken != "" {
		req.Header.Set("Authorization", "Bearer "+apiToken)
	}
	return httpClient().Do(req)
}

func apiJSON(method, path string, body any, result any) error {
	resp, err := apiRequest(method, path, body)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	// Check server version headers — push-based update notification.
	checkServerVersion(resp)

	if resp.StatusCode >= 400 {
		var errBody struct {
			Error string `json:"error"`
		}
		json.NewDecoder(resp.Body).Decode(&errBody)
		return &apiError{status: resp.Status, message: errBody.Error}
	}
	if result != nil {
		return json.NewDecoder(resp.Body).Decode(result)
	}
	return nil
}

// apiError wraps server errors with actionable recovery hints (B3).
type apiError struct {
	status  string
	message string
}

func (e *apiError) Error() string {
	base := fmt.Sprintf("%s: %s", e.status, e.message)
	hint := errorHint(e.message)
	if hint != "" {
		return base + "\n\n" + hint
	}
	return base
}

// errorHint returns a recovery suggestion for known error patterns.
func errorHint(msg string) string {
	lower := strings.ToLower(msg)
	switch {
	case strings.Contains(lower, "not running"):
		return "  Resume it first:\n    bhatti start <sandbox>"
	case strings.Contains(lower, "not found"):
		return "  Check sandbox name:\n    bhatti ls"
	case strings.Contains(lower, "already exists"):
		return "  Use a different name or destroy the existing one:\n    bhatti destroy <sandbox>"
	case strings.Contains(lower, "use 'bhatti start --force'"):
		return "  Retry with force:\n    bhatti start --force <sandbox>"
	case strings.Contains(lower, "limit") || strings.Contains(lower, "max sandbox"):
		return "  Destroy unused sandboxes to free capacity:\n    bhatti ls\n    bhatti destroy <sandbox>"
	}
	return ""
}

// versionChecked prevents duplicate update messages within a single
// CLI invocation (resolveID + actual command = 2 API calls).
var versionChecked bool

// checkServerVersion reads the X-Bhatti-Version and X-Bhatti-Min-CLI
// headers from the server response. If the CLI is outdated, it prints
// a one-time warning to stderr. This is the push mechanism — the server
// tells the CLI it's outdated through headers already present on every
// response, with zero extra latency.
func checkServerVersion(resp *http.Response) {
	if versionChecked || version == "dev" {
		return
	}
	versionChecked = true

	minCLI := resp.Header.Get("X-Bhatti-Min-CLI")

	// Hard warning: CLI is below the server's minimum required version.
	// This is the ONLY case where we show an update notice — when the
	// server explicitly requires a newer CLI via X-Bhatti-Min-CLI.
	if minCLI != "" && compareVersions(version, minCLI) < 0 {
		fmt.Fprintf(os.Stderr, "⚠ CLI version %s is below server minimum %s — please update:\n", version, minCLI)
		fmt.Fprintf(os.Stderr, "  bhatti update\n\n")
		return
	}

	// No soft notice — don't nag users about optional updates on every command.
}

// compareVersions compares two semver strings (with optional 'v' prefix).
// Returns -1 if a < b, 0 if equal, 1 if a > b.
// Only handles numeric major.minor.patch — no pre-release suffixes.
func compareVersions(a, b string) int {
	a = strings.TrimPrefix(a, "v")
	b = strings.TrimPrefix(b, "v")
	ap := strings.SplitN(a, ".", 3)
	bp := strings.SplitN(b, ".", 3)
	for i := 0; i < 3; i++ {
		var ai, bi int
		if i < len(ap) {
			fmt.Sscanf(ap[i], "%d", &ai)
		}
		if i < len(bp) {
			fmt.Sscanf(bp[i], "%d", &bi)
		}
		if ai < bi {
			return -1
		}
		if ai > bi {
			return 1
		}
	}
	return 0
}

// --- Confirmation helper ---

// confirmAction prompts for confirmation on destructive operations.
// Returns true if --yes is set or the user confirms interactively.
func confirmAction(cmd *cobra.Command, msg string) bool {
	yes, _ := cmd.Flags().GetBool("yes")
	if yes {
		return true
	}
	if !term.IsTerminal(int(os.Stdin.Fd())) {
		fmt.Fprintf(os.Stderr, "Use --yes to confirm in non-interactive mode\n")
		return false
	}
	fmt.Fprintf(os.Stderr, "%s [y/N]: ", msg)
	var answer string
	fmt.Scanln(&answer)
	return strings.ToLower(answer) == "y"
}

// --- Output helpers ---

func isJSON(cmd *cobra.Command) bool {
	v, _ := cmd.Flags().GetBool("json")
	return v
}

func outputJSON(v any) {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	enc.Encode(v)
}

// --- Timing ---

// requestTiming records timestamps from an HTTP request lifecycle.
// Used by --timing to show where time was spent (dns, connect, tls,
// server processing, transfer).
type requestTiming struct {
	mu           sync.Mutex
	start        time.Time
	dnsStart     time.Time
	dnsDone      time.Time
	connectStart time.Time
	connectDone  time.Time
	tlsStart     time.Time
	tlsDone      time.Time
	firstByte    time.Time
	end          time.Time
}

func (t *requestTiming) trace() *httptrace.ClientTrace {
	return &httptrace.ClientTrace{
		DNSStart:             func(_ httptrace.DNSStartInfo) { t.mu.Lock(); t.dnsStart = time.Now(); t.mu.Unlock() },
		DNSDone:              func(_ httptrace.DNSDoneInfo) { t.mu.Lock(); t.dnsDone = time.Now(); t.mu.Unlock() },
		ConnectStart:         func(_, _ string) { t.mu.Lock(); t.connectStart = time.Now(); t.mu.Unlock() },
		ConnectDone:          func(_, _ string, _ error) { t.mu.Lock(); t.connectDone = time.Now(); t.mu.Unlock() },
		TLSHandshakeStart:    func() { t.mu.Lock(); t.tlsStart = time.Now(); t.mu.Unlock() },
		TLSHandshakeDone:     func(_ tls.ConnectionState, _ error) { t.mu.Lock(); t.tlsDone = time.Now(); t.mu.Unlock() },
		GotFirstResponseByte: func() { t.mu.Lock(); t.firstByte = time.Now(); t.mu.Unlock() },
	}
}

func (t *requestTiming) finish() {
	t.mu.Lock()
	t.end = time.Now()
	t.mu.Unlock()
}

// fmtDuration formats a duration with appropriate precision:
//
//	< 1ms   → microseconds  (342µs)
//	< 1s    → milliseconds  (12.3ms)
//	≥ 1s    → seconds       (2.38s)
func fmtDuration(d time.Duration) string {
	switch {
	case d < time.Millisecond:
		return fmt.Sprintf("%dµs", d.Microseconds())
	case d < time.Second:
		ms := float64(d.Microseconds()) / 1000.0
		if ms < 10 {
			return fmt.Sprintf("%.2fms", ms)
		}
		return fmt.Sprintf("%.1fms", ms)
	default:
		return fmt.Sprintf("%.2fs", d.Seconds())
	}
}

func (t *requestTiming) print() {
	t.mu.Lock()
	defer t.mu.Unlock()

	fmt.Fprintf(os.Stderr, "---\n")
	if !t.dnsStart.IsZero() && !t.dnsDone.IsZero() {
		fmt.Fprintf(os.Stderr, "dns:       %s\n", fmtDuration(t.dnsDone.Sub(t.dnsStart)))
	}
	if !t.connectStart.IsZero() && !t.connectDone.IsZero() {
		fmt.Fprintf(os.Stderr, "connect:   %s\n", fmtDuration(t.connectDone.Sub(t.connectStart)))
	}
	if !t.tlsStart.IsZero() && !t.tlsDone.IsZero() {
		fmt.Fprintf(os.Stderr, "tls:       %s\n", fmtDuration(t.tlsDone.Sub(t.tlsStart)))
	}
	serverStart := t.tlsDone
	if serverStart.IsZero() {
		serverStart = t.connectDone
	}
	if !serverStart.IsZero() && !t.firstByte.IsZero() {
		fmt.Fprintf(os.Stderr, "server:    %s\n", fmtDuration(t.firstByte.Sub(serverStart)))
	}
	if !t.firstByte.IsZero() && !t.end.IsZero() {
		fmt.Fprintf(os.Stderr, "transfer:  %s\n", fmtDuration(t.end.Sub(t.firstByte)))
	}
	if !t.start.IsZero() && !t.end.IsZero() {
		fmt.Fprintf(os.Stderr, "total:     %s\n", fmtDuration(t.end.Sub(t.start)))
	}
}

type timingTransport struct {
	inner  http.RoundTripper
	timing *requestTiming
}

func (t *timingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	// Reset timestamps so only the last request's timings are reported.
	// For commands that call resolveID first, we want the timing
	// of the actual operation (exec/create), not the name-lookup preamble.
	t.timing.mu.Lock()
	t.timing.dnsStart = time.Time{}
	t.timing.dnsDone = time.Time{}
	t.timing.connectStart = time.Time{}
	t.timing.connectDone = time.Time{}
	t.timing.tlsStart = time.Time{}
	t.timing.tlsDone = time.Time{}
	t.timing.firstByte = time.Time{}
	t.timing.end = time.Time{}
	t.timing.start = time.Now()
	t.timing.mu.Unlock()

	ctx := httptrace.WithClientTrace(req.Context(), t.timing.trace())
	req = req.WithContext(ctx)
	resp, err := t.inner.RoundTrip(req)
	t.timing.finish()
	return resp, err
}

// currentTiming is set per-command when --timing is active.
var currentTiming *requestTiming

// baseTransport dials the unix control socket when configured, else default TCP.
func baseTransport() http.RoundTripper {
	if unixSocketPath != "" {
		return &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, "unix", unixSocketPath)
			},
		}
	}
	return http.DefaultTransport
}

func httpClient() *http.Client {
	tr := baseTransport()
	if currentTiming != nil {
		return &http.Client{Transport: &timingTransport{inner: tr, timing: currentTiming}}
	}
	return &http.Client{Transport: tr}
}

// wsDialer returns a websocket dialer that honors the unix control socket.
func wsDialer() *websocket.Dialer {
	if unixSocketPath != "" {
		return &websocket.Dialer{
			NetDialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, "unix", unixSocketPath)
			},
		}
	}
	return websocket.DefaultDialer
}

func setupTiming(cmd *cobra.Command) {
	if v, _ := cmd.Flags().GetBool("timing"); v {
		currentTiming = &requestTiming{}
	} else {
		currentTiming = nil
	}
}

func printTiming() {
	if currentTiming != nil {
		currentTiming.print()
		currentTiming = nil
	}
}

// --- Name-to-ID resolution ---

func resolveID(nameOrID string) (string, error) {
	// Try direct ID lookup first
	resp, err := apiRequest("GET", "/sandboxes/"+nameOrID, nil)
	if err == nil && resp.StatusCode == 200 {
		resp.Body.Close()
		return nameOrID, nil
	}
	if resp != nil {
		resp.Body.Close()
	}

	// Fall back to name search
	var sandboxes []struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	}
	if err := apiJSON("GET", "/sandboxes", nil, &sandboxes); err != nil {
		return "", fmt.Errorf("cannot list sandboxes: %w", err)
	}
	for _, sb := range sandboxes {
		if sb.Name == nameOrID {
			return sb.ID, nil
		}
	}
	return "", fmt.Errorf("sandbox %q not found", nameOrID)
}

func parseEnvFlag(s string) map[string]string {
	if s == "" {
		return nil
	}
	m := map[string]string{}
	for _, kv := range strings.Split(s, ",") {
		parts := strings.SplitN(kv, "=", 2)
		if len(parts) == 2 {
			m[parts[0]] = parts[1]
		}
	}
	return m
}

// --- Completions ---

// addToCompletionCache appends a sandbox name to the local cache file.
// Best-effort — errors are silently ignored.
func addToCompletionCache(name string) {
	if name == "" {
		return
	}
	path := completionCachePath()
	data, _ := os.ReadFile(path)
	existing := strings.TrimSpace(string(data))
	if existing == "" {
		os.WriteFile(path, []byte(name), 0600)
		return
	}
	for _, n := range strings.Split(existing, "\n") {
		if n == name {
			return // already present
		}
	}
	os.WriteFile(path, []byte(existing+"\n"+name), 0600)
}

// removeFromCompletionCache removes a sandbox name from the local cache file.
// Best-effort — errors are silently ignored.
func removeFromCompletionCache(name string) {
	if name == "" {
		return
	}
	path := completionCachePath()
	data, err := os.ReadFile(path)
	if err != nil {
		return
	}
	var kept []string
	for _, n := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if n != name && n != "" {
			kept = append(kept, n)
		}
	}
	os.WriteFile(path, []byte(strings.Join(kept, "\n")), 0600)
}

// completeSandboxNames reads sandbox names from a local cache file.
// The cache is updated by create, destroy, and list commands.
// Never hits the network — instant, works offline.
func completeSandboxNames(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
	if len(args) > 0 {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	path := completionCachePath()
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	raw := strings.TrimSpace(string(data))
	if raw == "" {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	return strings.Split(raw, "\n"), cobra.ShellCompDirectiveNoFileComp
}

// completionCachePath returns the per-user completion cache path,
// stable across sudo and non-sudo invocations of the same user.
//
// Using os.Getuid() directly would give us a different cache file
// under sudo (uid 0) than as the user (uid 501), causing tab-complete
// to silently miss sandbox names created by the "other" UID. Anchoring
// to the *invoking* user keeps both views in sync.
func completionCachePath() string {
	return filepath.Join(os.TempDir(), fmt.Sprintf("bhatti-completions-%d", pkg.InvokingUID()))
}

// --- Arg validators ---

// exactArgs works like cobra.ExactArgs but prints help when the wrong
// number of args is given. With SilenceUsage on the root command, bare
// cobra.ExactArgs only shows "accepts N arg(s), received 0" — this
// ensures the user always sees the full help text.
func exactArgs(n int) cobra.PositionalArgs {
	return func(cmd *cobra.Command, args []string) error {
		if len(args) != n {
			cmd.Help()
			fmt.Println()
			return fmt.Errorf("accepts %d arg(s), received %d", n, len(args))
		}
		return nil
	}
}

// minimumArgs works like cobra.MinimumNArgs but prints help when too
// few args are given.
func minimumArgs(n int) cobra.PositionalArgs {
	return func(cmd *cobra.Command, args []string) error {
		if len(args) < n {
			cmd.Help()
			fmt.Println()
			return fmt.Errorf("requires at least %d arg(s), only received %d", n, len(args))
		}
		return nil
	}
}

// =====================================================================
// Commands
// =====================================================================

// --- create ---
