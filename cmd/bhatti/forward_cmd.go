package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/gorilla/websocket"
	"github.com/sahil-shubham/bhatti/pkg/forward"
	"github.com/spf13/cobra"
)

var forwardCmd = &cobra.Command{
	Use:   "forward <sandbox> <port-spec>...",
	Short: "Forward local TCP ports to ports inside a sandbox",
	Long: `Bind ports on this machine and forward each TCP connection to the sandbox
over the authenticated API. PORT maps the same local and guest port;
LOCAL:GUEST maps different ports (LOCAL may be 0 for an ephemeral port).
All local ports must be available before any forward starts.`,
	Example: `  bhatti forward spc-dev 5174 3000:3000   # Vite + API on this machine
  bhatti forward spc-dev 0:5432            # pick a free local port
  bhatti forward spc-dev 8080 --address 0.0.0.0`,
	Args:              minimumArgs(2),
	ValidArgsFunction: completeSandboxNames,
	RunE: func(cmd *cobra.Command, args []string) error {
		specs := make([]portSpec, 0, len(args)-1)
		for _, arg := range args[1:] {
			spec, err := parsePortSpec(arg)
			if err != nil {
				return err
			}
			specs = append(specs, spec)
		}
		id, err := resolveID(args[0])
		if err != nil {
			return err
		}
		address, _ := cmd.Flags().GetString("address")
		ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		return runForward(ctx, id, specs, address, isJSON(cmd), os.Stdout, os.Stderr)
	},
}

func init() {
	forwardCmd.Flags().String("address", "127.0.0.1", "Local address to listen on (0.0.0.0 exposes the ports)")
}

type portSpec struct {
	local int
	guest int
}

func parsePortSpec(spec string) (portSpec, error) {
	parts := strings.Split(spec, ":")
	if len(parts) < 1 || len(parts) > 2 {
		return portSpec{}, fmt.Errorf("invalid port spec %q: expected PORT or LOCAL:GUEST", spec)
	}
	parsePort := func(value string, allowZero bool) (int, error) {
		if value == "" {
			return 0, fmt.Errorf("empty port")
		}
		for _, ch := range value {
			if ch < '0' || ch > '9' {
				return 0, fmt.Errorf("non-numeric port %q", value)
			}
		}
		port, err := strconv.ParseUint(value, 10, 16)
		if err != nil || (!allowZero && port == 0) {
			return 0, fmt.Errorf("port %q out of range (1..65535)", value)
		}
		return int(port), nil
	}
	local, err := parsePort(parts[0], len(parts) == 2)
	if err != nil {
		return portSpec{}, fmt.Errorf("invalid port spec %q: %w", spec, err)
	}
	if len(parts) == 1 {
		return portSpec{local: local, guest: local}, nil
	}
	guest, err := parsePort(parts[1], false)
	if err != nil {
		return portSpec{}, fmt.Errorf("invalid port spec %q: %w", spec, err)
	}
	return portSpec{local: local, guest: guest}, nil
}

type boundForward struct {
	listener net.Listener
	endpoint string
	guest    int
	local    int
}

type forwardMapping struct {
	Sandbox   string `json:"sandbox"`
	LocalAddr string `json:"local_addr"`
	LocalPort int    `json:"local_port"`
	GuestPort int    `json:"guest_port"`
}

// forwardSession owns all connections so Ctrl-C can interrupt reads and writes
// immediately, rather than waiting for idle TCP clients or WebSocket dials.
type forwardSession struct {
	mu         sync.Mutex
	active     map[net.Conn]*forward.WSConn
	closed     bool
	lastError  time.Time
	suppressed int
	stderr     io.Writer
}

func (s *forwardSession) register(conn net.Conn) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return false
	}
	s.active[conn] = nil
	return true
}

func (s *forwardSession) setRemote(conn net.Conn, remote *forward.WSConn) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return false
	}
	s.active[conn] = remote
	return true
}

func (s *forwardSession) release(conn net.Conn) {
	s.mu.Lock()
	delete(s.active, conn)
	s.mu.Unlock()
}

func (s *forwardSession) closeAll() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
	for local, remote := range s.active {
		local.Close()
		if remote != nil {
			remote.Abort()
		}
	}
}

func (s *forwardSession) report(err error) {
	if err == nil {
		return
	}
	var closeErr *websocket.CloseError
	if errors.As(err, &closeErr) {
		if closeErr.Code == websocket.CloseNormalClosure || closeErr.Code == websocket.CloseGoingAway {
			return
		}
		if closeErr.Text != "" {
			err = errors.New(closeErr.Text)
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	if time.Since(s.lastError) < 5*time.Second {
		s.suppressed++
		return
	}
	if s.suppressed > 0 {
		fmt.Fprintf(s.stderr, "forward: %d connection errors suppressed\n", s.suppressed)
		s.suppressed = 0
	}
	fmt.Fprintf(s.stderr, "forward: %v\n", err)
	s.lastError = time.Now()
}

func tunnelEndpoint(sandbox string, guestPort int) (string, error) {
	base, err := url.Parse(apiURL)
	if err != nil {
		return "", err
	}
	switch base.Scheme {
	case "http":
		base.Scheme = "ws"
	case "https":
		base.Scheme = "wss"
	default:
		return "", fmt.Errorf("invalid API URL scheme %q (expected http or https)", base.Scheme)
	}
	base.Path = strings.TrimRight(base.Path, "/") + "/sandboxes/" + url.PathEscape(sandbox) + "/tunnel"
	base.RawQuery = url.Values{"port": {strconv.Itoa(guestPort)}}.Encode()
	return base.String(), nil
}

func runForward(ctx context.Context, sandbox string, specs []portSpec, address string, jsonOutput bool, stdout, stderr io.Writer) error {
	bounds := make([]boundForward, 0, len(specs))
	defer func() {
		for _, b := range bounds {
			b.listener.Close()
		}
	}()
	for _, spec := range specs {
		endpoint, err := tunnelEndpoint(sandbox, spec.guest)
		if err != nil {
			return err
		}
		addr := net.JoinHostPort(address, strconv.Itoa(spec.local))
		listener, err := net.Listen("tcp", addr)
		if err != nil {
			return fmt.Errorf("cannot bind local %s for guest port %d: %w", addr, spec.guest, err)
		}
		bounds = append(bounds, boundForward{
			listener: listener, endpoint: endpoint, guest: spec.guest,
			local: listener.Addr().(*net.TCPAddr).Port,
		})
	}

	mappings := make([]forwardMapping, 0, len(bounds))
	for _, b := range bounds {
		mappings = append(mappings, forwardMapping{
			Sandbox: sandbox, LocalAddr: b.listener.Addr().String(),
			LocalPort: b.local, GuestPort: b.guest,
		})
	}
	if jsonOutput {
		if err := json.NewEncoder(stdout).Encode(mappings); err != nil {
			return err
		}
	} else {
		for _, mapping := range mappings {
			fmt.Fprintf(stdout, "Forwarding %s -> %s:%d\n", mapping.LocalAddr, sandbox, mapping.GuestPort)
		}
	}

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	session := &forwardSession{active: make(map[net.Conn]*forward.WSConn), stderr: stderr}
	dialer := wsDialer()
	header := http.Header{}
	if apiToken != "" {
		header.Set("Authorization", "Bearer "+apiToken)
	}
	var wg sync.WaitGroup
	acceptErrors := make(chan error, len(bounds))
	for _, bound := range bounds {
		wg.Add(1)
		go func(b boundForward) {
			defer wg.Done()
			for {
				local, err := b.listener.Accept()
				if err != nil {
					if ctx.Err() == nil {
						acceptErrors <- fmt.Errorf("accept on %s: %w", b.listener.Addr(), err)
					}
					return
				}
				if !session.register(local) {
					local.Close()
					return
				}
				wg.Add(1)
				go func() {
					defer wg.Done()
					defer session.release(local)
					defer local.Close()
					ws, resp, err := dialer.DialContext(ctx, b.endpoint, header)
					if err != nil {
						session.report(tunnelDialError(err, resp))
						return
					}
					remote := forward.NewWSConn(ws)
					if !session.setRemote(local, remote) {
						remote.Close()
						return
					}
					defer remote.Close()
					session.report(forward.Relay(local, remote))
				}()
			}
		}(bound)
	}

	var runErr error
	select {
	case <-ctx.Done():
	case runErr = <-acceptErrors:
	}
	cancel()
	for _, b := range bounds {
		b.listener.Close()
	}
	session.closeAll()
	wg.Wait()
	return runErr
}

func tunnelDialError(err error, resp *http.Response) error {
	if resp == nil {
		return err
	}
	defer resp.Body.Close()
	var body struct {
		Error string `json:"error"`
	}
	if json.NewDecoder(io.LimitReader(resp.Body, 2048)).Decode(&body) == nil && body.Error != "" {
		return fmt.Errorf("%s: %s", resp.Status, body.Error)
	}
	return fmt.Errorf("%s: %w", resp.Status, err)
}
