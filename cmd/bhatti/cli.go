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
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
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
	Use:          "bhatti",
	Short:        "Firecracker microVM orchestrator",
	SilenceUsage: true,
	PersistentPreRun: func(cmd *cobra.Command, args []string) {
		loadConfig(cmd)
	},
}

func init() {
	rootCmd.PersistentFlags().String("url", "", "API endpoint (overrides config)")
	rootCmd.PersistentFlags().String("token", "", "API key (overrides config)")
	rootCmd.PersistentFlags().Bool("json", false, "Output as JSON")
	rootCmd.PersistentFlags().Bool("timing", false, "Show request timing breakdown")

	rootCmd.AddCommand(createCmd)
	rootCmd.AddCommand(listCmd)
	rootCmd.AddCommand(destroyCmd)
	rootCmd.AddCommand(execCmd)
	rootCmd.AddCommand(shellCmd)
	rootCmd.AddCommand(psCmd)
	rootCmd.AddCommand(fileCmd)
	rootCmd.AddCommand(secretCmd)
	rootCmd.AddCommand(volumeCmd)
	rootCmd.AddCommand(imageCmd)
	rootCmd.AddCommand(snapshotCmd)
	rootCmd.AddCommand(userCmd)
	rootCmd.AddCommand(setupCmd)
	rootCmd.AddCommand(versionCmd)
	rootCmd.AddCommand(completionCmd)
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

// completeSandboxNames reads sandbox names from a local cache file.
// The cache is written by `bhatti list` on every successful call.
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
		volFlags, _ := cmd.Flags().GetStringSlice("volume")

		envMap := parseEnvFlag(env)
		req := map[string]any{
			"name": name, "cpus": cpus, "memory_mb": memory,
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
	createCmd.Flags().Int("memory", 512, "Memory in MB")
	createCmd.Flags().Int("disk-size", 0, "Rootfs disk size in MB (0 = use image size)")
	createCmd.Flags().String("env", "", "Environment variables (K=V,K=V)")
	createCmd.Flags().String("init", "", "Init script")
	createCmd.Flags().StringSlice("volume", nil, "Persistent volume (name:mount[:ro])")
}

// --- list ---

var listCmd = &cobra.Command{
	Use:     "list",
	Aliases: []string{"ls"},
	Short:   "List sandboxes",
	RunE: func(cmd *cobra.Command, args []string) error {
		setupTiming(cmd)
		defer printTiming()

		var sandboxes []struct {
			ID     string `json:"id"`
			Name   string `json:"name"`
			Status string `json:"status"`
			IP     string `json:"ip"`
		}
		if err := apiJSON("GET", "/sandboxes", nil, &sandboxes); err != nil {
			return err
		}

		// Update completion cache (best-effort, never errors)
		var names []string
		for _, sb := range sandboxes {
			names = append(names, sb.Name)
		}
		path := completionCachePath()
		os.WriteFile(path, []byte(strings.Join(names, "\n")), 0600)

		if isJSON(cmd) {
			outputJSON(sandboxes)
		} else {
			fmt.Printf("%-20s %-20s %-10s %-16s\n", "ID", "NAME", "STATUS", "IP")
			for _, sb := range sandboxes {
				fmt.Printf("%-20s %-20s %-10s %-16s\n", sb.ID, sb.Name, sb.Status, sb.IP)
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
	Args:              cobra.ExactArgs(1),
	ValidArgsFunction: completeSandboxNames,
	RunE: func(cmd *cobra.Command, args []string) error {
		setupTiming(cmd)
		defer printTiming()

		id, err := resolveID(args[0])
		if err != nil {
			return err
		}
		if err := apiJSON("DELETE", "/sandboxes/"+id, nil, nil); err != nil {
			return err
		}
		if isJSON(cmd) {
			outputJSON(map[string]string{"status": "destroyed"})
		} else {
			fmt.Println("destroyed")
		}
		return nil
	},
}

// --- exec ---

var execCmd = &cobra.Command{
	Use:               "exec <id|name> -- CMD...",
	Short:             "Execute a command in a sandbox",
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
	Args:              cobra.ExactArgs(1),
	ValidArgsFunction: completeSandboxNames,
	RunE: func(cmd *cobra.Command, args []string) error {
		id, err := resolveID(args[0])
		if err != nil {
			return err
		}

		wsURL := strings.Replace(apiURL, "http://", "ws://", 1)
		wsURL = strings.Replace(wsURL, "https://", "wss://", 1)
		header := http.Header{}
		if apiToken != "" {
			header.Set("Authorization", "Bearer "+apiToken)
		}
		conn, _, err := websocket.DefaultDialer.Dial(
			wsURL+"/sandboxes/"+id+"/ws", header)
		if err != nil {
			return err
		}
		defer conn.Close()

		// Raw terminal mode
		oldState, err := term.MakeRaw(int(os.Stdin.Fd()))
		if err != nil {
			return err
		}
		defer term.Restore(int(os.Stdin.Fd()), oldState)

		// Initial size
		w, h, _ := term.GetSize(int(os.Stdin.Fd()))
		conn.WriteJSON(map[string]any{"type": "resize", "rows": h, "cols": w})

		// SIGWINCH → resize
		sigwinch := make(chan os.Signal, 1)
		signal.Notify(sigwinch, syscall.SIGWINCH)
		go func() {
			for range sigwinch {
				w, h, _ := term.GetSize(int(os.Stdin.Fd()))
				conn.WriteJSON(map[string]any{
					"type": "resize", "rows": h, "cols": w,
				})
			}
		}()

		// WebSocket → stdout
		done := make(chan struct{})
		go func() {
			defer close(done)
			for {
				_, msg, err := conn.ReadMessage()
				if err != nil {
					return
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
						term.Restore(int(os.Stdin.Fd()), oldState)
						fmt.Fprintf(os.Stderr, "\r\ndetached\r\n")
						conn.Close()
						return
					}
				}
				conn.WriteMessage(websocket.BinaryMessage, buf[:n])
			}
		}()

		<-done
		return nil
	},
}

// --- ps ---

var psCmd = &cobra.Command{
	Use:               "ps <id|name>",
	Short:             "List sessions in a sandbox",
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
	Short: "File operations",
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
	Short: "Manage secrets",
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
	Short: "Manage users (local, requires DB access)",
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
	userCmd.AddCommand(userDeleteCmd)
	userCmd.AddCommand(userRotateKeyCmd)
}

// openLocalStore opens the SQLite store from the local config.
// Used by user commands which operate directly on the DB.
func openLocalStore() *store.Store {
	cfg, err := pkg.LoadConfig()
	if err != nil {
		fmt.Fprintf(os.Stderr, "load config: %v\n", err)
		os.Exit(1)
	}
	st, err := store.New(filepath.Join(cfg.DataDir, "state.db"))
	if err != nil {
		fmt.Fprintf(os.Stderr, "open store: %v\n", err)
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
	Short: "Configure CLI (endpoint + API key)",
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

		// Test connection
		fmt.Print("Testing connection... ")
		apiURL = endpoint
		apiToken = key
		var health map[string]any
		if err := apiJSON("GET", "/health", nil, &health); err != nil {
			fmt.Printf("✗ %v\n", err)
			return nil
		}
		fmt.Printf("✓ connected (sandboxes: %v, uptime: %v)\n",
			health["sandboxes"], health["uptime"])
		return nil
	},
}

// --- volume ---

var volumeCmd = &cobra.Command{
	Use:   "volume <create|list|delete|resize>",
	Short: "Manage persistent volumes",
}

var volumeCreateCmd = &cobra.Command{
	Use:   "create",
	Short: "Create a persistent volume",
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
	volumeCmd.AddCommand(volumeDeleteCmd)
	volumeCmd.AddCommand(volumeResizeCmd)
}

// --- image ---

var imageCmd = &cobra.Command{
	Use:   "image <list|delete>",
	Short: "Manage rootfs images",
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

		if err := apiJSON("DELETE", "/images/"+args[0], nil, nil); err != nil {
			return err
		}
		fmt.Println("deleted")
		return nil
	},
}

var imagePullCmd = &cobra.Command{
	Use:   "pull <ref>",
	Short: "Pull an OCI/Docker image (async)",
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

func init() {
	imagePullCmd.Flags().String("name", "", "Image name (default: derived from ref)")
	imagePullCmd.Flags().String("auth", "", "Registry auth (user:token)")
	imageSaveCmd.Flags().String("name", "", "Image name (required)")

	imageCmd.AddCommand(imageListCmd)
	imageCmd.AddCommand(imageDeleteCmd)
	imageCmd.AddCommand(imagePullCmd)
	imageCmd.AddCommand(imageSaveCmd)
}

// --- snapshot ---

var snapshotCmd = &cobra.Command{
	Use:   "snapshot <create|list|resume|delete>",
	Short: "Manage named VM snapshots",
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
	snapshotCmd.AddCommand(snapshotDeleteCmd)
}

// --- version ---

var versionCmd = &cobra.Command{
	Use:   "version",
	Short: "Print version and API endpoint",
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
