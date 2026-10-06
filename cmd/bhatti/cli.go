package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptrace"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/gorilla/websocket"
	"github.com/sahil-shubham/bhatti/pkg"
	"github.com/spf13/cobra"
	"golang.org/x/term"
	"gopkg.in/yaml.v3"
)

var (
	apiURL   = ""
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
	PersistentPreRunE: func(cmd *cobra.Command, args []string) error {
		return loadConfig(cmd)
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
//	flag → env var → config file → local socket
//
// Env vars override the config file (12-factor convention, matching
// docker/kubectl): an agent or CI job can point an already-configured CLI at
// another daemon with BHATTI_URL/BHATTI_TOKEN without editing
// ~/.bhatti/config.yaml. `bhatti setup` still just works when no env
// overrides are set.
func loadConfig(cmd *cobra.Command) error {
	cfg, err := pkg.LoadConfig()
	if err != nil {
		return err
	}

	// Reset per-command state so a prior local command cannot redirect an
	// explicitly remote URL through the socket.
	unixSocketPath = ""
	flagURL := ""
	if cmd != setupCmd {
		// setup's --url is the endpoint to save, not an override of the
		// currently configured socket used to validate that choice.
		flagURL, _ = cmd.Flags().GetString("url")
	}
	if flagURL != "" {
		apiURL = flagURL
	} else if v := os.Getenv("BHATTI_URL"); v != "" {
		apiURL = v
	} else if cfg.APIURL != "" {
		apiURL = cfg.APIURL
	} else {
		// Even when the socket is absent, dial it rather than silently
		// sending an API request (and its token) to localhost:8080.
		unixSocketPath, err = cliSocketPath(cfg, "/var/lib/bhatti/api.sock")
		if err != nil {
			return err
		}
		apiURL = "http://unix"
	}

	apiToken = ""
	if v, _ := cmd.Flags().GetString("token"); v != "" {
		apiToken = v
	} else if v := os.Getenv("BHATTI_TOKEN"); v != "" {
		apiToken = v
	} else {
		apiToken = cfg.AuthToken
	}
	return nil
}

// cliSocketPath distinguishes a configured data_dir from LoadConfig's default.
// The daemon's config defaults are unchanged; only endpoint selection differs.
func cliSocketPath(cfg *pkg.Config, systemSocket string) (string, error) {
	if cfg.APISocket != "" {
		return cfg.APISocket, nil
	}
	if cfg.ConfigPath != "" {
		data, err := os.ReadFile(cfg.ConfigPath)
		if err != nil {
			return "", fmt.Errorf("read config %s: %w", cfg.ConfigPath, err)
		}
		var explicit struct {
			APISocket string `yaml:"api_socket"`
			DataDir   string `yaml:"data_dir"`
		}
		if err := yaml.Unmarshal(data, &explicit); err != nil {
			return "", fmt.Errorf("parse config %s: %w", cfg.ConfigPath, err)
		}
		if explicit.APISocket != "" {
			return explicit.APISocket, nil
		}
		if explicit.DataDir != "" {
			return filepath.Join(explicit.DataDir, "api.sock"), nil
		}
	}
	if _, err := os.Stat(systemSocket); err == nil || !errors.Is(err, os.ErrNotExist) {
		return systemSocket, nil
	}
	return filepath.Join(pkg.DefaultDataDir(), "api.sock"), nil
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
// errAborted is returned when a confirmation prompt is declined — including
// non-interactive runs without --yes. It must be an error (non-zero exit):
// scripts and agents chain commands (`bhatti destroy x && ...`), and an
// aborted destructive operation that exits 0 reads as success.
var errAborted = fmt.Errorf("aborted (use --yes to skip confirmation)")

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

// socketDialError supplies a recovery hint without changing remote URL errors.
func socketDialError(sock string, err error) error {
	switch {
	case errors.Is(err, syscall.ENOENT), errors.Is(err, syscall.ECONNREFUSED):
		return fmt.Errorf("no bhatti daemon at %s: is it running? To use a remote server: bhatti setup", sock)
	case errors.Is(err, syscall.EACCES):
		return fmt.Errorf("permission denied on %s: add yourself to the bhatti group (then log in again) or use sudo", sock)
	default:
		return err
	}
}

func dialUnixSocket(ctx context.Context, sock string) (net.Conn, error) {
	conn, err := (&net.Dialer{}).DialContext(ctx, "unix", sock)
	if err != nil {
		return nil, socketDialError(sock, err)
	}
	return conn, nil
}

// baseTransport dials the unix control socket when configured, else default TCP.
func baseTransport() http.RoundTripper {
	if unixSocketPath != "" {
		sock := unixSocketPath
		return &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return dialUnixSocket(ctx, sock)
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
		sock := unixSocketPath
		return &websocket.Dialer{
			NetDialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return dialUnixSocket(ctx, sock)
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

// resolveID passes the sandbox name or ID through unchanged. The server
// resolves either on every /sandboxes/{id} route (store.GetSandbox matches ID
// first, then name), so a client-side resolution round trip here would double
// the latency of every CLI command — a full RTT wasted on remote links — for
// no benefit. Kept as a function so call sites read as intent, and as the
// place to reintroduce client-side resolution if a route ever needs a real ID.
// resolveID turns a user's sandbox reference into the API's: a name, an ID,
// or the "sandbox/<name>" form the CLI itself prints (so its output can be
// pasted back).
func resolveID(nameOrID string) (string, error) {
	nameOrID = strings.TrimPrefix(nameOrID, "sandbox/")
	if nameOrID == "" {
		return "", fmt.Errorf("sandbox name or ID required")
	}
	return nameOrID, nil
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
