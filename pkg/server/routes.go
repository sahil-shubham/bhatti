package server

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/http"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/sahil-shubham/bhatti/pkg/engine"
	"github.com/sahil-shubham/bhatti/pkg/store"
)

// route is one entry of the API's route table.
type route struct {
	// pattern is a ServeMux pattern: "METHOD /path/{wildcard}". The method is
	// part of it, so another method on the same path is answered 405 by the
	// router and never reaches a handler. A pattern without a method answers
	// every method itself.
	pattern string
	handler http.HandlerFunc
	// public routes are served without a bearer token, and so without a user
	// to rate limit or attribute a request line to. Everything else needs a key.
	public bool
}

// api is a route that needs a bearer token; open is one that does not.
func api(pattern string, h http.HandlerFunc) route  { return route{pattern, h, false} }
func open(pattern string, h http.HandlerFunc) route { return route{pattern, h, true} }

// routeTable is every route the API answers. To add one, add a row: the
// router, the 405 answers and the request log all key on the pattern, and
// rateClasses names the rows that don't spend the read bucket.
//
// More specific patterns win regardless of order (GET /secrets/grants over
// DELETE /secrets/{name...}); a pair neither of which is more specific panics
// when the server is built. A pattern with no method answers every method: the
// public routes do, as they always have (a load balancer may probe /health
// with OPTIONS), and so does the proxy, which hands the method to the app.
func (s *Server) routeTable() []route {
	return []route{
		open("/health", s.handleHealth),

		// The web shell is reached from a browser, which can't set an
		// Authorization header: the page is static and the token is checked
		// on the WebSocket, after the upgrade. Anything else under /_shell is
		// a 404 for everyone, so the prefix gives nothing away.
		open("/_shell/{id}", s.serveShellHTML),
		open("/_shell/{id}/{$}", s.serveShellHTML),
		open("/_shell/{id}/ws", s.handleShellWS),
		open("/_shell/", handleNotFound),
		open("/_shell", handleNotFound),

		api("GET /sandboxes", s.handleSandboxList),
		api("POST /sandboxes", s.handleSandboxCreate),
		api("GET /sandboxes/{id}", s.handleSandboxGet),
		api("PATCH /sandboxes/{id}", s.handleSandboxPatch),
		api("DELETE /sandboxes/{id}", s.handleSandboxDelete),
		api("POST /sandboxes/{id}/start", s.handleSandboxStart),
		api("POST /sandboxes/{id}/stop", s.handleSandboxStop),
		api("POST /sandboxes/{id}/exec", s.handleSandboxExec),
		api("GET /sandboxes/{id}/exec/ws", s.handleSandboxExecWS),
		api("GET /sandboxes/{id}/ws", s.handleSandboxWS),
		api("GET /sandboxes/{id}/sessions", s.handleSandboxSessions),
		api("GET /sandboxes/{id}/ports", s.handleSandboxPorts),
		api("GET /sandboxes/{id}/tunnel", s.handleSandboxTunnel),
		api("/sandboxes/{id}/proxy/{portPath...}", s.handleSandboxProxyRoute),
		api("GET /sandboxes/{id}/files", s.handleSandboxFileRead),
		api("PUT /sandboxes/{id}/files", s.handleSandboxFileWrite),
		api("HEAD /sandboxes/{id}/files", s.handleSandboxFileStat),
		api("POST /sandboxes/{id}/save-image", s.handleSandboxSaveImage),
		api("POST /sandboxes/{id}/checkpoint", s.handleSandboxCheckpoint),
		api("POST /sandboxes/{id}/shell-token", s.handleShellTokenCreate),
		api("DELETE /sandboxes/{id}/shell-token", s.handleShellTokenRevoke),
		api("GET /sandboxes/{id}/publish", s.handleListPublishRules),
		api("GET /sandboxes/{id}/publish/{$}", s.handleListPublishRules),
		api("POST /sandboxes/{id}/publish", s.handlePublish),
		api("POST /sandboxes/{id}/publish/{$}", s.handlePublish),
		api("DELETE /sandboxes/{id}/publish/{port}", s.handleUnpublish),

		api("GET /ports", s.handleAllPorts),
		api("GET /events", s.handleEvents),

		api("GET /templates", s.handleTemplateList),
		api("POST /templates", s.handleTemplateCreate),
		api("GET /templates/{id}", s.handleTemplateGet),
		api("DELETE /templates/{id}", s.handleTemplateDelete),

		// A secret name is not restricted to one path segment, so the delete
		// takes the whole remainder; the grant routes are the exceptions that
		// win by being more specific. POST /secrets/{name}/grants reaches a
		// secret that is itself named "grants".
		api("GET /secrets", s.handleSecretList),
		api("POST /secrets", s.handleSecretCreate),
		api("GET /secrets/grants", s.handleSecretGrants),
		api("POST /secrets/{name}/grants", s.handleSecretGrantCreate),
		api("DELETE /secrets/grants/{id}", s.handleSecretGrantRevoke),
		api("DELETE /secrets/{name...}", s.handleSecretDelete),

		api("GET /volumes", s.handleVolumeList),
		api("POST /volumes", s.handleVolumeCreate),
		api("GET /volumes/{name}", s.handleVolumeGet),
		api("DELETE /volumes/{name}", s.handleVolumeDelete),
		api("POST /volumes/{name}/resize", s.handleVolumeResize),
		api("POST /volumes/{name}/snapshot", s.handleVolumeSnapshot),
		api("GET /volumes/{name}/backups", s.handleVolumeBackupList),
		api("POST /volumes/{name}/backups", s.handleVolumeBackupCreate),
		api("POST /volumes/{name}/backups/restore", s.handleVolumeBackupRestore),
		api("DELETE /volumes/{name}/backups/{id}", s.handleVolumeBackupDelete),

		api("GET /images", s.handleImageList),
		api("POST /images/pull", s.handleImagePull),
		api("POST /images/import", s.handleImageImport),
		api("GET /images/{name}", s.handleImageGet),
		api("DELETE /images/{name}", s.handleImageDelete),

		api("GET /snapshots", s.handleSnapshotList),
		api("POST /snapshots/import", s.handleSnapshotImport),
		api("GET /snapshots/{name}", s.handleSnapshotGet),
		api("DELETE /snapshots/{name}", s.handleSnapshotDelete),
		api("POST /snapshots/{name}/resume", s.handleSnapshotResume),
		api("GET /snapshots/{name}/export", s.handleSnapshotExport),

		api("GET /tasks/{id}", s.handleTaskGet),
		api("DELETE /tasks/{id}", s.handleTaskDelete),
	}
}

// routes registers the route table and records which patterns are public.
func (s *Server) routes() {
	table := s.routeTable()
	s.public = make(map[string]bool, len(table))
	for _, rt := range table {
		s.mux.HandleFunc(rt.pattern, rt.handler)
		s.public[rt.pattern] = rt.public
	}
}

// unmatchedRoute is the route label of a request no route matched: a 404, a 405
// or a redirect to the canonical path. It is a fixed word rather than the
// request path, so a route label is always a pattern from the route table.
const unmatchedRoute = "unmatched"

// routeLabel is how a matched pattern ("" when none matched) appears in logs
// and metrics.
func routeLabel(pattern string) string {
	if pattern == "" {
		return unmatchedRoute
	}
	return pattern
}

func handleNotFound(w http.ResponseWriter, r *http.Request) {
	errResp(w, 404, "not found")
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	h := map[string]any{
		"status": "ok",
		"uptime": time.Since(s.startTime).Round(time.Second).String(),
	}
	if cr, ok := s.engine.(engine.CgroupReporter); ok {
		h["cgroups"] = cr.CgroupStatus()
	}
	writeJSON(w, 200, h)
}

func genID() string {
	b := make([]byte, 8)
	rand.Read(b)
	return hex.EncodeToString(b)
}

var validNameRe = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]{0,62}$`)

func isValidName(name string) bool {
	return validNameRe.MatchString(name)
}

// isValidMountPath validates that a volume mount path is safe.
// Rejects system paths that would overlay critical guest filesystems.
func isValidMountPath(mount string) bool {
	if mount == "" || mount[0] != '/' {
		return false // must be absolute
	}
	clean := filepath.Clean(mount)
	if strings.Contains(clean, "..") {
		return false
	}
	// Reject system mount points that lohar or the kernel use
	forbidden := []string{"/", "/proc", "/sys", "/dev", "/dev/pts",
		"/run", "/tmp", "/etc", "/bin", "/sbin", "/lib", "/lib64",
		"/usr", "/usr/local/bin", "/boot", "/root"}
	for _, f := range forbidden {
		if clean == f {
			return false
		}
	}
	return true
}

// getUserSandbox is a helper that retrieves a sandbox scoped to the authenticated user.
// id is the URL's {id}, which may be a name: handlers use the returned sandbox's
// ID and EngineID downstream, never the URL value. Returns nil and writes a 404
// error if not found.
func (s *Server) getUserSandbox(w http.ResponseWriter, r *http.Request, id string) *store.Sandbox {
	user := UserFromContext(r.Context())
	sb, err := s.store.GetSandbox(r.Context(), user.ID, id)
	if err != nil {
		// The CLI passes names through without client-side resolution, so this
		// message is what a user sees for a typo'd name — name it clearly.
		errResp(w, 404, fmt.Sprintf("sandbox %q not found", id))
		return nil
	}
	return sb
}
