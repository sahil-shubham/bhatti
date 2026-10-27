package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/gorilla/websocket"
	"github.com/spf13/cobra"
	"golang.org/x/term"
)

// apiRequestWithAccept sends an HTTP request with a custom Accept header.
func apiRequestWithAccept(method, path string, body any, accept string) (*http.Response, error) {
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
	req.Header.Set("Accept", accept)
	if apiToken != "" {
		req.Header.Set("Authorization", "Bearer "+apiToken)
	}
	return httpClient().Do(req)
}

func init() {
	destroyCmd.Flags().BoolP("yes", "y", false, "Skip confirmation")
}

// --- exec ---

var execCmd = &cobra.Command{
	Use:   "exec <id|name> [--] <command...>",
	Short: "Execute a command in a sandbox",
	Long: `Execute a command inside a sandbox. Output streams as the command runs, to a
pipe or file as well as a terminal, with stdout and stderr kept separate. The
exit code is forwarded. Sleeping sandboxes wake automatically.

A command is stopped after 300 seconds unless --timeout allows longer (up to
86400). --json waits for the command and prints the whole result at the end.`,
	Example: `  bhatti exec dev -- echo hello
  bhatti exec dev echo hello           # -- is optional
  bhatti exec dev -- sudo apt-get install -y ripgrep
  bhatti exec dev --timeout 60 -- long-running-script.sh
  bhatti exec dev --sync -- write-important-data`,
	Args:              minimumArgs(1),
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
		detach, _ := cmd.Flags().GetBool("detach")
		syncFiles, _ := cmd.Flags().GetBool("sync")

		reqBody := map[string]any{"cmd": cmdArgs}
		if timeout > 0 {
			reqBody["timeout_sec"] = timeout
		}
		if detach {
			reqBody["detach"] = true
		}
		if syncFiles {
			reqBody["sync"] = true
		}

		// Stream unless the caller needs the whole result (--json) or only the PID
		// (--detach). TTY or not: scripts, agents and CI want output as it happens
		// as much as a person does, and stdout and stderr stay separate either way.
		if !isJSON(cmd) && !detach {
			exitCode, err := execStream(id, reqBody, os.Stdout, os.Stderr)
			if err != nil {
				return err
			}
			printTiming()
			os.Exit(exitCode)
			return nil
		}

		// Buffered JSON path (--json or --detach)
		var result struct {
			ExitCode   int    `json:"exit_code"`
			Stdout     string `json:"stdout"`
			Stderr     string `json:"stderr"`
			PID        int    `json:"pid"`
			OutputFile string `json:"output_file"`
			Detached   bool   `json:"detached"`
		}
		if err := apiJSON("POST", "/sandboxes/"+id+"/exec", reqBody, &result); err != nil {
			return err
		}

		if isJSON(cmd) {
			outputJSON(result)
		} else if result.Detached {
			fmt.Printf("pid: %d\noutput: %s\n", result.PID, result.OutputFile)
			return nil
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

// execStream runs a command over the streaming (NDJSON) exec endpoint, writing
// its output to stdout/stderr as it arrives, and returns the command's exit code.
// A failure the server reports mid-stream (timeout, agent error) is printed to
// stderr and returned as exit code 1. A stream that ends without an exit status
// is a failure too: the connection dropped, and the command's outcome is unknown.
func execStream(id string, reqBody map[string]any, stdout, stderr io.Writer) (int, error) {
	resp, err := apiRequestWithAccept("POST", "/sandboxes/"+id+"/exec", reqBody, "application/x-ndjson")
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	checkServerVersion(resp)
	if resp.StatusCode >= 400 {
		return 0, apiErrorFromResponse(resp)
	}

	dec := json.NewDecoder(resp.Body)
	for {
		var event struct {
			Type     string `json:"type"`
			Data     string `json:"data"`
			ExitCode *int   `json:"exit_code"`
		}
		if err := dec.Decode(&event); err != nil {
			fmt.Fprintln(stderr, "Error: exec stream ended without an exit status (connection lost?)")
			return 1, nil
		}
		switch event.Type {
		case "stdout":
			io.WriteString(stdout, event.Data)
		case "stderr":
			io.WriteString(stderr, event.Data)
		case "exit":
			if event.ExitCode != nil {
				return *event.ExitCode, nil
			}
			return 0, nil
		case "error":
			fmt.Fprintf(stderr, "Error: %s\n", event.Data)
			return 1, nil
		}
	}
}

func init() {
	execCmd.Flags().Int("timeout", 0, "Exec timeout in seconds (default: 300, max: 86400)")
	execCmd.Flags().Bool("detach", false, "Run in background, return PID immediately")
	execCmd.Flags().Bool("sync", false, "Sync guest filesystems before reporting command completion")
}

// --- shell ---

var shellCmd = &cobra.Command{
	Use:     "shell <id|name>",
	Aliases: []string{"sh"},
	Short:   "Open an interactive shell",
	Long: `Open an interactive terminal inside the sandbox. Ctrl+\ to detach —
the shell keeps running. Reconnect with 'bhatti shell' again.`,
	Example: `  bhatti shell dev
  bhatti sh dev        # alias`,
	Args:              exactArgs(1),
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
		conn, _, err := wsDialer().Dial(endpoint, header)
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
	Use:   "ps <id|name>",
	Short: "List sessions in a sandbox",
	Example: `  bhatti ps dev
  bhatti ps dev --json`,
	Args:              exactArgs(1),
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

// --- ports (B5) ---

var portsCmd = &cobra.Command{
	Use:   "ports <sandbox>",
	Short: "List listening ports in a sandbox",
	Example: `  bhatti ports dev
  bhatti ports dev --json`,
	Args:              exactArgs(1),
	ValidArgsFunction: completeSandboxNames,
	RunE: func(cmd *cobra.Command, args []string) error {
		id, err := resolveID(args[0])
		if err != nil {
			return err
		}
		var ports []struct {
			Port  int    `json:"container_port"`
			Proxy string `json:"proxy_url"`
		}
		if err := apiJSON("GET", "/sandboxes/"+id+"/ports", nil, &ports); err != nil {
			return err
		}
		if isJSON(cmd) {
			outputJSON(ports)
		} else {
			if len(ports) == 0 {
				fmt.Println("No listening ports detected.")
			} else {
				fmt.Printf("%-8s %s\n", "PORT", "URL")
				for _, p := range ports {
					// Prepend apiURL so the proxy path is a clickable URL
					fullURL := strings.TrimRight(apiURL, "/") + p.Proxy
					fmt.Printf("%-8d %s\n", p.Port, fullURL)
				}
			}
		}
		return nil
	},
}

// --- file ---
