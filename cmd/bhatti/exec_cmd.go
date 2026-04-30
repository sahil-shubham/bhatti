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
	Long: `Execute a command inside a sandbox. The exit code is forwarded.
Sleeping sandboxes wake automatically.`,
	Example: `  bhatti exec dev -- echo hello
  bhatti exec dev echo hello           # -- is optional
  bhatti exec dev -- sudo apt-get install -y ripgrep
  bhatti exec dev --timeout 60 -- long-running-script.sh`,
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

		reqBody := map[string]any{"cmd": cmdArgs}
		if timeout > 0 {
			reqBody["timeout_sec"] = timeout
		}
		if detach {
			reqBody["detach"] = true
		}

		// B2: stream when stdout is a TTY (or BHATTI_FORCE_STREAM=1)
		stream := (term.IsTerminal(int(os.Stdout.Fd())) || os.Getenv("BHATTI_FORCE_STREAM") == "1") &&
			!isJSON(cmd) && !detach

		if stream {
			// Streaming NDJSON path
			resp, err := apiRequestWithAccept("POST", "/sandboxes/"+id+"/exec", reqBody, "application/x-ndjson")
			if err != nil {
				return err
			}
			defer resp.Body.Close()
			checkServerVersion(resp)
			if resp.StatusCode >= 400 {
				var errBody struct{ Error string `json:"error"` }
				json.NewDecoder(resp.Body).Decode(&errBody)
				return &apiError{status: resp.Status, message: errBody.Error}
			}

			exitCode := 0
			dec := json.NewDecoder(resp.Body)
			for dec.More() {
				var event struct {
					Type     string `json:"type"`
					Data     string `json:"data"`
					ExitCode *int   `json:"exit_code"`
				}
				if err := dec.Decode(&event); err != nil {
					break
				}
				switch event.Type {
				case "stdout":
					os.Stdout.WriteString(event.Data)
				case "stderr":
					os.Stderr.WriteString(event.Data)
				case "exit":
					if event.ExitCode != nil {
						exitCode = *event.ExitCode
					}
				case "error":
					fmt.Fprintf(os.Stderr, "exec error: %s\n", event.Data)
					exitCode = 1
				}
			}
			printTiming()
			os.Exit(exitCode)
			return nil
		}

		// Buffered JSON path (piped, --json, or --detach)
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

func init() {
	execCmd.Flags().Int("timeout", 0, "Exec timeout in seconds (default: 300, max: 3600)")
	execCmd.Flags().Bool("detach", false, "Run in background, return PID immediately")
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
				fmt.Printf("%-8s %s\n", "PORT", "PROXY")
				for _, p := range ports {
					fmt.Printf("%-8d %s\n", p.Port, p.Proxy)
				}
			}
		}
		return nil
	},
}

// --- file ---
