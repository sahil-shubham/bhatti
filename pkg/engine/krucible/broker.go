package krucible

import (
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"

	"github.com/sahil-shubham/bhatti/pkg/engine"
)

// Credential substitution: each owner's netd asks the daemon's credential
// broker for leaf certificates and secret values over a unix socket in the
// netd's own directory. netd drops to netdUID:netdGID once its sockets are
// open, so the socket (and the path to it) must be reachable by that identity
// and nobody else that matters: the socket is owned by netdUID, mode 0600; the
// netd directory is root:netdGID 0710 (traverse only); the socket directory
// is 0711 (traverse only — its other children are 0700).

const (
	netdUID = 65534
	netdGID = 65534
)

var _ engine.CredentialBrokerHost = (*Engine)(nil)

func brokerSockPath(netdDir string) string { return filepath.Join(netdDir, "b.sock") }

// SetCredentialBroker attaches the daemon's broker and serves it to every
// netd already known (adopted across a daemon restart); ensureNetd serves it
// to netds spawned later.
func (e *Engine) SetCredentialBroker(b engine.CredentialBroker) {
	e.netdMu.Lock()
	e.broker = b
	insts := make([]*netdInstance, 0, len(e.netds))
	for _, inst := range e.netds {
		insts = append(insts, inst)
	}
	e.netdMu.Unlock()
	for _, inst := range insts {
		inst.mu.Lock()
		serveBrokerLocked(inst, b)
		inst.mu.Unlock()
	}
}

// serveBrokerLocked opens inst's broker socket, if it isn't open, and hands
// it to b. Caller holds inst.mu. A failure leaves the netd without credential
// substitution (its connections are spliced untouched), never without network.
func serveBrokerLocked(inst *netdInstance, b engine.CredentialBroker) {
	if b == nil || inst.brokerLn != nil {
		return
	}
	userID, owned := strings.CutPrefix(inst.owner, "u:")
	if !owned {
		return // an unowned sandbox has no grants
	}
	path := brokerSockPath(inst.dir)
	_ = os.Remove(path) // a stale socket from the previous daemon
	ln, err := net.Listen("unix", path)
	if err != nil {
		slog.Warn("krucible.broker_socket", "owner", inst.owner, "path", path, "error", err)
		return
	}
	if err := shareWithNetd(inst.dir, path); err != nil {
		ln.Close()
		slog.Warn("krucible.broker_socket", "owner", inst.owner, "path", path, "error", err)
		return
	}
	inst.brokerLn = ln
	go b.Serve(ln, userID)
}

// shareWithNetd lets the confined netd (and only it) reach the broker socket.
// A daemon that isn't root runs netd as itself, which needs nothing.
func shareWithNetd(dir, sock string) error {
	if os.Geteuid() != 0 {
		return nil
	}
	if err := os.Chown(dir, 0, netdGID); err != nil {
		return err
	}
	if err := os.Chmod(dir, 0o710); err != nil {
		return err
	}
	if err := os.Chown(sock, netdUID, netdGID); err != nil {
		return err
	}
	return os.Chmod(sock, 0o600)
}

// shareSocketDir makes the socket directory traversable (not listable) so the
// confined netd can reach its broker socket below it. Its other children are
// 0700, so this opens nothing else.
func shareSocketDir(dir string) {
	if os.Geteuid() != 0 {
		return
	}
	if err := os.Chmod(dir, 0o711); err != nil {
		slog.Warn("krucible.socket_dir", "dir", dir, "error", err)
		return
	}
	// Every ancestor must be traversable too; a private one (an operator's
	// krucible_socket_dir under a 0700 dir) silently disables substitution.
	for d := filepath.Dir(dir); ; d = filepath.Dir(d) {
		if fi, err := os.Stat(d); err == nil && fi.Mode().Perm()&0o001 == 0 {
			slog.Warn("krucible.socket_dir", "dir", d, "error", "not traversable by netd; credential substitution can't reach the broker")
		}
		if d == filepath.Dir(d) {
			return
		}
	}
}
