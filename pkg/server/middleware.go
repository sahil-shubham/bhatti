package server

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"path"
	"runtime/debug"
	"strconv"
	"strings"
	"time"

	"github.com/sahil-shubham/bhatti/pkg/store"
)

// middleware wraps a handler with one cross-cutting concern.
type middleware func(http.Handler) http.Handler

// chain wraps h in mws. The first is outermost: it sees the request first and
// the response last.
func chain(h http.Handler, mws ...middleware) http.Handler {
	for i := len(mws) - 1; i >= 0; i-- {
		h = mws[i](h)
	}
	return h
}

// apiHandler is what every API request passes through, outermost first:
// request ID, panic recovery, auth, rate limit, request log, then the mux.
//
// The order is the contract. Auth sits before the router's answers, so a client
// without a key learns nothing about which routes exist: an unknown path or a
// wrong method is a 401 first. The rate limit comes after auth because its
// buckets are per user, and the request log is innermost so that it times the
// handler and records the status the handler chose. A refusal by auth or by
// the rate limit never gets that far; each is counted and logged where it
// happens.
//
// Two small stages sit among them: versionHeaders, first, stamps what CLIs read
// off any response, and matchRoute, after recovery, resolves the route that
// auth, the limiter and the log all key on.
func (s *Server) apiHandler() http.Handler {
	return chain(http.HandlerFunc(s.dispatch),
		s.versionHeaders,
		s.requestID,
		s.recoverPanics,
		s.matchRoute,
		s.authenticate,
		s.rateLimit,
		s.logRequests,
	)
}

// versionHeaders advertises the server version on every API response, errors
// included, so a CLI can tell an update is available (push, not pull).
func (s *Server) versionHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Bhatti-Version", ServerVersion)
		if MinCLIVersion != "" {
			w.Header().Set("X-Bhatti-Min-CLI", MinCLIVersion)
		}
		next.ServeHTTP(w, r)
	})
}

// requestID names the request: in the context for handlers and logs, and in
// X-Request-ID so a client can quote it. It is generated here, never taken from
// the request, so a caller can't forge the ID a log line is filed under.
// WebSocket upgraders must pass w.Header() explicitly: Gorilla builds the
// hijacked handshake from that argument, not the ResponseWriter's headers.
func (s *Server) requestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := generateRequestID()
		w.Header().Set("X-Request-ID", id)
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), requestIDKey, id)))
	})
}

func generateRequestID() string {
	b := make([]byte, 8)
	rand.Read(b)
	return "req_" + hex.EncodeToString(b)
}

// RequestIDFromContext extracts the request ID from context.
func RequestIDFromContext(ctx context.Context) string {
	id, _ := ctx.Value(requestIDKey).(string)
	return id
}

// recoverPanics answers a handler that panicked with a 500 naming the request,
// and logs the panic with its stack. Without it net/http drops the connection
// and the client learns nothing.
//
// http.ErrAbortHandler is not a failure: httputil.ReverseProxy panics with it to
// cut off a response it can't finish, and it is passed on for net/http to close
// the connection quietly. A panic after the response has begun is turned into
// the same abort: a status line can't be taken back, and returning normally
// would let net/http end the body as if it were complete.
func (s *Server) recoverPanics(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sw := trackWriter(w)
		defer func() {
			p := recover()
			if p == nil {
				return
			}
			if p == http.ErrAbortHandler {
				panic(p)
			}
			reqID := RequestIDFromContext(r.Context())
			slog.Error("request.panic", "request_id", reqID, "method", r.Method,
				"path", path.Clean(r.URL.Path), "route", routeLabel(r.Pattern),
				"panic", fmt.Sprint(p), "stack", string(debug.Stack()))
			if sw.wroteHeader {
				panic(http.ErrAbortHandler)
			}
			writeJSON(sw, http.StatusInternalServerError, map[string]string{
				"error":      "internal error",
				"request_id": reqID,
			})
		}()
		next.ServeHTTP(sw, r)
	})
}

// matchRoute records in r.Pattern which route will serve the request, before
// anything acts on it: auth needs to know whether the route is public, the
// limiter which bucket it spends, the log and histogram which route to file it
// under. Matching on the pattern rather than on the path is what keeps a
// sandbox named "ws-1" from being treated as a websocket.
//
// The mux sets r.Pattern again when it dispatches, to the same value. Some
// redirects it makes name a pattern that is not a registered route (in some Go
// versions the request's own path with a slash added); that counts as no match,
// which keeps what auth, the limiter, the log and the metrics see to the route
// table.
func (s *Server) matchRoute(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, pattern := s.mux.Handler(r)
		if _, registered := s.public[pattern]; !registered {
			pattern = ""
		}
		r.Pattern = pattern
		next.ServeHTTP(w, r)
	})
}

// authenticate resolves the bearer token to a user and puts it in the context.
// The token is read from the Authorization header only: no query-string auth,
// which would leak tokens into URLs and logs. A public route is passed through
// without looking at the token, valid or not.
func (s *Server) authenticate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.public[r.Pattern] {
			next.ServeHTTP(w, r)
			return
		}

		authHeader := r.Header.Get("Authorization")
		token := strings.TrimPrefix(authHeader, "Bearer ")
		if token == "" || token == authHeader {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "authorization required"})
			return
		}

		user, err := s.store.GetUserByKeyHash(r.Context(), sha256Hex(token))
		if err != nil {
			s.authFailures.Add(1)
			slog.Warn("auth.failed", "ip", r.RemoteAddr)
			s.RecordEvent(store.Event{
				Type: "auth.failed",
				Meta: map[string]any{"ip": r.RemoteAddr},
			})
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "invalid api key"})
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), userContextKey, user)))
	})
}

// rateLimit spends from the caller's bucket for the matched route. A refusal is
// logged like any request, naming the bucket so an operator can tell which
// budget to raise, and tells the client when a retry can succeed.
func (s *Server) rateLimit(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user := UserFromContext(r.Context())
		if user == nil { // a public route has no caller to budget
			next.ServeHTTP(w, r)
			return
		}
		ok, bucket, wait := s.limiter.Allow(user.ID, r)
		if ok {
			next.ServeHTTP(w, r)
			return
		}
		s.rateLimited.Add(1)
		secs := retryAfterSeconds(wait)
		lim := s.limiter.bucketLimits(bucket)
		slog.Info("request",
			"method", r.Method,
			"path", path.Clean(r.URL.Path),
			"status", http.StatusTooManyRequests,
			"user", user.Name,
			"user_id", user.ID,
			"rate_limit_bucket", bucket,
			"retry_after_s", secs,
		)
		w.Header().Set("Retry-After", strconv.Itoa(secs))
		errRespCode(w, http.StatusTooManyRequests, codeRateLimited, fmt.Sprintf(
			"rate limit exceeded: %s requests are limited to %g/min with a burst of %g (rate_limits.%s in the server config); retry in %ds",
			bucket, lim.PerMinute, lim.Burst, bucket, secs))
	})
}

// logRequests writes the `request` line, feeds the request counters and the
// duration histogram, once the handler has returned. A long-lived response
// (an events stream, an exec stream, a websocket) is therefore logged when it
// ends and its duration is the connection's life; a websocket is hijacked, so
// its status is 101.
//
// A handler that panics is logged as a 500: the recovery layer, which sits above
// this one, sends that response once the stack has unwound past here.
//
// Only authenticated requests are logged and counted. A public route has no user
// to attribute a line to, and /health is polled.
func (s *Server) logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user := UserFromContext(r.Context())
		if user == nil {
			next.ServeHTTP(w, r)
			return
		}
		sw := trackWriter(w)
		start := time.Now()
		returned := false
		defer func() {
			status := sw.status
			if !returned && !sw.wroteHeader {
				status = http.StatusInternalServerError
			}
			elapsed := time.Since(start)
			s.requestTotal.Add(1)
			if status >= 500 {
				s.requestErrors.Add(1)
			}
			s.reqMetrics.observe(r.Pattern, status, elapsed)
			slog.Info("request",
				"request_id", RequestIDFromContext(r.Context()),
				"method", r.Method,
				"path", path.Clean(r.URL.Path),
				"route", routeLabel(r.Pattern),
				"status", status,
				"duration_ms", elapsed.Milliseconds(),
				"user", user.Name,
				"user_id", user.ID,
			)
		}()
		next.ServeHTTP(sw, r)
		returned = true
	})
}

// dispatch hands the request to the route that matched it.
func (s *Server) dispatch(w http.ResponseWriter, r *http.Request) {
	if r.Pattern == "" {
		w = &routeErrorWriter{ResponseWriter: w}
	}
	s.mux.ServeHTTP(w, r)
}

// routeErrorWriter answers a request no route matched. The mux's own 404 and 405
// (which already carries the Allow header, worked out from the table) are plain
// text; here they become the JSON every other error from this API is, with the
// same bodies the handlers use. Any other response from the mux, a redirect to
// the cleaned path, passes through.
type routeErrorWriter struct {
	http.ResponseWriter
	replaced bool
}

func (w *routeErrorWriter) WriteHeader(code int) {
	switch code {
	case http.StatusNotFound:
		w.replace(code, "not found")
	case http.StatusMethodNotAllowed:
		w.replace(code, "method not allowed")
	default:
		w.ResponseWriter.WriteHeader(code)
	}
}

func (w *routeErrorWriter) replace(code int, msg string) {
	w.Header().Del("X-Content-Type-Options")
	w.replaced = true
	errResp(w.ResponseWriter, code, msg)
}

// Write drops the mux's plain-text body once it has been replaced.
func (w *routeErrorWriter) Write(b []byte) (int, error) {
	if w.replaced {
		return len(b), nil
	}
	return w.ResponseWriter.Write(b)
}

func (w *routeErrorWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// statusWriter records the status the client was sent and whether the response
// has begun, and keeps the Flusher and Hijacker a streaming handler or a
// websocket upgrade type-asserts for.
type statusWriter struct {
	http.ResponseWriter
	status      int  // what the client got; 200 until a header is written
	wroteHeader bool // a header, a body byte or a flush has gone out, or the connection was hijacked
}

// trackWriter returns w as a statusWriter, wrapping it only once: layers that
// each want the status share one record.
func trackWriter(w http.ResponseWriter) *statusWriter {
	if sw, ok := w.(*statusWriter); ok {
		return sw
	}
	return &statusWriter{ResponseWriter: w, status: http.StatusOK}
}

func (w *statusWriter) WriteHeader(code int) {
	// A 1xx other than 101 is interim: the final status is still to come.
	if code >= 100 && code < 200 && code != http.StatusSwitchingProtocols {
		w.ResponseWriter.WriteHeader(code)
		return
	}
	if !w.wroteHeader {
		w.status = code
		w.wroteHeader = true
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Write(b []byte) (int, error) {
	w.wroteHeader = true
	return w.ResponseWriter.Write(b)
}

func (w *statusWriter) Flush() {
	w.wroteHeader = true
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Hijack hands the connection over, as a websocket upgrade needs. What happens
// on it is no longer an HTTP response, so the request is recorded as the 101
// every upgrade here ends in.
func (w *statusWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	h, ok := w.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, fmt.Errorf("hijack: %w", http.ErrNotSupported)
	}
	conn, rw, err := h.Hijack()
	if err == nil {
		if !w.wroteHeader {
			w.status = http.StatusSwitchingProtocols
		}
		w.wroteHeader = true
	}
	return conn, rw, err
}

// Unwrap lets http.ResponseController reach the connection's deadlines.
func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }
