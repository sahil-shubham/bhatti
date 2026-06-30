package krucible

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sahil-shubham/bhatti/pkg/engine"
)

// TestEngineCapabilities boots one VM and probes the agent surface + TSI
// networking, printing a WORKS / FAILS / N/A matrix. Informational — run with:
//
//	go test ./pkg/engine/krucible/ -run TestEngineCapabilities -v
//
// Only Create failing is fatal; individual probes are reported, not asserted,
// so this is a live "what works today" report rather than a pass/fail gate.
func TestEngineCapabilities(t *testing.T) {
	repo := repoRoot(t)
	if !hasLibkrun() {
		t.Skip("libkrun not installed; skipping")
	}
	if !hasHypervisor() {
		t.Skip("no hypervisor (/dev/kvm or HVF); skipping VM suite")
	}
	vmm := filepath.Join(repo, "bhatti-vmm")
	if _, err := os.Stat(vmm); err != nil {
		t.Skip("bhatti-vmm not built — run `make vmm`; skipping")
	}

	eng, err := New(Config{
		DataDir:    t.TempDir(),
		BaseRootfs: buildBaseRootfs(t, repo),
		VMMBinary:  vmm,
		LibDir:     libDir(),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	info, err := eng.Create(ctx, engine.SandboxSpec{Name: "probe", CPUs: 1, MemoryMB: 512})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	defer eng.Destroy(context.Background(), info.ID)
	id := info.ID

	var rows []string
	record := func(cap, status, detail string) {
		rows = append(rows, fmt.Sprintf("  %-26s %-7s %s", cap, status, detail))
	}
	run := func(cap string, mustWork bool, fn func() (string, error)) {
		detail, err := fn()
		if err != nil {
			record(cap, "FAILS", err.Error())
			if mustWork {
				t.Errorf("%s FAILED (must work on krucible): %v", cap, err)
			}
		} else {
			record(cap, "WORKS", detail)
		}
	}
	// must = assert (regression fails the test); info = report only (e.g. caps
	// that need a richer guest userland than the minimal box rootfs).
	must := func(cap string, fn func() (string, error)) { run(cap, true, fn) }
	report := func(cap string, fn func() (string, error)) { run(cap, false, fn) }

	must("create", func() (string, error) { return "booted, agent ready", nil })

	report("exec exit-code 0", func() (string, error) {
		r, err := eng.Exec(ctx, id, []string{"true"})
		if err != nil {
			return "", err
		}
		if r.ExitCode != 0 {
			return "", fmt.Errorf("exit=%d", r.ExitCode)
		}
		return "true -> 0", nil
	})
	report("exec exit-code !=0", func() (string, error) {
		r, err := eng.Exec(ctx, id, []string{"false"})
		if err != nil {
			return "", err
		}
		if r.ExitCode != 1 {
			return "", fmt.Errorf("exit=%d, want 1", r.ExitCode)
		}
		return "false -> 1", nil
	})
	report("exec stdout", func() (string, error) {
		r, err := eng.Exec(ctx, id, []string{"echo", "hi-stdout"})
		if err != nil {
			return "", err
		}
		if !strings.Contains(r.Stdout, "hi-stdout") {
			return "", fmt.Errorf("stdout=%q", r.Stdout)
		}
		return "captured", nil
	})
	report("exec stderr", func() (string, error) {
		r, err := eng.Exec(ctx, id, []string{"errcho", "hi-stderr"})
		if err != nil {
			return "", err
		}
		if !strings.Contains(r.Stderr, "hi-stderr") {
			return "", fmt.Errorf("stderr=%q", r.Stderr)
		}
		return "captured", nil
	})

	report("file write+read", func() (string, error) {
		content := "krucible-file-probe-12345"
		if err := eng.FileWrite(ctx, id, "/tmp/probe.txt", "0644", int64(len(content)), strings.NewReader(content)); err != nil {
			return "", fmt.Errorf("write: %w", err)
		}
		var buf bytes.Buffer
		if _, _, err := eng.FileRead(ctx, id, "/tmp/probe.txt", &buf); err != nil {
			return "", fmt.Errorf("read: %w", err)
		}
		if buf.String() != content {
			return "", fmt.Errorf("roundtrip mismatch: %q", buf.String())
		}
		return "roundtrip ok", nil
	})
	report("file stat", func() (string, error) {
		fi, err := eng.FileStat(ctx, id, "/init.krun")
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("/init.krun size=%d", fi.Size), nil
	})
	report("file list", func() (string, error) {
		fis, err := eng.FileList(ctx, id, "/bin")
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("/bin has %d entries", len(fis)), nil
	})

	must("tsi egress: tcp", func() (string, error) {
		r, err := eng.Exec(ctx, id, []string{"netcheck", "tcp"})
		if err != nil {
			return "", err
		}
		if r.ExitCode != 0 {
			return "", fmt.Errorf("%s", strings.TrimSpace(r.Stdout))
		}
		return strings.TrimSpace(r.Stdout), nil
	})
	must("tsi egress: dns", func() (string, error) {
		r, err := eng.Exec(ctx, id, []string{"netcheck", "dns"})
		if err != nil {
			return "", err
		}
		if r.ExitCode != 0 {
			return "", fmt.Errorf("%s", strings.TrimSpace(r.Stdout))
		}
		return strings.TrimSpace(r.Stdout), nil
	})
	must("tsi egress: http", func() (string, error) {
		r, err := eng.Exec(ctx, id, []string{"netcheck", "http"})
		if err != nil {
			return "", err
		}
		if r.ExitCode != 0 {
			return "", fmt.Errorf("%s", strings.TrimSpace(r.Stdout))
		}
		return strings.TrimSpace(r.Stdout), nil
	})

	must("detached exec + tunnel", func() (string, error) {
		if _, _, err := eng.ExecDetached(ctx, id, []string{"netcheck", "serve", "8088"}, "/tmp/serve.log"); err != nil {
			return "", fmt.Errorf("detached: %w", err)
		}
		time.Sleep(1500 * time.Millisecond)
		conn, err := eng.Tunnel(ctx, id, 8088)
		if err != nil {
			return "", fmt.Errorf("tunnel: %w", err)
		}
		defer conn.Close()
		if _, err := io.WriteString(conn, "GET / HTTP/1.0\r\nHost: x\r\n\r\n"); err != nil {
			return "", fmt.Errorf("write: %w", err)
		}
		body, err := readWithTimeout(conn, 5*time.Second)
		if err != nil && len(body) == 0 {
			return "", fmt.Errorf("read: %w", err)
		}
		if !strings.Contains(string(body), "hello-from-guest") {
			return "", fmt.Errorf("unexpected response: %q", string(body))
		}
		return "tunneled to guest HTTP server", nil
	})

	report("listening ports (ss)", func() (string, error) {
		ports, err := eng.ListeningPorts(ctx, id)
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("%v", ports), nil
	})
	report("shell / PTY", func() (string, error) {
		_, term, err := eng.ShellSession(ctx, id)
		if err != nil {
			return "", err
		}
		term.Close()
		return "opened", nil
	})

	t.Logf("\n=== krucible capability matrix (sandbox %s) ===\n%s\n", id, strings.Join(rows, "\n"))
}

// readWithTimeout reads the first chunk (up to 4KB) so we can tell "data flows"
// from "nothing" without waiting for EOF (a half-close that may not propagate).
func readWithTimeout(r io.Reader, d time.Duration) ([]byte, error) {
	type res struct {
		b   []byte
		err error
	}
	ch := make(chan res, 1)
	go func() {
		buf := make([]byte, 4096)
		n, err := r.Read(buf)
		ch <- res{buf[:n], err}
	}()
	select {
	case r := <-ch:
		return r.b, r.err
	case <-time.After(d):
		return nil, fmt.Errorf("timeout")
	}
}
