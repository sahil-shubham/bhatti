package main

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptrace"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/gorilla/websocket"
	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/sahil-shubham/bhatti/pkg"
	"github.com/sahil-shubham/bhatti/pkg/store"
)

var (
	apiURL   = "http://localhost:8080"
	apiToken = ""
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

	imageCmd.GroupID = "resource"
	volumeCmd.GroupID = "resource"
	secretCmd.GroupID = "resource"
	snapshotCmd.GroupID = "resource"
	publishCmd.GroupID = "resource"
	unpublishCmd.GroupID = "resource"

	setupCmd.GroupID = "admin"
	userCmd.GroupID = "admin"
	updateCmd.GroupID = "admin"

	// inspect, ps, file, version, completion have no GroupID →
	// fall into "Additional Commands"

	rootCmd.AddCommand(createCmd)
	rootCmd.AddCommand(editCmd)
	rootCmd.AddCommand(listCmd)
	rootCmd.AddCommand(destroyCmd)
	rootCmd.AddCommand(stopCmd)
	rootCmd.AddCommand(startCmd)
	rootCmd.AddCommand(inspectCmd)
	rootCmd.AddCommand(execCmd)
	rootCmd.AddCommand(shellCmd)
	shellCmd.Flags().Bool("new", false, "Force a new session (don't reattach)")
	rootCmd.AddCommand(psCmd)
	rootCmd.AddCommand(fileCmd)
	rootCmd.AddCommand(secretCmd)
	rootCmd.AddCommand(volumeCmd)
	rootCmd.AddCommand(imageCmd)
	rootCmd.AddCommand(snapshotCmd)
	rootCmd.AddCommand(userCmd)
	rootCmd.AddCommand(setupCmd)
	rootCmd.AddCommand(updateCmd)
	rootCmd.AddCommand(versionCmd)
	rootCmd.AddCommand(completionCmd)

	publishCmd.Flags().IntP("port", "p", 0, "Port to publish (required)")
	publishCmd.MarkFlagRequired("port")
	publishCmd.Flags().StringP("alias", "a", "", "Custom alias (auto-generated if omitted)")
	unpublishCmd.Flags().IntP("port", "p", 0, "Port to unpublish (required)")
	unpublishCmd.MarkFlagRequired("port")
	rootCmd.AddCommand(publishCmd)
	rootCmd.AddCommand(unpublishCmd)
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
		return fmt.Errorf("%s: %s", resp.Status, errBody.Error)
	}
	if result != nil {
		return json.NewDecoder(resp.Body).Decode(result)
	}
	return nil
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
		DNSStart:              func(_ httptrace.DNSStartInfo) { t.mu.Lock(); t.dnsStart = time.Now(); t.mu.Unlock() },
		DNSDone:               func(_ httptrace.DNSDoneInfo) { t.mu.Lock(); t.dnsDone = time.Now(); t.mu.Unlock() },
		ConnectStart:          func(_, _ string) { t.mu.Lock(); t.connectStart = time.Now(); t.mu.Unlock() },
		ConnectDone:           func(_, _ string, _ error) { t.mu.Lock(); t.connectDone = time.Now(); t.mu.Unlock() },
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

func httpClient() *http.Client {
	if currentTiming != nil {
		return &http.Client{
			Transport: &timingTransport{
				inner:  http.DefaultTransport,
				timing: currentTiming,
			},
		}
	}
	return http.DefaultClient
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

func completionCachePath() string {
	return filepath.Join(os.TempDir(), fmt.Sprintf("bhatti-completions-%d", os.Getuid()))
}

// =====================================================================
// Commands
// =====================================================================

// --- create ---

var createCmd = &cobra.Command{
	Use:   "create [flags]",
	Short: "Create a new sandbox",
	Long: `Create a new sandbox VM. Each sandbox is an isolated Linux environment
with its own kernel, filesystem, and network.`,
	Example: `  # Basic sandbox
  bhatti create --name dev

  # Custom resources
  bhatti create --name ml --cpus 4 --memory 4096

  # With environment variables and init script
  bhatti create --name api --env API_KEY=sk-abc --init "npm install"

  # From a custom image
  bhatti create --name py --image python-3.12

  # With a persistent volume
  bhatti create --name work --volume workspace:/workspace

  # Autonomous agent (stays hot, never paused)
  bhatti create --name agent --init "hermes gateway" --keep-hot`,
	RunE: func(cmd *cobra.Command, args []string) error {
		setupTiming(cmd)
		defer printTiming()

		name, _ := cmd.Flags().GetString("name")
		image, _ := cmd.Flags().GetString("image")
		cpus, _ := cmd.Flags().GetFloat64("cpus")
		memory, _ := cmd.Flags().GetInt("memory")
		diskSize, _ := cmd.Flags().GetInt("disk-size")
		env, _ := cmd.Flags().GetString("env")
		initScript, _ := cmd.Flags().GetString("init")
		keepHot, _ := cmd.Flags().GetBool("keep-hot")
		volFlags, _ := cmd.Flags().GetStringSlice("volume")

		tmpl, _ := cmd.Flags().GetString("template")

		envMap := parseEnvFlag(env)
		req := map[string]any{
			"name": name, "cpus": cpus,
		}
		if tmpl != "" {
			req["template_id"] = tmpl
		}
		if memory > 0 {
			req["memory_mb"] = memory
		}
		if image != "" {
			req["image"] = image
		}
		if diskSize > 0 {
			req["disk_size_mb"] = diskSize
		}
		if len(envMap) > 0 {
			req["env"] = envMap
		}
		if initScript != "" {
			req["init"] = initScript
		}
		if keepHot {
			req["keep_hot"] = true
		}

		// Parse --volume flags: name:mount[:ro]
		if len(volFlags) > 0 {
			var pvols []map[string]any
			for _, vf := range volFlags {
				parts := strings.SplitN(vf, ":", 3)
				if len(parts) < 2 {
					return fmt.Errorf("invalid --volume format %q (expected name:mount[:ro])", vf)
				}
				pv := map[string]any{
					"name":        parts[0],
					"mount":       parts[1],
					"auto_create": false,
					"read_only":   false,
				}
				if len(parts) == 3 && parts[2] == "ro" {
					pv["read_only"] = true
				}
				pvols = append(pvols, pv)
			}
			req["persistent_volumes"] = pvols
		}

		var sb struct {
			ID   string `json:"id"`
			Name string `json:"name"`
			IP   string `json:"ip"`
		}
		if err := apiJSON("POST", "/sandboxes", req, &sb); err != nil {
			return err
		}
		addToCompletionCache(sb.Name)
		if isJSON(cmd) {
			outputJSON(sb)
		} else {
			fmt.Printf("%s\t%s\t%s\n", sb.ID, sb.Name, sb.IP)
		}
		return nil
	},
}

func init() {
	createCmd.Flags().String("name", "", "Sandbox name")
	createCmd.Flags().String("image", "", "Rootfs image name")
	createCmd.Flags().Float64("cpus", 1, "Number of vCPUs")
	createCmd.Flags().Int("memory", 0, "Memory in MB (0 = server default: 2048)")
	createCmd.Flags().Int("disk-size", 0, "Rootfs disk size in MB (0 = use image size)")
	createCmd.Flags().String("env", "", "Environment variables (K=V,K=V)")
	createCmd.Flags().String("init", "", "Init script")
	createCmd.Flags().Bool("keep-hot", false, "Prevent thermal transitions (for autonomous agents)")
	createCmd.Flags().String("template", "", "Template name or ID")
	createCmd.Flags().StringSlice("volume", nil, "Persistent volume (name:mount[:ro])")

	editCmd.Flags().Bool("keep-hot", false, "Prevent thermal transitions (for autonomous agents)")
	editCmd.Flags().Bool("allow-cold", false, "Re-enable thermal transitions")
}

// --- edit ---

var editCmd = &cobra.Command{
	Use:   "edit <sandbox> [flags]",
	Short: "Update sandbox settings",
	Long: `Update mutable settings on an existing sandbox. Currently supports
toggling keep_hot to control thermal management.`,
	Example: `  # Prevent a sandbox from being paused/snapshotted
  bhatti edit my-agent --keep-hot

  # Re-enable thermal transitions
  bhatti edit my-agent --allow-cold`,
	Args:              cobra.ExactArgs(1),
	ValidArgsFunction: completeSandboxNames,
	RunE: func(cmd *cobra.Command, args []string) error {
		id, err := resolveID(args[0])
		if err != nil {
			return err
		}

		req := map[string]any{}
		keepHot, _ := cmd.Flags().GetBool("keep-hot")
		allowCold, _ := cmd.Flags().GetBool("allow-cold")
		if keepHot && allowCold {
			return fmt.Errorf("cannot use --keep-hot and --allow-cold together")
		}
		if keepHot {
			req["keep_hot"] = true
		}
		if allowCold {
			req["keep_hot"] = false
		}

		if len(req) == 0 {
			return fmt.Errorf("nothing to update — use --keep-hot or --allow-cold")
		}

		var sb map[string]any
		if err := apiJSON("PATCH", "/sandboxes/"+id, req, &sb); err != nil {
			return err
		}
		if isJSON(cmd) {
			outputJSON(sb)
		} else {
			fmt.Printf("Updated %s\n", args[0])
			if keepHot {
				fmt.Println("  keep_hot: true (thermal transitions disabled)")
			}
			if allowCold {
				fmt.Println("  keep_hot: false (thermal transitions re-enabled)")
			}
		}
		return nil
	},
}

// --- stop ---

var stopCmd = &cobra.Command{
	Use:   "stop <sandbox>",
	Short: "Snapshot and stop a sandbox",
	Long: `Pause the sandbox and save a snapshot to disk. Resume later with
'bhatti start'. Stopped sandboxes use zero CPU and memory.`,
	Example: `  bhatti stop dev
  bhatti start dev     # resume later`,
	Args:              cobra.ExactArgs(1),
	ValidArgsFunction: completeSandboxNames,
	RunE: func(cmd *cobra.Command, args []string) error {
		setupTiming(cmd)
		defer printTiming()
		id, err := resolveID(args[0])
		if err != nil {
			return err
		}
		var sb map[string]any
		if err := apiJSON("POST", "/sandboxes/"+id+"/stop", nil, &sb); err != nil {
			return err
		}
		if isJSON(cmd) {
			outputJSON(sb)
		} else {
			fmt.Println("stopped")
		}
		return nil
	},
}

// --- start ---

var startCmd = &cobra.Command{
	Use:   "start <sandbox>",
	Short: "Resume a stopped sandbox",
	Long:  `Resume a sandbox from its snapshot. Continues exactly where it left off.`,
	Example: `  bhatti start dev`,
	Args:              cobra.ExactArgs(1),
	ValidArgsFunction: completeSandboxNames,
	RunE: func(cmd *cobra.Command, args []string) error {
		setupTiming(cmd)
		defer printTiming()
		id, err := resolveID(args[0])
		if err != nil {
			return err
		}
		var sb map[string]any
		if err := apiJSON("POST", "/sandboxes/"+id+"/start", nil, &sb); err != nil {
			return err
		}
		if isJSON(cmd) {
			outputJSON(sb)
		} else {
			fmt.Printf("started (%s)\n", sb["status"])
		}
		return nil
	},
}

// --- inspect ---

var inspectCmd = &cobra.Command{
	Use:     "inspect <sandbox>",
	Short:   "Show sandbox details",
	Aliases: []string{"info"},
	Example: `  bhatti inspect dev
  bhatti inspect dev --json`,
	Args:              cobra.ExactArgs(1),
	ValidArgsFunction: completeSandboxNames,
	RunE: func(cmd *cobra.Command, args []string) error {
		setupTiming(cmd)
		defer printTiming()
		id, err := resolveID(args[0])
		if err != nil {
			return err
		}
		var sb map[string]any
		if err := apiJSON("GET", "/sandboxes/"+id, nil, &sb); err != nil {
			return err
		}
		if isJSON(cmd) {
			outputJSON(sb)
			return nil
		}
		fmt.Printf("Name:       %s\n", sb["name"])
		fmt.Printf("ID:         %s\n", sb["id"])
		fmt.Printf("Status:     %s\n", sb["status"])
		fmt.Printf("IP:         %s\n", sb["ip"])
		fmt.Printf("Created:    %s\n", sb["created_at"])
		if t, ok := sb["template_id"]; ok && t != nil && t != "" {
			fmt.Printf("Template:   %s\n", t)
		}
		if stopped, ok := sb["stopped_at"]; ok && stopped != nil {
			fmt.Printf("Stopped:    %s\n", stopped)
		}
		return nil
	},
}

// --- list ---

var listCmd = &cobra.Command{
	Use:     "list",
	Aliases: []string{"ls"},
	Short:   "List sandboxes",
	Example: `  bhatti list
  bhatti ls            # alias
  bhatti ls --json`,
	RunE: func(cmd *cobra.Command, args []string) error {
		setupTiming(cmd)
		defer printTiming()

		var sandboxes []struct {
			ID      string   `json:"id"`
			Name    string   `json:"name"`
			Status  string   `json:"status"`
			Thermal string   `json:"thermal"`
			IP      string   `json:"ip"`
			URLs    []string `json:"urls"`
		}
		if err := apiJSON("GET", "/sandboxes", nil, &sandboxes); err != nil {
			return err
		}

		// Update completion cache (best-effort, never errors)
		var names []string
		for _, sb := range sandboxes {
			names = append(names, sb.Name)
		}
		os.WriteFile(completionCachePath(), []byte(strings.Join(names, "\n")), 0600)

		if isJSON(cmd) {
			outputJSON(sandboxes)
		} else {
			// Show URL column only if any sandbox has published URLs
			hasURLs := false
			for _, sb := range sandboxes {
				if len(sb.URLs) > 0 {
					hasURLs = true
					break
				}
			}

			if hasURLs {
				fmt.Printf("%-20s %-20s %-10s %-8s %-16s %s\n", "ID", "NAME", "STATUS", "THERMAL", "IP", "URL")
			} else {
				fmt.Printf("%-20s %-20s %-10s %-8s %-16s\n", "ID", "NAME", "STATUS", "THERMAL", "IP")
			}
			for _, sb := range sandboxes {
				thermal := sb.Thermal
				if thermal == "" {
					thermal = "-"
				}
				if hasURLs {
					url := ""
					if len(sb.URLs) > 0 {
						url = sb.URLs[0]
					}
					fmt.Printf("%-20s %-20s %-10s %-8s %-16s %s\n", sb.ID, sb.Name, sb.Status, thermal, sb.IP, url)
				} else {
					fmt.Printf("%-20s %-20s %-10s %-8s %-16s\n", sb.ID, sb.Name, sb.Status, thermal, sb.IP)
				}
			}
		}
		return nil
	},
}

// --- destroy ---

var destroyCmd = &cobra.Command{
	Use:               "destroy <id|name>",
	Aliases:           []string{"rm"},
	Short:             "Destroy a sandbox",
	Long: `Permanently destroy a sandbox and all its data. This cannot be undone.
Persistent volumes are detached but not deleted.`,
	Example: `  bhatti destroy dev
  bhatti rm dev        # alias`,
	Args:              cobra.ExactArgs(1),
	ValidArgsFunction: completeSandboxNames,
	RunE: func(cmd *cobra.Command, args []string) error {
		setupTiming(cmd)
		defer printTiming()

		if !confirmAction(cmd, fmt.Sprintf("Destroy sandbox %q?", args[0])) {
			return nil
		}

		id, err := resolveID(args[0])
		if err != nil {
			return err
		}
		if err := apiJSON("DELETE", "/sandboxes/"+id, nil, nil); err != nil {
			return err
		}
		removeFromCompletionCache(args[0])
		if isJSON(cmd) {
			outputJSON(map[string]string{"status": "destroyed"})
		} else {
			fmt.Println("destroyed")
		}
		return nil
	},
}

func init() {
	destroyCmd.Flags().BoolP("yes", "y", false, "Skip confirmation")
}

// --- exec ---

var execCmd = &cobra.Command{
	Use:               "exec <id|name> [--] <command...>",
	Short:             "Execute a command in a sandbox",
	Long: `Execute a command inside a sandbox. The exit code is forwarded.
Sleeping sandboxes wake automatically.`,
	Example: `  bhatti exec dev -- echo hello
  bhatti exec dev echo hello           # -- is optional
  bhatti exec dev -- sudo apt-get install -y ripgrep
  bhatti exec dev --timeout 60 -- long-running-script.sh`,
	Args:              cobra.MinimumNArgs(1),
	ValidArgsFunction: completeSandboxNames,
	RunE: func(cmd *cobra.Command, args []string) error {
		setupTiming(cmd)
		defer printTiming()

		// args[0] is the sandbox name/ID. Everything after "--" ends up
		// as the remaining args — Cobra strips the separator.
		target := args[0]
		cmdArgs := args[1:]
		if len(cmdArgs) == 0 {
			return cmd.Help()
		}

		id, err := resolveID(target)
		if err != nil {
			return err
		}

		timeout, _ := cmd.Flags().GetInt("timeout")
		reqBody := map[string]any{"cmd": cmdArgs}
		if timeout > 0 {
			reqBody["timeout_sec"] = timeout
		}

		var result struct {
			ExitCode int    `json:"exit_code"`
			Stdout   string `json:"stdout"`
			Stderr   string `json:"stderr"`
		}
		if err := apiJSON("POST", "/sandboxes/"+id+"/exec", reqBody, &result); err != nil {
			return err
		}

		if isJSON(cmd) {
			outputJSON(result)
		} else {
			os.Stdout.WriteString(result.Stdout)
			os.Stderr.WriteString(result.Stderr)
		}
		// Print timing before os.Exit (defer won't run)
		printTiming()
		os.Exit(result.ExitCode)
		return nil
	},
}

func init() {
	execCmd.Flags().Int("timeout", 0, "Exec timeout in seconds (default: 300, max: 3600)")
}

// --- shell ---

var shellCmd = &cobra.Command{
	Use:               "shell <id|name>",
	Aliases:           []string{"sh"},
	Short:             "Open an interactive shell",
	Long: `Open an interactive terminal inside the sandbox. Ctrl+\ to detach —
the shell keeps running. Reconnect with 'bhatti shell' again.`,
	Example: `  bhatti shell dev
  bhatti sh dev        # alias`,
	Args:              cobra.ExactArgs(1),
	ValidArgsFunction: completeSandboxNames,
	RunE: func(cmd *cobra.Command, args []string) error {
		id, err := resolveID(args[0])
		if err != nil {
			return err
		}

		forceNew, _ := cmd.Flags().GetBool("new")

		wsURL := strings.Replace(apiURL, "http://", "ws://", 1)
		wsURL = strings.Replace(wsURL, "https://", "wss://", 1)
		endpoint := wsURL + "/sandboxes/" + id + "/ws"
		if forceNew {
			endpoint += "?new=true"
		}
		header := http.Header{}
		if apiToken != "" {
			header.Set("Authorization", "Bearer "+apiToken)
		}
		conn, _, err := websocket.DefaultDialer.Dial(endpoint, header)
		if err != nil {
			return err
		}
		defer conn.Close()

		const (
			pongTimeout  = 90 * time.Second
			writeTimeout = 10 * time.Second
		)

		// Ping/pong keepalives. The server sends pings; we respond
		// with pongs and reset our read deadline on each ping.
		conn.SetReadDeadline(time.Now().Add(pongTimeout))
		conn.SetPongHandler(func(string) error {
			conn.SetReadDeadline(time.Now().Add(pongTimeout))
			return nil
		})

		// Serialize all WebSocket writes. gorilla allows one concurrent
		// reader + one concurrent writer, but we have three write sources:
		// stdin, SIGWINCH resize, and PingHandler pong replies.
		var wsMu sync.Mutex
		wsWrite := func(msgType int, data []byte) error {
			wsMu.Lock()
			defer wsMu.Unlock()
			conn.SetWriteDeadline(time.Now().Add(writeTimeout))
			return conn.WriteMessage(msgType, data)
		}
		wsWriteJSON := func(v any) error {
			wsMu.Lock()
			defer wsMu.Unlock()
			conn.SetWriteDeadline(time.Now().Add(writeTimeout))
			return conn.WriteJSON(v)
		}

		// Custom PingHandler: reset read deadline + send pong under lock.
		conn.SetPingHandler(func(appData string) error {
			conn.SetReadDeadline(time.Now().Add(pongTimeout))
			wsMu.Lock()
			defer wsMu.Unlock()
			conn.SetWriteDeadline(time.Now().Add(writeTimeout))
			err := conn.WriteMessage(websocket.PongMessage, []byte(appData))
			if err != nil {
				// Pong write failed — connection is dead. Close so
				// ReadMessage returns immediately.
				conn.Close()
			}
			return err
		})

		// Raw terminal mode
		oldState, err := term.MakeRaw(int(os.Stdin.Fd()))
		if err != nil {
			return err
		}
		defer term.Restore(int(os.Stdin.Fd()), oldState)

		// Initial size
		w, h, _ := term.GetSize(int(os.Stdin.Fd()))
		wsWriteJSON(map[string]any{"type": "resize", "rows": h, "cols": w})

		// SIGWINCH → resize
		sigwinch := make(chan os.Signal, 1)
		signal.Notify(sigwinch, syscall.SIGWINCH)
		go func() {
			for range sigwinch {
				w, h, _ := term.GetSize(int(os.Stdin.Fd()))
				wsWriteJSON(map[string]any{
					"type": "resize", "rows": h, "cols": w,
				})
			}
		}()

		var userDetached atomic.Bool
		var cleanExit atomic.Bool
		var sessionID string

		// WebSocket → stdout
		done := make(chan struct{})
		go func() {
			defer close(done)
			for {
				msgType, msg, err := conn.ReadMessage()
				if err != nil {
					// CloseNormalClosure means the shell process exited
					// (Ctrl+D / exit). Anything else is a real disconnection.
					if websocket.IsCloseError(err, websocket.CloseNormalClosure) {
						cleanExit.Store(true)
					}
					return
				}
				// Parse session info message (sent once on connect).
				if msgType == websocket.TextMessage {
					var meta struct {
						Type      string `json:"type"`
						SessionID string `json:"session_id"`
					}
					if json.Unmarshal(msg, &meta) == nil && meta.Type == "session" {
						sessionID = meta.SessionID
						continue
					}
				}
				os.Stdout.Write(msg)
			}
		}()

		// stdin → WebSocket (Ctrl+\ = detach)
		go func() {
			buf := make([]byte, 4096)
			for {
				n, err := os.Stdin.Read(buf)
				if err != nil {
					conn.Close()
					return
				}
				for i := 0; i < n; i++ {
					if buf[i] == 0x1c { // Ctrl+backslash
						// Send bytes before the escape character.
						if i > 0 {
							wsWrite(websocket.BinaryMessage, buf[:i])
						}
						userDetached.Store(true)
						term.Restore(int(os.Stdin.Fd()), oldState)
						fmt.Fprintf(os.Stderr, "\r\ndetached\r\n")
						conn.Close()
						return
					}
				}
				wsWrite(websocket.BinaryMessage, buf[:n])
			}
		}()

		<-done
		// Restore terminal before printing (defer may not have run yet
		// if we got here via the reader goroutine closing done).
		term.Restore(int(os.Stdin.Fd()), oldState)
		if !userDetached.Load() {
			if cleanExit.Load() {
				// Shell exited normally (Ctrl+D / exit command).
				// Nothing to reconnect to.
			} else {
				fmt.Fprintf(os.Stderr, "\r\nconnection lost")
				if sessionID != "" {
					fmt.Fprintf(os.Stderr, " (session %s still running)", sessionID)
				}
				fmt.Fprintf(os.Stderr, "\r\nreconnect: bhatti shell %s\r\n", args[0])
			}
		}
		return nil
	},
}

// --- ps ---

var psCmd = &cobra.Command{
	Use:               "ps <id|name>",
	Short:             "List sessions in a sandbox",
	Example: `  bhatti ps dev
  bhatti ps dev --json`,
	Args:              cobra.ExactArgs(1),
	ValidArgsFunction: completeSandboxNames,
	RunE: func(cmd *cobra.Command, args []string) error {
		setupTiming(cmd)
		defer printTiming()

		id, err := resolveID(args[0])
		if err != nil {
			return err
		}

		var sessions []struct {
			SessionID string `json:"session_id"`
			Argv      string `json:"argv"`
			Running   bool   `json:"running"`
			Attached  bool   `json:"attached"`
		}
		if err := apiJSON("GET", "/sandboxes/"+id+"/sessions", nil, &sessions); err != nil {
			return err
		}
		if isJSON(cmd) {
			outputJSON(sessions)
		} else {
			fmt.Printf("%-10s %-40s %-8s %-8s\n", "ID", "COMMAND", "RUNNING", "ATTACHED")
			for _, s := range sessions {
				fmt.Printf("%-10s %-40s %-8v %-8v\n",
					s.SessionID, s.Argv, s.Running, s.Attached)
			}
		}
		return nil
	},
}

// --- file ---

var fileCmd = &cobra.Command{
	Use:   "file <read|write|ls> <id|name> <path>",
	Short: "Read, write, and list files in a sandbox",
	Example: `  bhatti file read dev /workspace/app.js
  echo 'hello' | bhatti file write dev /workspace/greeting.txt
  bhatti file ls dev /workspace/`,
}

var fileReadCmd = &cobra.Command{
	Use:               "read <id|name> <path>",
	Short:             "Read a file from a sandbox",
	Args:              cobra.ExactArgs(2),
	ValidArgsFunction: completeSandboxNames,
	RunE: func(cmd *cobra.Command, args []string) error {
		setupTiming(cmd)
		defer printTiming()

		id, err := resolveID(args[0])
		if err != nil {
			return err
		}
		resp, err := apiRequest("GET",
			"/sandboxes/"+id+"/files?path="+url.QueryEscape(args[1]), nil)
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		if resp.StatusCode >= 400 {
			body, _ := io.ReadAll(resp.Body)
			return fmt.Errorf("%s", body)
		}
		io.Copy(os.Stdout, resp.Body)
		return nil
	},
}

var fileWriteCmd = &cobra.Command{
	Use:               "write <id|name> <path>",
	Short:             "Write a file to a sandbox (reads from stdin)",
	Args:              cobra.ExactArgs(2),
	ValidArgsFunction: completeSandboxNames,
	RunE: func(cmd *cobra.Command, args []string) error {
		setupTiming(cmd)
		defer printTiming()

		id, err := resolveID(args[0])
		if err != nil {
			return err
		}
		// Read all stdin to get Content-Length
		data, _ := io.ReadAll(os.Stdin)
		req, _ := http.NewRequest("PUT",
			apiURL+"/sandboxes/"+id+"/files?path="+url.QueryEscape(args[1]),
			bytes.NewReader(data))
		req.Header.Set("Content-Type", "application/octet-stream")
		req.ContentLength = int64(len(data))
		if apiToken != "" {
			req.Header.Set("Authorization", "Bearer "+apiToken)
		}
		resp, err := httpClient().Do(req)
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		if resp.StatusCode >= 400 {
			body, _ := io.ReadAll(resp.Body)
			return fmt.Errorf("%s", body)
		}
		fmt.Println("ok")
		return nil
	},
}

var fileLSCmd = &cobra.Command{
	Use:               "ls <id|name> <path>",
	Short:             "List files in a sandbox directory",
	Args:              cobra.ExactArgs(2),
	ValidArgsFunction: completeSandboxNames,
	RunE: func(cmd *cobra.Command, args []string) error {
		setupTiming(cmd)
		defer printTiming()

		id, err := resolveID(args[0])
		if err != nil {
			return err
		}
		var files []struct {
			Name  string `json:"name"`
			Size  int64  `json:"size"`
			IsDir bool   `json:"is_dir"`
			Mode  string `json:"mode"`
		}
		if err := apiJSON("GET",
			"/sandboxes/"+id+"/files?path="+url.QueryEscape(args[1])+"&ls=true",
			nil, &files); err != nil {
			return err
		}
		if isJSON(cmd) {
			outputJSON(files)
		} else {
			for _, f := range files {
				dirFlag := "-"
				if f.IsDir {
					dirFlag = "d"
				}
				fmt.Printf("%s%s %8d %s\n", dirFlag, f.Mode, f.Size, f.Name)
			}
		}
		return nil
	},
}

func init() {
	fileCmd.AddCommand(fileReadCmd)
	fileCmd.AddCommand(fileWriteCmd)
	fileCmd.AddCommand(fileLSCmd)
}

// --- secret ---

var secretCmd = &cobra.Command{
	Use:   "secret <set|list|delete>",
	Short: "Manage encrypted secrets",
	Long: `Secrets are encrypted at rest (age) and scoped to your API key.
They can be referenced in templates and injected into sandboxes at boot.`,
	Example: `  bhatti secret set API_KEY sk-abc123
  bhatti secret list
  bhatti secret delete API_KEY`,
}

var secretSetCmd = &cobra.Command{
	Use:   "set <name> <value>",
	Short: "Create or update a secret",
	Args:  cobra.ExactArgs(2),
	RunE: func(cmd *cobra.Command, args []string) error {
		setupTiming(cmd)
		defer printTiming()

		if err := apiJSON("POST", "/secrets", map[string]any{
			"name": args[0], "value": args[1],
		}, nil); err != nil {
			return err
		}
		fmt.Println("ok")
		return nil
	},
}

var secretListCmd = &cobra.Command{
	Use:   "list",
	Short: "List secrets",
	RunE: func(cmd *cobra.Command, args []string) error {
		setupTiming(cmd)
		defer printTiming()

		var secrets []struct {
			Name string `json:"name"`
		}
		if err := apiJSON("GET", "/secrets", nil, &secrets); err != nil {
			return err
		}
		if isJSON(cmd) {
			outputJSON(secrets)
		} else {
			for _, s := range secrets {
				fmt.Println(s.Name)
			}
		}
		return nil
	},
}

var secretDeleteCmd = &cobra.Command{
	Use:   "delete <name>",
	Short: "Delete a secret",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		setupTiming(cmd)
		defer printTiming()

		if err := apiJSON("DELETE", "/secrets/"+args[0], nil, nil); err != nil {
			return err
		}
		fmt.Println("deleted")
		return nil
	},
}

func init() {
	secretCmd.AddCommand(secretSetCmd)
	secretCmd.AddCommand(secretListCmd)
	secretCmd.AddCommand(secretDeleteCmd)
}

// --- user (local commands — operate on SQLite directly) ---

var userCmd = &cobra.Command{
	Use:   "user <create|list|delete|rotate-key>",
	Short: "Manage users (requires DB access)",
	Long: `User management operates directly on the local SQLite database.
Run on the server, not remotely.`,
	Example: `  sudo bhatti user create --name alice --max-sandboxes 10
  sudo bhatti user list
  sudo bhatti user rotate-key alice
  sudo bhatti user delete alice`,
}

var userCreateCmd = &cobra.Command{
	Use:   "create",
	Short: "Create a new user",
	RunE: func(cmd *cobra.Command, args []string) error {
		name, _ := cmd.Flags().GetString("name")
		maxSandboxes, _ := cmd.Flags().GetInt("max-sandboxes")
		maxCPUs, _ := cmd.Flags().GetInt("max-cpus")
		maxMemory, _ := cmd.Flags().GetInt("max-memory")

		if name == "" {
			return fmt.Errorf("--name is required")
		}

		st := openLocalStore()
		defer st.Close()

		apiKey := generateAPIKey()
		keyHash := sha256HexCLI(apiKey)

		subnetIdx, err := st.NextSubnetIndex()
		if err != nil {
			return fmt.Errorf("subnet index: %w", err)
		}

		idBytes := make([]byte, 4)
		rand.Read(idBytes)
		userID := "usr_" + hex.EncodeToString(idBytes)

		u := store.User{
			ID:                    userID,
			Name:                  name,
			APIKeyHash:            keyHash,
			MaxSandboxes:          maxSandboxes,
			MaxCPUsPerSandbox:     maxCPUs,
			MaxMemoryMBPerSandbox: maxMemory,
			SubnetIndex:           subnetIdx,
			CreatedAt:             time.Now(),
		}

		if err := st.CreateUser(u); err != nil {
			if strings.Contains(err.Error(), "UNIQUE") {
				return fmt.Errorf("user %q already exists", name)
			}
			return fmt.Errorf("create user: %w", err)
		}

		fmt.Printf("User created:\n")
		fmt.Printf("  ID:       %s\n", u.ID)
		fmt.Printf("  Name:     %s\n", u.Name)
		fmt.Printf("  Subnet:   %d\n", u.SubnetIndex)
		fmt.Printf("  API key:  %s\n", apiKey)
		fmt.Println()
		fmt.Println("This key will not be shown again. Save it now.")
		fmt.Printf("\nQuick start:\n")
		fmt.Printf("  export BHATTI_URL=%s\n", apiURL)
		fmt.Printf("  export BHATTI_TOKEN=%s\n", apiKey)
		fmt.Printf("  bhatti create --name my-sandbox\n")
		return nil
	},
}

var userListCmd = &cobra.Command{
	Use:   "list",
	Short: "List users",
	RunE: func(cmd *cobra.Command, args []string) error {
		st := openLocalStore()
		defer st.Close()

		users, err := st.ListUsers()
		if err != nil {
			return fmt.Errorf("list users: %w", err)
		}
		if isJSON(cmd) {
			outputJSON(users)
		} else {
			fmt.Printf("%-12s %-20s %-8s %-6s %-6s %-8s\n",
				"ID", "NAME", "SANDBOXES", "CPUS", "MEM", "SUBNET")
			for _, u := range users {
				count, _ := st.CountUserSandboxes(u.ID)
				fmt.Printf("%-12s %-20s %d/%-6d %-6d %-6d %-8d\n",
					u.ID, u.Name, count, u.MaxSandboxes,
					u.MaxCPUsPerSandbox, u.MaxMemoryMBPerSandbox, u.SubnetIndex)
			}
		}
		return nil
	},
}

var userDeleteCmd = &cobra.Command{
	Use:   "delete <name>",
	Short: "Delete a user",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		if !confirmAction(cmd, fmt.Sprintf("Delete user %q?", args[0])) {
			return nil
		}

		st := openLocalStore()
		defer st.Close()

		users, _ := st.ListUsers()
		var userID string
		for _, u := range users {
			if u.Name == args[0] {
				userID = u.ID
				break
			}
		}
		if userID == "" {
			return fmt.Errorf("user %q not found", args[0])
		}
		if err := st.DeleteUser(userID); err != nil {
			return err
		}
		fmt.Printf("deleted user %q\n", args[0])
		return nil
	},
}

var userRotateKeyCmd = &cobra.Command{
	Use:   "rotate-key <name>",
	Short: "Rotate a user's API key",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		st := openLocalStore()
		defer st.Close()

		users, _ := st.ListUsers()
		var userID string
		for _, u := range users {
			if u.Name == args[0] {
				userID = u.ID
				break
			}
		}
		if userID == "" {
			return fmt.Errorf("user %q not found", args[0])
		}

		newKey := generateAPIKey()
		newHash := sha256HexCLI(newKey)

		if err := st.RotateUserKey(userID, newHash); err != nil {
			return err
		}

		fmt.Printf("API key rotated for %q\n", args[0])
		fmt.Printf("  New key: %s\n", newKey)
		fmt.Println()
		fmt.Println("The old key is immediately invalidated.")
		fmt.Println("This key will not be shown again. Save it now.")
		return nil
	},
}

func init() {
	userCreateCmd.Flags().String("name", "", "User name (required)")
	userCreateCmd.Flags().Int("max-sandboxes", 5, "Max sandboxes for this user")
	userCreateCmd.Flags().Int("max-cpus", 4, "Max CPUs per sandbox")
	userCreateCmd.Flags().Int("max-memory", 4096, "Max memory MB per sandbox")

	userCmd.AddCommand(userCreateCmd)
	userCmd.AddCommand(userListCmd)
	userDeleteCmd.Flags().BoolP("yes", "y", false, "Skip confirmation")
	userCmd.AddCommand(userDeleteCmd)
	userCmd.AddCommand(userRotateKeyCmd)
}

// openLocalStore opens the SQLite store for user admin commands.
//
// Resolution order for data_dir:
//  1. --data-dir flag (explicit)
//  2. config file's data_dir field (from LoadConfig — if it was set)
//  3. /var/lib/bhatti if config.yaml exists there (standard server path)
//  4. ~/.bhatti (default)
//
// Step 3 prevents the split-brain bug where `bhatti user rotate-key`
// run from ~ opens ~/.bhatti/state.db while the daemon uses
// /var/lib/bhatti/state.db (because ~/.bhatti/config.yaml has no
// data_dir field and LoadConfig defaults to ~/.bhatti).
func openLocalStore() *store.Store {
	// Check --data-dir flag first
	dataDir, _ := rootCmd.PersistentFlags().GetString("data-dir")

	if dataDir == "" {
		cfg, err := pkg.LoadConfig()
		if err != nil {
			fmt.Fprintf(os.Stderr, "load config: %v\n", err)
			os.Exit(1)
		}
		dataDir = cfg.DataDir

		// If LoadConfig didn't find an explicit data_dir (fell back to
		// ~/.bhatti), check the standard server location. This handles
		// both `bhatti user rotate-key` (state.db exists) and fresh
		// installs (config.yaml exists but state.db doesn't yet).
		if dataDir == pkg.DefaultDataDir() {
			const serverDataDir = "/var/lib/bhatti"
			if _, err := os.Stat(filepath.Join(serverDataDir, "config.yaml")); err == nil {
				dataDir = serverDataDir
			}
		}
	}

	dbPath := filepath.Join(dataDir, "state.db")
	st, err := store.New(dbPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "open store %s: %v\n", dbPath, err)
		os.Exit(1)
	}
	return st
}

func generateAPIKey() string {
	b := make([]byte, 32)
	rand.Read(b)
	return "bht_" + hex.EncodeToString(b)
}

func sha256HexCLI(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

// --- setup ---

var setupCmd = &cobra.Command{
	Use:   "setup",
	Short: "Configure CLI endpoint and API key",
	Long: `Interactive setup for remote CLI users. Prompts for the API endpoint
and API key, saves to ~/.bhatti/config.yaml, and tests the connection.`,
	Example: `  bhatti setup`,
	RunE: func(cmd *cobra.Command, args []string) error {
		fmt.Printf("API endpoint [%s]: ", apiURL)
		var endpoint string
		fmt.Scanln(&endpoint)
		if endpoint == "" {
			endpoint = apiURL
		}

		fmt.Print("API key: ")
		keyBytes, err := term.ReadPassword(int(os.Stdin.Fd()))
		fmt.Println()
		if err != nil {
			return fmt.Errorf("read key: %w", err)
		}
		key := strings.TrimSpace(string(keyBytes))
		if key == "" {
			return fmt.Errorf("API key is required")
		}

		// Write config
		cfgDir := pkg.DefaultDataDir()
		os.MkdirAll(cfgDir, 0700)
		cfgPath := filepath.Join(cfgDir, "config.yaml")

		var cfgContent string
		if strings.HasPrefix(endpoint, "https://") || strings.HasPrefix(endpoint, "http://") {
			// Remote endpoint — save URL and token
			cfgContent = fmt.Sprintf("api_url: %s\nauth_token: %s\n", endpoint, key)
		} else {
			// Local — save listen address and token
			cfgContent = fmt.Sprintf("listen: %s\nauth_token: %s\n", endpoint, key)
		}

		if err := os.WriteFile(cfgPath, []byte(cfgContent), 0600); err != nil {
			return fmt.Errorf("write config: %w", err)
		}
		fmt.Printf("Saved to %s\n", cfgPath)

		// Test connection using an authenticated endpoint.
		// /health is unauthenticated — it only proves the server is
		// reachable, not that the API key is valid. Use /sandboxes
		// (GET) which requires auth, so we catch bad keys immediately.
		fmt.Print("Testing connection... ")
		apiURL = endpoint
		apiToken = key
		var sandboxes []any
		if err := apiJSON("GET", "/sandboxes", nil, &sandboxes); err != nil {
			fmt.Printf("✗ %v\n", err)
			return nil
		}
		fmt.Printf("✓ authenticated (%d sandboxes)\n", len(sandboxes))

		// Suggest shell completions
		shell := os.Getenv("SHELL")
		switch {
		case strings.HasSuffix(shell, "/zsh"):
			fmt.Println("\nEnable completions:")
			fmt.Println("  echo 'source <(bhatti completion zsh)' >> ~/.zshrc")
		case strings.HasSuffix(shell, "/bash"):
			fmt.Println("\nEnable completions:")
			fmt.Println("  echo 'source <(bhatti completion bash)' >> ~/.bashrc")
		case strings.HasSuffix(shell, "/fish"):
			fmt.Println("\nEnable completions:")
			fmt.Println("  bhatti completion fish > ~/.config/fish/completions/bhatti.fish")
		}
		return nil
	},
}

// --- volume ---

var volumeCmd = &cobra.Command{
	Use:   "volume <create|list|delete|resize>",
	Short: "Manage persistent volumes",
	Long: `Persistent volumes are ext4 filesystems that survive sandbox destruction.
Attach them with '--volume name:/mount' on create.`,
	Example: `  bhatti volume create --name workspace --size 5120
  bhatti create --name dev --volume workspace:/workspace
  bhatti volume resize workspace --size 10240
  bhatti volume list`,
}

var volumeCreateCmd = &cobra.Command{
	Use:   "create",
	Short: "Create a persistent volume",
	Example: `  bhatti volume create --name workspace --size 5120
  bhatti volume create --name data --size 20480`,
	RunE: func(cmd *cobra.Command, args []string) error {
		setupTiming(cmd)
		defer printTiming()

		name, _ := cmd.Flags().GetString("name")
		sizeMB, _ := cmd.Flags().GetInt("size")
		if name == "" || sizeMB <= 0 {
			return fmt.Errorf("--name and --size (> 0) required")
		}

		var vol struct {
			ID     string `json:"id"`
			Name   string `json:"name"`
			SizeMB int    `json:"size_mb"`
			Status string `json:"status"`
		}
		if err := apiJSON("POST", "/volumes", map[string]any{
			"name": name, "size_mb": sizeMB,
		}, &vol); err != nil {
			return err
		}
		if isJSON(cmd) {
			outputJSON(vol)
		} else {
			fmt.Printf("%s\t%s\t%dMB\t%s\n", vol.ID, vol.Name, vol.SizeMB, vol.Status)
		}
		return nil
	},
}

var volumeListCmd = &cobra.Command{
	Use:   "list",
	Short: "List persistent volumes",
	RunE: func(cmd *cobra.Command, args []string) error {
		setupTiming(cmd)
		defer printTiming()

		var volumes []struct {
			ID     string `json:"id"`
			Name   string `json:"name"`
			SizeMB int    `json:"size_mb"`
			Status string `json:"status"`
		}
		if err := apiJSON("GET", "/volumes", nil, &volumes); err != nil {
			return err
		}
		if isJSON(cmd) {
			outputJSON(volumes)
		} else {
			fmt.Printf("%-20s %-20s %-10s %-10s\n", "ID", "NAME", "SIZE", "STATUS")
			for _, v := range volumes {
				fmt.Printf("%-20s %-20s %dMB\t%-10s\n", v.ID, v.Name, v.SizeMB, v.Status)
			}
		}
		return nil
	},
}

var volumeDeleteCmd = &cobra.Command{
	Use:   "delete <name>",
	Short: "Delete a persistent volume",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		setupTiming(cmd)
		defer printTiming()

		if !confirmAction(cmd, fmt.Sprintf("Delete volume %q?", args[0])) {
			return nil
		}
		if err := apiJSON("DELETE", "/volumes/"+args[0], nil, nil); err != nil {
			return err
		}
		fmt.Println("deleted")
		return nil
	},
}

var volumeResizeCmd = &cobra.Command{
	Use:   "resize <name>",
	Short: "Resize a persistent volume (grow only)",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		setupTiming(cmd)
		defer printTiming()

		sizeMB, _ := cmd.Flags().GetInt("size")
		if sizeMB <= 0 {
			return fmt.Errorf("--size (> 0) required")
		}

		if err := apiJSON("POST", "/volumes/"+args[0]+"/resize", map[string]any{
			"size_mb": sizeMB,
		}, nil); err != nil {
			return err
		}
		fmt.Printf("resized to %dMB\n", sizeMB)
		return nil
	},
}

func init() {
	volumeCreateCmd.Flags().String("name", "", "Volume name (required)")
	volumeCreateCmd.Flags().Int("size", 0, "Size in MB (required)")
	volumeResizeCmd.Flags().Int("size", 0, "New size in MB (must be larger)")

	volumeCmd.AddCommand(volumeCreateCmd)
	volumeCmd.AddCommand(volumeListCmd)
	volumeDeleteCmd.Flags().BoolP("yes", "y", false, "Skip confirmation")
	volumeCmd.AddCommand(volumeDeleteCmd)
	volumeCmd.AddCommand(volumeResizeCmd)
	volumeCmd.AddCommand(volumeBackupCmd)
	volumeCmd.AddCommand(volumeBackupListCmd)
	volumeCmd.AddCommand(volumeRestoreCmd)
	volumeBackupDeleteCmd.Flags().BoolP("yes", "y", false, "Skip confirmation")
	volumeCmd.AddCommand(volumeBackupDeleteCmd)
}

var volumeBackupCmd = &cobra.Command{
	Use:   "backup <volume-name>",
	Short: "Backup a volume to S3-compatible storage",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		setupTiming(cmd)
		defer printTiming()

		var result struct {
			ID         string `json:"id"`
			VolumeName string `json:"volume_name"`
			SizeBytes  int64  `json:"size_bytes"`
			S3Key      string `json:"s3_key"`
			CreatedAt  string `json:"created_at"`
		}
		if err := apiJSON("POST", "/volumes/"+args[0]+"/backups", nil, &result); err != nil {
			return err
		}
		if isJSON(cmd) {
			outputJSON(result)
		} else {
			fmt.Printf("backup %s created (%s, %d bytes)\n", result.ID, result.VolumeName, result.SizeBytes)
		}
		return nil
	},
}

var volumeBackupListCmd = &cobra.Command{
	Use:   "backup-list <volume-name>",
	Short: "List backups for a volume",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		setupTiming(cmd)
		defer printTiming()

		var backups []struct {
			ID        string `json:"id"`
			SizeBytes int64  `json:"size_bytes"`
			CreatedAt string `json:"created_at"`
		}
		if err := apiJSON("GET", "/volumes/"+args[0]+"/backups", nil, &backups); err != nil {
			return err
		}
		if isJSON(cmd) {
			outputJSON(backups)
		} else {
			if len(backups) == 0 {
				fmt.Println("no backups")
				return nil
			}
			fmt.Printf("%-20s %-12s %s\n", "ID", "SIZE", "CREATED")
			for _, b := range backups {
				size := fmt.Sprintf("%d MB", b.SizeBytes/1024/1024)
				fmt.Printf("%-20s %-12s %s\n", b.ID, size, b.CreatedAt)
			}
		}
		return nil
	},
}

var volumeRestoreCmd = &cobra.Command{
	Use:   "restore <volume-name> --backup-id <id>",
	Short: "Restore a volume from a backup",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		setupTiming(cmd)
		defer printTiming()

		backupID, _ := cmd.Flags().GetString("backup-id")
		if backupID == "" {
			return fmt.Errorf("--backup-id required")
		}

		var result struct {
			Status   string `json:"status"`
			BackupID string `json:"backup_id"`
		}
		if err := apiJSON("POST", "/volumes/"+args[0]+"/backups/restore", map[string]any{
			"backup_id": backupID,
		}, &result); err != nil {
			return err
		}
		fmt.Printf("volume restored from backup %s\n", backupID)
		return nil
	},
}

var volumeBackupDeleteCmd = &cobra.Command{
	Use:   "backup-delete <volume-name> <backup-id>",
	Short: "Delete a volume backup",
	Args:  cobra.ExactArgs(2),
	RunE: func(cmd *cobra.Command, args []string) error {
		setupTiming(cmd)
		defer printTiming()

		if !confirmAction(cmd, fmt.Sprintf("Delete backup %s?", args[1])) {
			return nil
		}
		if err := apiJSON("DELETE", "/volumes/"+args[0]+"/backups/"+args[1], nil, nil); err != nil {
			return err
		}
		fmt.Println("deleted")
		return nil
	},
}

func init() {
	volumeRestoreCmd.Flags().String("backup-id", "", "Backup ID to restore from (required)")
}

// --- image ---

var imageCmd = &cobra.Command{
	Use:   "image <list|pull|save|delete>",
	Short: "Manage rootfs images",
	Long: `Images are ext4 filesystem snapshots used as sandbox root filesystems.
Pull public images from registries with 'image pull'.`,
	Example: `  # Pull a public image
  bhatti image pull python:3.12

  # Save a running sandbox as an image
  bhatti image save dev --name my-custom-env

  # List images
  bhatti image list`,
}

var imageListCmd = &cobra.Command{
	Use:   "list",
	Short: "List available images",
	RunE: func(cmd *cobra.Command, args []string) error {
		setupTiming(cmd)
		defer printTiming()

		var images []struct {
			ID     string `json:"id"`
			Name   string `json:"name"`
			Source string `json:"source"`
			SizeMB int    `json:"size_mb"`
			UserID string `json:"user_id"`
		}
		if err := apiJSON("GET", "/images", nil, &images); err != nil {
			return err
		}
		if isJSON(cmd) {
			outputJSON(images)
		} else {
			fmt.Printf("%-20s %-20s %-30s %-10s %-10s\n", "ID", "NAME", "SOURCE", "SIZE", "SCOPE")
			for _, img := range images {
				scope := "admin"
				if img.UserID != "" {
					scope = "user"
				}
				fmt.Printf("%-20s %-20s %-30s %dMB\t%-10s\n",
					img.ID, img.Name, img.Source, img.SizeMB, scope)
			}
		}
		return nil
	},
}

var imageDeleteCmd = &cobra.Command{
	Use:   "delete <name>",
	Short: "Delete an image",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		setupTiming(cmd)
		defer printTiming()

		if !confirmAction(cmd, fmt.Sprintf("Delete image %q?", args[0])) {
			return nil
		}
		if err := apiJSON("DELETE", "/images/"+args[0], nil, nil); err != nil {
			return err
		}
		fmt.Println("deleted")
		return nil
	},
}

var imagePullCmd = &cobra.Command{
	Use:   "pull <ref>",
	Short: "Pull an OCI/Docker image from a public registry",
	Long: `Pull a public image from any OCI-compatible registry. The server pulls
the image and converts it to an ext4 rootfs for 'bhatti create --image'.`,
	Example: `  bhatti image pull python:3.12
  bhatti image pull ubuntu:24.04 --name ubuntu
  bhatti image pull node:22-slim --name node-22`,
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		setupTiming(cmd)
		defer printTiming()

		ref := args[0]
		name, _ := cmd.Flags().GetString("name")
		auth, _ := cmd.Flags().GetString("auth")
		if name == "" {
			// Derive name from ref: "python:3.12" → "python-3.12"
			name = strings.NewReplacer("/", "-", ":", "-", ".", "-").Replace(ref)
			// Remove registry prefix for common cases
			if idx := strings.LastIndex(name, "-"); idx > 0 {
				parts := strings.Split(ref, "/")
				if len(parts) > 1 {
					name = strings.NewReplacer("/", "-", ":", "-").Replace(
						strings.Join(parts[len(parts)-1:], "-"))
				}
			}
		}

		body := map[string]any{"ref": ref, "name": name}
		if auth != "" {
			body["auth"] = auth
		}

		var result struct {
			TaskID string `json:"task_id"`
			Status string `json:"status"`
		}
		if err := apiJSON("POST", "/images/pull", body, &result); err != nil {
			return err
		}

		if isJSON(cmd) {
			outputJSON(result)
			return nil
		}

		fmt.Printf("pulling %s as %q (task: %s)\n", ref, name, result.TaskID)

		// Poll for completion
		for {
			time.Sleep(2 * time.Second)
			var task struct {
				Status   string `json:"status"`
				Progress string `json:"progress"`
				Error    string `json:"error"`
			}
			if err := apiJSON("GET", "/tasks/"+result.TaskID, nil, &task); err != nil {
				return err
			}
			switch task.Status {
			case "completed":
				fmt.Println("done")
				return nil
			case "failed":
				if strings.Contains(task.Error, "unauthorized") ||
					strings.Contains(task.Error, "UNAUTHORIZED") ||
					strings.Contains(task.Error, "403") {
					fmt.Fprintf(os.Stderr, "This image may require authentication.\n")
					fmt.Fprintf(os.Stderr, "Pull it with Docker locally, then import:\n")
					fmt.Fprintf(os.Stderr, "  docker pull %s\n", ref)
					fmt.Fprintf(os.Stderr, "  bhatti image import %s\n", ref)
					return fmt.Errorf("pull failed: authentication required")
				}
				return fmt.Errorf("pull failed: %s", task.Error)
			default:
				if task.Progress != "" {
					fmt.Printf("\r  %s", task.Progress)
				}
			}
		}
	},
}

var imageSaveCmd = &cobra.Command{
	Use:               "save <sandbox-id|name>",
	Short:             "Save a sandbox's rootfs as an image",
	Example: `  bhatti image save dev --name my-custom-env`,
	Args:              cobra.ExactArgs(1),
	ValidArgsFunction: completeSandboxNames,
	RunE: func(cmd *cobra.Command, args []string) error {
		setupTiming(cmd)
		defer printTiming()

		id, err := resolveID(args[0])
		if err != nil {
			return err
		}
		name, _ := cmd.Flags().GetString("name")
		if name == "" {
			return fmt.Errorf("--name is required")
		}

		var img struct {
			ID     string `json:"id"`
			Name   string `json:"name"`
			SizeMB int    `json:"size_mb"`
		}
		if err := apiJSON("POST", "/sandboxes/"+id+"/save-image",
			map[string]any{"name": name}, &img); err != nil {
			return err
		}
		if isJSON(cmd) {
			outputJSON(img)
		} else {
			fmt.Printf("saved %q (%dMB)\n", img.Name, img.SizeMB)
		}
		return nil
	},
}

var imageImportCmd = &cobra.Command{
	Use:   "import <docker-ref>",
	Short: "Import a local Docker image as a bhatti rootfs",
	Long: `Import an image that exists in your local Docker daemon. The CLI runs
'docker save' on your machine and streams the result to the bhatti server,
which converts it to an ext4 rootfs for use with 'bhatti create --image'.

The image name is derived from the ref automatically. Use --name to override.
Docker must be installed locally.

For private registries, pull with Docker first (which handles auth):
  docker pull ghcr.io/org/private:latest
  bhatti image import ghcr.io/org/private:latest

For raw tarballs (no Docker needed), use --tar:
  bhatti image import --tar /path/to/image.tar --name my-image`,
	Example: `  # Import from local Docker (name derived from ref)
  bhatti image import python:3.12

  # Private image (pull with Docker first)
  docker pull ghcr.io/org/private:latest
  bhatti image import ghcr.io/org/private:latest

  # Locally built image
  docker build -t my-env .
  bhatti image import my-env

  # Custom name
  bhatti image import python:3.12 --name py312

  # From a raw tarball (--name required)
  bhatti image import --tar /tmp/image.tar --name from-tar`,
	Args: cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		setupTiming(cmd)
		defer printTiming()

		name, _ := cmd.Flags().GetString("name")
		tarPath, _ := cmd.Flags().GetString("tar")

		var body io.Reader

		if tarPath != "" {
			if name == "" {
				return fmt.Errorf("--name is required when using --tar")
			}
			f, err := os.Open(tarPath)
			if err != nil {
				return fmt.Errorf("open tarball: %w", err)
			}
			defer f.Close()
			body = f
			fmt.Fprintf(os.Stderr, "importing %q from tarball...\n", name)
		} else {
			if len(args) == 0 {
				return fmt.Errorf("docker image ref required (or use --tar)")
			}
			ref := args[0]

			if name == "" {
				name = deriveImageName(ref)
			}

			if _, err := exec.LookPath("docker"); err != nil {
				return fmt.Errorf("docker not found — install Docker or use --tar with a tarball")
			}

			check := exec.Command("docker", "image", "inspect", ref)
			check.Stdout = nil
			check.Stderr = nil
			if err := check.Run(); err != nil {
				return fmt.Errorf("image %q not found in Docker — run 'docker pull %s' first", ref, ref)
			}

			save := exec.Command("docker", "save", ref)
			stdout, err := save.StdoutPipe()
			if err != nil {
				return fmt.Errorf("docker save pipe: %w", err)
			}
			if err := save.Start(); err != nil {
				return fmt.Errorf("docker save start: %w", err)
			}
			defer save.Wait()
			body = stdout
			fmt.Fprintf(os.Stderr, "importing %q from local Docker...\n", ref)
		}

		req, err := http.NewRequest("POST",
			apiURL+"/images/import?name="+url.QueryEscape(name),
			body)
		if err != nil {
			return err
		}
		req.Header.Set("Content-Type", "application/x-tar")
		if apiToken != "" {
			req.Header.Set("Authorization", "Bearer "+apiToken)
		}

		resp, err := httpClient().Do(req)
		if err != nil {
			return err
		}
		defer resp.Body.Close()

		if resp.StatusCode >= 400 {
			var errBody struct{ Error string `json:"error"` }
			json.NewDecoder(resp.Body).Decode(&errBody)
			return fmt.Errorf("%s: %s", resp.Status, errBody.Error)
		}

		var result struct {
			Name   string `json:"name"`
			SizeMB int    `json:"size_mb"`
		}
		json.NewDecoder(resp.Body).Decode(&result)

		if isJSON(cmd) {
			outputJSON(result)
		} else {
			fmt.Printf("imported %q (%dMB)\n", result.Name, result.SizeMB)
		}
		return nil
	},
}

// deriveImageName extracts a short image name from a Docker/OCI ref.
//
//	"python:3.12"                   → "python-3.12"
//	"ghcr.io/org/private:latest"    → "private-latest"
//	"my-env"                        → "my-env"
//	"docker.io/library/python:3.12" → "python-3.12"
func deriveImageName(ref string) string {
	parts := strings.Split(ref, "/")
	base := parts[len(parts)-1]
	return strings.ReplaceAll(base, ":", "-")
}

func init() {
	imagePullCmd.Flags().String("name", "", "Image name (default: derived from ref)")
	imagePullCmd.Flags().String("auth", "", "Registry auth (user:token)")
	imageSaveCmd.Flags().String("name", "", "Image name (required)")
	imageImportCmd.Flags().String("name", "", "Image name (default: derived from ref)")
	imageImportCmd.Flags().String("tar", "", "Import from tarball path instead of Docker")

	imageCmd.AddCommand(imageListCmd)
	imageDeleteCmd.Flags().BoolP("yes", "y", false, "Skip confirmation")
	imageCmd.AddCommand(imageDeleteCmd)
	imageCmd.AddCommand(imagePullCmd)
	imageCmd.AddCommand(imageImportCmd)
	imageCmd.AddCommand(imageSaveCmd)
	imageShareCmd.Flags().StringSlice("user", nil, "User name(s) to share with")
	imageShareCmd.Flags().Bool("list", false, "List current shares")
	imageCmd.AddCommand(imageShareCmd)
	imageUnshareCmd.Flags().StringSlice("user", nil, "User name(s) to unshare from")
	imageCmd.AddCommand(imageUnshareCmd)
}

var imageShareCmd = &cobra.Command{
	Use:   "share <image-name>",
	Short: "Share an image with other users (requires DB access)",
	Example: `  sudo bhatti image share spc-golden --user kowshik --user sumo
  sudo bhatti image share spc-golden --list`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		st := openLocalStore()
		defer st.Close()

		imageName := args[0]
		listShares, _ := cmd.Flags().GetBool("list")
		users, _ := cmd.Flags().GetStringSlice("user")

		img, err := st.GetImageByName(imageName)
		if err != nil {
			return err
		}

		if listShares {
			shares, _ := st.ListImageShares(img.ID)
			if len(shares) == 0 {
				fmt.Printf("%s: not shared\n", imageName)
			} else {
				fmt.Printf("%s shared with: %s\n", imageName, strings.Join(shares, ", "))
			}
			return nil
		}

		if len(users) == 0 {
			return fmt.Errorf("--user required (or use --list)")
		}

		for _, userName := range users {
			user, err := st.GetUserByName(userName)
			if err != nil {
				return err
			}
			if err := st.ShareImage(img.ID, user.ID); err != nil {
				return fmt.Errorf("share with %q: %w", userName, err)
			}
		}
		fmt.Printf("shared %q with: %s\n", imageName, strings.Join(users, ", "))
		return nil
	},
}

var imageUnshareCmd = &cobra.Command{
	Use:   "unshare <image-name>",
	Short: "Revoke image access from users (requires DB access)",
	Example: `  sudo bhatti image unshare spc-golden --user sumo`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		st := openLocalStore()
		defer st.Close()

		imageName := args[0]
		users, _ := cmd.Flags().GetStringSlice("user")

		if len(users) == 0 {
			return fmt.Errorf("--user required")
		}

		img, err := st.GetImageByName(imageName)
		if err != nil {
			return err
		}

		for _, userName := range users {
			user, err := st.GetUserByName(userName)
			if err != nil {
				return err
			}
			st.UnshareImage(img.ID, user.ID)
		}
		fmt.Printf("unshared %q from: %s\n", imageName, strings.Join(users, ", "))
		return nil
	},
}

// --- snapshot ---

var snapshotCmd = &cobra.Command{
	Use:   "snapshot <create|list|resume|delete>",
	Short: "Manage named VM snapshots",
	Long: `Snapshots capture the entire VM state: memory, CPU, disk. Resume
produces an exact continuation — processes running, files open.`,
	Example: `  bhatti snapshot create dev --name dev-ready
  bhatti snapshot resume dev-ready --name dev-2
  bhatti snapshot list`,
}

var snapshotCreateCmd = &cobra.Command{
	Use:               "create <sandbox-id|name>",
	Short:             "Checkpoint a running sandbox",
	Args:              cobra.ExactArgs(1),
	ValidArgsFunction: completeSandboxNames,
	RunE: func(cmd *cobra.Command, args []string) error {
		setupTiming(cmd)
		defer printTiming()

		id, err := resolveID(args[0])
		if err != nil {
			return err
		}
		name, _ := cmd.Flags().GetString("name")
		if name == "" {
			return fmt.Errorf("--name is required")
		}

		var snap struct {
			ID     string `json:"id"`
			Name   string `json:"name"`
			SizeMB int    `json:"size_mb"`
		}
		if err := apiJSON("POST", "/sandboxes/"+id+"/checkpoint",
			map[string]any{"name": name}, &snap); err != nil {
			return err
		}
		if isJSON(cmd) {
			outputJSON(snap)
		} else {
			fmt.Printf("checkpoint %q created (%dMB)\n", snap.Name, snap.SizeMB)
		}
		return nil
	},
}

var snapshotListCmd = &cobra.Command{
	Use:   "list",
	Short: "List snapshots",
	RunE: func(cmd *cobra.Command, args []string) error {
		setupTiming(cmd)
		defer printTiming()

		var snaps []struct {
			ID            string `json:"id"`
			Name          string `json:"name"`
			SourceSandbox string `json:"source_sandbox"`
			SizeMB        int    `json:"size_mb"`
		}
		if err := apiJSON("GET", "/snapshots", nil, &snaps); err != nil {
			return err
		}
		if isJSON(cmd) {
			outputJSON(snaps)
		} else {
			fmt.Printf("%-20s %-20s %-20s %-10s\n", "ID", "NAME", "SOURCE", "SIZE")
			for _, s := range snaps {
				fmt.Printf("%-20s %-20s %-20s %dMB\n", s.ID, s.Name, s.SourceSandbox, s.SizeMB)
			}
		}
		return nil
	},
}

var snapshotResumeCmd = &cobra.Command{
	Use:   "resume <snapshot-name>",
	Short: "Resume a sandbox from a snapshot",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		setupTiming(cmd)
		defer printTiming()

		name, _ := cmd.Flags().GetString("name")

		var sb struct {
			ID   string `json:"id"`
			Name string `json:"name"`
			IP   string `json:"ip"`
		}
		body := map[string]any{}
		if name != "" {
			body["name"] = name
		}
		if err := apiJSON("POST", "/snapshots/"+args[0]+"/resume", body, &sb); err != nil {
			return err
		}
		if isJSON(cmd) {
			outputJSON(sb)
		} else {
			fmt.Printf("%s\t%s\t%s\n", sb.ID, sb.Name, sb.IP)
		}
		return nil
	},
}

var snapshotDeleteCmd = &cobra.Command{
	Use:   "delete <snapshot-name>",
	Short: "Delete a snapshot",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		setupTiming(cmd)
		defer printTiming()

		if !confirmAction(cmd, fmt.Sprintf("Delete snapshot %q?", args[0])) {
			return nil
		}
		if err := apiJSON("DELETE", "/snapshots/"+args[0], nil, nil); err != nil {
			return err
		}
		fmt.Println("deleted")
		return nil
	},
}

func init() {
	snapshotCreateCmd.Flags().String("name", "", "Snapshot name (required)")
	snapshotResumeCmd.Flags().String("name", "", "New sandbox name")

	snapshotCmd.AddCommand(snapshotCreateCmd)
	snapshotCmd.AddCommand(snapshotListCmd)
	snapshotCmd.AddCommand(snapshotResumeCmd)
	snapshotDeleteCmd.Flags().BoolP("yes", "y", false, "Skip confirmation")
	snapshotCmd.AddCommand(snapshotDeleteCmd)
}

// --- update ---

var updateCmd = &cobra.Command{
	Use:   "update",
	Short: "Update bhatti CLI to the latest version",
	RunE: func(cmd *cobra.Command, args []string) error {
		fmt.Printf("Current version: %s\n", version)

		// Find a shell to run the installer
		shellBin := "/bin/sh"
		if _, err := os.Stat("/bin/bash"); err == nil {
			shellBin = "/bin/bash"
		}

		fmt.Println("Downloading latest version...")
		install := exec.Command(shellBin, "-c", "curl -fsSL bhatti.sh/install | bash")
		install.Stdout = os.Stdout
		install.Stderr = os.Stderr
		install.Env = append(os.Environ(), "BHATTI_MODE=cli")
		return install.Run()
	},
}

// --- version ---

var versionCmd = &cobra.Command{
	Use:   "version",
	Short: "Print version and API endpoint",
	Example: `  bhatti version
  bhatti version --json`,
	Run: func(cmd *cobra.Command, args []string) {
		if isJSON(cmd) {
			outputJSON(map[string]string{
				"version": version,
				"api":     apiURL,
			})
		} else {
			fmt.Printf("bhatti %s\n", version)
			fmt.Printf("api: %s\n", apiURL)
		}
	},
}

// --- publish / unpublish ---

var publishCmd = &cobra.Command{
	Use:               "publish <sandbox> -p <port> [-a <alias>]",
	Short:             "Publish a sandbox port with a public URL",
	Example: `  bhatti publish dev -p 3000
  bhatti publish dev -p 3000 -a my-app`,
	Args:              cobra.ExactArgs(1),
	ValidArgsFunction: completeSandboxNames,
	Run: func(cmd *cobra.Command, args []string) {
		port, _ := cmd.Flags().GetInt("port")
		alias, _ := cmd.Flags().GetString("alias")

		body := map[string]interface{}{"port": port}
		if alias != "" {
			body["alias"] = alias
		}
		id, err := resolveID(args[0])
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			os.Exit(1)
		}
		var result map[string]interface{}
		if err := apiJSON("POST", fmt.Sprintf("/sandboxes/%s/publish", id), body, &result); err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			os.Exit(1)
		}
		if isJSON(cmd) {
			outputJSON(result)
		} else {
			fmt.Printf("Published: %v\n", result["url"])
		}
	},
}

var unpublishCmd = &cobra.Command{
	Use:               "unpublish <sandbox> -p <port>",
	Short:             "Unpublish a sandbox port",
	Example: `  bhatti unpublish dev -p 3000`,
	Args:              cobra.ExactArgs(1),
	ValidArgsFunction: completeSandboxNames,
	Run: func(cmd *cobra.Command, args []string) {
		port, _ := cmd.Flags().GetInt("port")
		id, err := resolveID(args[0])
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			os.Exit(1)
		}
		resp, err := apiRequest("DELETE", fmt.Sprintf("/sandboxes/%s/publish/%d", id, port), nil)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			os.Exit(1)
		}
		resp.Body.Close()
		if resp.StatusCode >= 400 {
			fmt.Fprintf(os.Stderr, "Error: %s\n", resp.Status)
			os.Exit(1)
		}
		if !isJSON(cmd) {
			fmt.Printf("Unpublished port %d\n", port)
		}
	},
}

// --- completion ---

var completionCmd = &cobra.Command{
	Use:       "completion <bash|zsh|fish>",
	Short:     "Generate shell completion script",
	Args:      cobra.ExactArgs(1),
	ValidArgs: []string{"bash", "zsh", "fish"},
	RunE: func(cmd *cobra.Command, args []string) error {
		switch args[0] {
		case "bash":
			return rootCmd.GenBashCompletion(os.Stdout)
		case "zsh":
			return rootCmd.GenZshCompletion(os.Stdout)
		case "fish":
			return rootCmd.GenFishCompletion(os.Stdout, true)
		default:
			return cmd.Help()
		}
	},
}
