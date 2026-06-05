package server

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/sahil-shubham/bhatti/pkg/engine"
)

type portInfo struct {
	SandboxID     string `json:"sandbox_id,omitempty"`
	ContainerPort int    `json:"container_port"`
	ProxyURL      string `json:"proxy_url"`
}

func (s *Server) handleSandboxPorts(w http.ResponseWriter, r *http.Request, id string) {
	if r.Method != http.MethodGet {
		errResp(w, 405, "method not allowed")
		return
	}
	sb := s.getUserSandbox(w, r, id)
	if sb == nil {
		return
	}

	if err := s.ensureHot(r.Context(), sb.EngineID); err != nil {
		errResp(w, 500, "wake sandbox: "+err.Error())
		return
	}
	ports, err := s.engine.ListeningPorts(r.Context(), sb.EngineID)
	if err != nil {
		ports = []int{}
	}

	out := make([]portInfo, 0, len(ports))
	for _, p := range ports {
		out = append(out, portInfo{
			ContainerPort: p,
			ProxyURL:      fmt.Sprintf("/sandboxes/%s/proxy/%d/", id, p),
		})
	}
	writeJSON(w, 200, out)
}

func (s *Server) handleAllPorts(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		errResp(w, 405, "method not allowed")
		return
	}
	user := UserFromContext(r.Context())
	sandboxes, err := s.store.ListSandboxes(user.ID)
	if err != nil {
		errRespInternal(w, r, "list sandboxes failed", err)
		return
	}

	var out []portInfo
	for _, sb := range sandboxes {
		if sb.Status != "running" {
			continue
		}
		ports, err := s.engine.ListeningPorts(context.Background(), sb.EngineID)
		if err != nil {
			continue
		}
		for _, p := range ports {
			out = append(out, portInfo{
				SandboxID:     sb.ID,
				ContainerPort: p,
				ProxyURL:      fmt.Sprintf("/sandboxes/%s/proxy/%d/", sb.ID, p),
			})
		}
	}
	if out == nil {
		out = []portInfo{}
	}
	writeJSON(w, 200, out)
}

func (s *Server) handleSandboxProxyRoute(w http.ResponseWriter, r *http.Request, sandboxID, portPath string) {
	// Split "4321/some/path" → port=4321, rest="/some/path"
	portStr := portPath
	rest := "/"
	if idx := strings.IndexByte(portPath, '/'); idx >= 0 {
		portStr = portPath[:idx]
		rest = portPath[idx:]
	}
	port, err := strconv.Atoi(portStr)
	if err != nil || port < 1 || port > 65535 {
		errResp(w, 400, "invalid port")
		return
	}

	sb := s.getUserSandbox(w, r, sandboxID)
	if sb == nil {
		return
	}

	if err := s.ensureHot(r.Context(), sb.EngineID); err != nil {
		errResp(w, 500, "wake sandbox: "+err.Error())
		return
	}

	// WebSocket upgrade → tunnel raw bytes
	if websocket.IsWebSocketUpgrade(r) {
		s.handleProxyWS(w, r, sb.EngineID, port, rest)
		return
	}

	// Regular HTTP → tunnel through exec
	s.handleProxyHTTP(w, r, sb.EngineID, port, rest)
}

// --- Tunnel Transport ---

// tunnelTransport wraps Engine.Tunnel() as an http.RoundTripper.
// Each RoundTrip opens a new tunnel connection to the sandbox.
// Used by httputil.ReverseProxy for proper streaming, hop-by-hop
// header removal, and response flushing.
type tunnelTransport struct {
	engine   engine.Engine
	engineID string
	port     int
}

func (t *tunnelTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	tunnel, err := t.engine.Tunnel(req.Context(), t.engineID, t.port)
	if err != nil {
		return nil, err
	}

	// Guard against context cancellation leaking the tunnel FD.
	// If the client disconnects mid-response, ReverseProxy may cancel
	// the context without closing resp.Body. This AfterFunc ensures
	// the tunnel is always cleaned up.
	stop := context.AfterFunc(req.Context(), func() {
		tunnel.Close()
	})

	if err := req.Write(tunnel); err != nil {
		stop()
		tunnel.Close()
		return nil, err
	}
	resp, err := http.ReadResponse(bufio.NewReader(tunnel), req)
	if err != nil {
		stop()
		tunnel.Close()
		return nil, err
	}
	// Closing the body also closes the tunnel and cancels the AfterFunc.
	resp.Body = &tunnelBody{ReadCloser: resp.Body, tunnel: tunnel, stopGuard: stop}
	return resp, nil
}

// tunnelBody wraps the response body to close the tunnel when done.
// Close is idempotent via sync.Once — safe if called by both
// ReverseProxy and the context.AfterFunc guard.
type tunnelBody struct {
	io.ReadCloser
	tunnel    io.Closer
	stopGuard func() bool
	once      sync.Once
}

func (tb *tunnelBody) Close() error {
	var err error
	tb.once.Do(func() {
		tb.stopGuard()
		tb.ReadCloser.Close()
		err = tb.tunnel.Close()
	})
	return err
}

// handleProxyHTTP tunnels an HTTP request/response through Engine.Tunnel()
// using httputil.ReverseProxy. This handles hop-by-hop header removal,
// chunked transfer encoding, trailer forwarding, and response flushing
// (FlushInterval: -1 flushes every chunk — required for SSE/streaming).
func (s *Server) handleProxyHTTP(w http.ResponseWriter, r *http.Request, engineID string, port int, path string) {
	proxy := &httputil.ReverseProxy{
		Transport: &tunnelTransport{
			engine:   s.engine,
			engineID: engineID,
			port:     port,
		},
		Director: func(req *http.Request) {
			req.URL.Scheme = "http"
			req.URL.Host = fmt.Sprintf("localhost:%d", port)
			req.URL.Path = path
			req.URL.RawQuery = r.URL.RawQuery
			req.Host = fmt.Sprintf("localhost:%d", port)
			if clientIP, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
				req.Header.Set("X-Forwarded-For", clientIP)
			}
			req.Header.Set("X-Forwarded-Proto", schemeOf(r))
			req.Header.Set("X-Forwarded-Host", r.Host)
		},
		FlushInterval: -1, // flush immediately (streaming/SSE)
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			errResp(w, 502, "bad gateway: "+err.Error())
		},
	}
	proxy.ServeHTTP(w, r)
}

// schemeOf returns "https" if the request came over TLS, else "http".
func schemeOf(r *http.Request) string {
	if r.TLS != nil {
		return "https"
	}
	return "http"
}

// --- WebSocket Proxy ---

const wsIdleTimeout = 10 * time.Minute

// deadlineConn is satisfied by net.Conn (which clientConn always is)
// and by tunnel implementations that wrap net.Conn.
type deadlineConn interface {
	io.Reader
	SetReadDeadline(t time.Time) error
}

// idleCopyWithDeadline copies src → dst, resetting the read deadline
// on src after every successful read. Returns when src hits the idle
// timeout or either side errors/closes.
func idleCopyWithDeadline(dst io.Writer, src deadlineConn, timeout time.Duration) {
	buf := make([]byte, 32*1024)
	for {
		src.SetReadDeadline(time.Now().Add(timeout))
		n, err := src.Read(buf)
		if n > 0 {
			if _, wErr := dst.Write(buf[:n]); wErr != nil {
				return
			}
		}
		if err != nil {
			return
		}
	}
}

// relayBidirectional copies bytes in both directions between a and b
// until either direction ends (close, error, or idle timeout). When
// one direction returns, the function closes the OTHER side's source
// so the still-running direction's blocking Read errors out and its
// goroutine exits.
//
// Without that explicit close, plain io.Copy between two long-lived
// half-open-tolerant peers (e.g. one side EOFs but the other keeps
// its Read blocked because nobody closed its source) leaks the
// goroutine for the blocked direction — the deferred Close in the
// caller doesn't fire until the function returns, and the function
// can't return until the blocked direction unblocks. Tranche 0a #1.
//
// If a side implements deadlineConn it uses idle-timeout detection;
// otherwise plain io.Copy. The close-the-other-side mechanism is
// the teardown path either way.
func relayBidirectional(a, b io.ReadWriteCloser, idle time.Duration) {
	done := make(chan struct{})

	// Direction 1 (background goroutine): a → b.
	go func() {
		if dc, ok := a.(deadlineConn); ok {
			idleCopyWithDeadline(b, dc, idle)
		} else {
			io.Copy(b, a)
		}
		// a's read ended — wake the other direction by closing b
		// so its Read on b returns.
		b.Close()
		close(done)
	}()

	// Direction 2 (foreground): b → a.
	if dc, ok := b.(deadlineConn); ok {
		idleCopyWithDeadline(a, dc, idle)
	} else {
		io.Copy(a, b)
	}
	// b's read ended — wake the background goroutine.
	a.Close()

	<-done
}

// proxyWebSocket hijacks the client connection and relays WS frames
// through an engine tunnel. Used by both the authenticated proxy and
// (in the future) the public proxy. Includes an idle timeout to prevent
// FD exhaustion from abandoned connections.
func proxyWebSocket(w http.ResponseWriter, r *http.Request, eng engine.Engine, engineID string, port int, path string, onActivity func()) {
	tunnel, err := eng.Tunnel(r.Context(), engineID, port)
	if err != nil {
		errResp(w, 502, "tunnel failed: "+err.Error())
		return
	}
	defer tunnel.Close()

	hj, ok := w.(http.Hijacker)
	if !ok {
		errResp(w, 500, "server doesn't support hijacking")
		return
	}
	clientConn, clientBuf, err := hj.Hijack()
	if err != nil {
		errResp(w, 500, "hijack failed")
		return
	}
	defer clientConn.Close()

	outReq := r.Clone(r.Context())
	outReq.URL.Scheme = "http"
	outReq.URL.Host = fmt.Sprintf("localhost:%d", port)
	outReq.URL.Path = path
	outReq.URL.RawQuery = r.URL.RawQuery
	outReq.RequestURI = ""
	outReq.Host = fmt.Sprintf("localhost:%d", port)
	outReq.Write(tunnel)

	resp, err := http.ReadResponse(bufio.NewReader(tunnel), outReq)
	if err != nil {
		return
	}
	resp.Write(clientBuf)
	clientBuf.Flush()

	// Keep the sandbox marked as active while the relay runs.
	// Without this, long-lived WebSocket connections (CDP, HMR, etc.)
	// are invisible to the thermal manager after the initial ensureHot.
	var stopActivity func()
	if onActivity != nil {
		ctx, cancel := context.WithCancel(context.Background())
		stopActivity = cancel
		go func() {
			tick := time.NewTicker(10 * time.Second)
			defer tick.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-tick.C:
					onActivity()
				}
			}
		}()
	}

	// Bidirectional relay with idle timeout and leak-free teardown.
	// See relayBidirectional's docs for the close-the-other-side
	// teardown pattern. The deferred clientConn.Close() and
	// tunnel.Close() at function entry still fire afterwards;
	// net.Conn.Close is safe to call multiple times.
	relayBidirectional(clientConn, tunnel, wsIdleTimeout)

	if stopActivity != nil {
		stopActivity()
	}
}

// handleProxyWS delegates to the shared proxyWebSocket function.
func (s *Server) handleProxyWS(w http.ResponseWriter, r *http.Request, engineID string, port int, path string) {
	proxyWebSocket(w, r, s.engine, engineID, port, path, func() {
		s.touchActivity(engineID)
	})
}

// --- File Operations ---

// FileEngine is optionally implemented by engines that support direct file operations.
