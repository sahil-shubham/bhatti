//go:build linux

package firecracker

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)


// fcProcess holds the resources for a running Firecracker process.
type fcProcess struct {
	cmd       *exec.Cmd
	cancel    context.CancelFunc
	stderrBuf *ringBuffer
	socket    string    // host-visible API socket path
	jailRoot  string    // chroot root dir (empty if bare mode)
}

// startFCOpts are passed to startFC for jailed mode. Bare mode ignores them.
type startFCOpts struct {
	id       string // sandbox/VM ID
	vcpus    int64
	memMB    int64
	files    map[string]string // chroot filename → host path (hard-linked into jail)
}

// startFC launches a Firecracker process. In jailed mode, sets up the chroot,
// hard-links files, applies cgroups, and drops privileges.
func (e *Engine) startFC(socketPath string, opts startFCOpts) (*fcProcess, error) {
	if e.cfg.jailed() {
		return e.startFCJailed(socketPath, opts)
	}
	return e.startFCBare(socketPath)
}

// startFCBare launches FC directly (no isolation). Used in dev mode.
func (e *Engine) startFCBare(socketPath string) (*fcProcess, error) {
	if err := validateSocketPath(socketPath); err != nil {
		return nil, err
	}
	os.Remove(socketPath)
	vmCtx, cancel := context.WithCancel(context.Background())
	stderrBuf := newRingBuffer(64 * 1024)
	cmd := exec.CommandContext(vmCtx, e.cfg.FCBinary, "--api-sock", socketPath)
	cmd.Stderr = stderrBuf
	if err := cmd.Start(); err != nil {
		cancel()
		return nil, fmt.Errorf("start firecracker: %w", err)
	}
	waitForSocket(socketPath)
	return &fcProcess{cmd: cmd, cancel: cancel, stderrBuf: stderrBuf, socket: socketPath}, nil
}

// startFCJailed launches FC inside a jailer chroot with UID drop, PID namespace,
// and cgroup limits.
func (e *Engine) startFCJailed(socketPath string, opts startFCOpts) (*fcProcess, error) {
	jailStart := time.Now()
	jailPhase := func(name string) {
		slog.Debug("fc_jail.phase", "sandbox", opts.id, "phase", name,
			"elapsed_ms", time.Since(jailStart).Milliseconds())
	}

	chrootBase := filepath.Join(e.cfg.DataDir, "jails")
	jailRoot := filepath.Join(chrootBase, "firecracker", opts.id, "root")
	// Clean stale jail from previous run (e.g. stop/start cycle).
	// The jailer's mknod for /dev/kvm and /dev/net/tun fails with
	// EEXIST if these device nodes are left over.
	os.RemoveAll(filepath.Join(chrootBase, "firecracker", opts.id))
	if err := os.MkdirAll(jailRoot, 0700); err != nil {
		return nil, fmt.Errorf("create jail root: %w", err)
	}
	jailPhase("mkdir_done")

	// Hard-link all files FC needs into the chroot.
	// Hard-links are instant (same filesystem) and FC writes go to the
	// same inode — no copy, no sync issue.
	for name, hostPath := range opts.files {
		dst := filepath.Join(jailRoot, name)
		os.Remove(dst) // remove stale from previous run
		if err := os.Link(hostPath, dst); err != nil {
			// Cross-device? Fall back to copy (e.g. kernel on different mount)
			if err := copyBlock(hostPath, dst); err != nil {
				os.RemoveAll(filepath.Join(chrootBase, "firecracker", opts.id))
				return nil, fmt.Errorf("link %s into jail: %w", name, err)
			}
		}
	}

	jailPhase("hardlink_done")

	// chown the chroot tree to the jail user so FC can write
	uid, gid := e.cfg.JailUID, e.cfg.JailGID
	filepath.Walk(jailRoot, func(path string, info os.FileInfo, err error) error {
		if err == nil {
			os.Lchown(path, uid, gid)
		}
		return nil
	})

	jailPhase("chown_done")

	// The API socket path as seen from inside the chroot.
	// Must be short to stay under the 108-byte Unix socket limit.
	internalSock := "api.sock"
	hostSock := filepath.Join(jailRoot, internalSock)
	os.Remove(hostSock)

	vmCtx, cancel := context.WithCancel(context.Background())
	stderrBuf := newRingBuffer(64 * 1024)

	args := []string{
		"--id", opts.id,
		"--exec-file", e.cfg.FCBinary,
		"--uid", fmt.Sprintf("%d", uid),
		"--gid", fmt.Sprintf("%d", gid),
		"--chroot-base-dir", chrootBase,
		// NOTE: --new-pid-ns is intentionally omitted. With --new-pid-ns,
		// the jailer's pivot_root makes the API socket inaccessible from
		// the host mount namespace. Chroot + UID drop + cgroups provide
		// the critical isolation. PID namespace can be revisited when we
		// add per-VM network namespaces (Phase 6b).
		"--cgroup-version", "2",
	}

	// cgroup limits — at least one --cgroup flag is REQUIRED with cgroup v2.
	// Without it, the jailer tries to move the process directly into
	// /sys/fs/cgroup/firecracker which hits "no internal process constraint".
	vcpus := opts.vcpus
	if vcpus < 1 {
		vcpus = 1
	}
	memMB := opts.memMB
	if memMB < 128 {
		memMB = 2048
	}
	args = append(args, "--cgroup", fmt.Sprintf("cpu.max=%d 100000", vcpus*100000))
	args = append(args, "--cgroup", fmt.Sprintf("memory.max=%d", (memMB+128)*1024*1024))

	// Everything after "--" is forwarded to firecracker
	args = append(args, "--", "--api-sock", internalSock)

	jailPhase("jailer_cmd_built")
	cmd := exec.CommandContext(vmCtx, e.cfg.JailerBinary, args...)
	cmd.Stderr = stderrBuf
	if err := cmd.Start(); err != nil {
		cancel()
		os.RemoveAll(filepath.Join(chrootBase, "firecracker", opts.id))
		return nil, fmt.Errorf("start jailer: %w", err)
	}
	jailPhase("jailer_started")

	// Wait for the API socket. Check process liveness via /proc to
	// detect early death without calling cmd.Wait() (which can only
	// be called once — killFC needs it later).
	socketReady := false
	for i := 0; i < 100; i++ {
		if _, err := os.Stat(hostSock); err == nil {
			jailPhase("socket_ready")
			socketReady = true
			break
		}
		// Check if process died
		if _, err := os.Stat(fmt.Sprintf("/proc/%d", cmd.Process.Pid)); err != nil {
			cancel()
			cmd.Wait() // reap
			os.RemoveAll(filepath.Join(chrootBase, "firecracker", opts.id))
			return nil, fmt.Errorf("jailer exited prematurely\nstderr: %s", stderrBuf.String())
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !socketReady {
		killFC(cmd, 1*time.Second)
		cancel()
		os.RemoveAll(filepath.Join(chrootBase, "firecracker", opts.id))
		return nil, fmt.Errorf("jailer: API socket not ready after 2s\nstderr: %s", stderrBuf.String())
	}

	return &fcProcess{
		cmd: cmd, cancel: cancel, stderrBuf: stderrBuf,
		socket: hostSock, jailRoot: jailRoot,
	}, nil
}

// waitForSocket polls for a Unix socket to appear (up to 1s).
func waitForSocket(path string) {
	for i := 0; i < 50; i++ {
		if _, err := os.Stat(path); err == nil {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// validateSocketPath checks that a Unix socket path fits within the 108-byte
// limit on Linux. A long DataDir could overflow silently.
func validateSocketPath(path string) error {
	if len(path) >= 108 {
		return fmt.Errorf("socket path too long (%d >= 108): %s "+
			"\u2014 use a shorter data_dir", len(path), path)
	}
	return nil
}

// killFC sends SIGTERM and waits up to timeout for clean exit, then SIGKILL.
// Firecracker may or may not handle SIGTERM in bare mode — the timeout
// ensures we don't block indefinitely before falling back to SIGKILL.
func killFC(cmd *exec.Cmd, timeout time.Duration) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	cmd.Process.Signal(syscall.SIGTERM)
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case <-done:
		return
	case <-time.After(timeout):
		cmd.Process.Kill()
		<-done
	}
}

// copyBlock copies a block device file (rootfs, volume, snapshot artifact).
// Uses reflink for instant CoW clones on btrfs/xfs, falls back to sparse copy.
func copyBlock(src, dst string) error {
	return exec.Command("cp", "--reflink=auto", "--sparse=always", src, dst).Run()
}

// copyBlockCtx is like copyBlock but respects context cancellation.
// Used in errgroup pipelines where remaining copies should abort on first failure.
func copyBlockCtx(ctx context.Context, src, dst string) error {
	return exec.CommandContext(ctx, "cp", "--reflink=auto", "--sparse=always", src, dst).Run()
}

// copyRootfs is an alias for copyBlock (kept for call-site readability).
func copyRootfs(src, dst string) error {
	return copyBlock(src, dst)
}

// fcAPIClient returns an HTTP client that talks to Firecracker's API
// over a Unix socket. Keep-alives are enabled, so multi-call sequences
// (the ~10-PUT Create boot configuration, the pause+snapshot dance)
// reuse a single underlying connection instead of dialing/closing the
// socket per call. Each call site still constructs its own client, so
// across-call connection reuse is not in scope here — each Pause /
// Resume / BalloonSet on a VM still dials fresh.
//
// Pre-fix, DisableKeepAlives=true forced one connection per request
// with a defensive note about "stale socket issues." The relevant
// staleness is when FC dies and respawns (snapshot stop/restore), at
// which point the socket on the kernel side is gone and any cached
// connection would be invalid. Within a single fcAPIClient lifetime
// FC does not respawn (the caller holds the VM's stateMu, and
// snapshot stop/restore constructs a fresh client afterwards), so
// keep-alives are safe here. Tranche 0a item #5 of PLAN-bhatti-v2.md.
func fcAPIClient(socketPath string) *http.Client {
	return &http.Client{
		// No Timeout — each call site uses context.WithTimeout.
		// This avoids the old 10s global timeout silently racing with
		// per-call context deadlines (see issue #4).
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				d := net.Dialer{Timeout: 5 * time.Second}
				return d.DialContext(ctx, "unix", socketPath)
			},
		},
	}
}

func fcPut(ctx context.Context, client *http.Client, path, body string) error {
	req, _ := http.NewRequestWithContext(ctx, "PUT", "http://localhost"+path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("firecracker %s: %s %s", path, resp.Status, string(b))
	}
	return nil
}

func fcPatch(ctx context.Context, client *http.Client, path, body string) error {
	req, _ := http.NewRequestWithContext(ctx, "PATCH", "http://localhost"+path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("firecracker %s: %s %s", path, resp.Status, string(b))
	}
	return nil
}

