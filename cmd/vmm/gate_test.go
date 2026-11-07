//go:build krucible

package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sahil-shubham/bhatti/pkg/launchgate"
)

// The helper is a cgo binary that main() can't be driven from in-process (it
// exits, and libkrun owns the process), so these run the `make vmm` build.
// The spec path never exists: what the helper complains about says how far it
// got, and the gate must come before the spec is even read.
//
// This test binary links libkrun itself, so it needs the library on its
// loader path like the helper does (macOS: DYLD_FALLBACK_LIBRARY_PATH, Linux:
// LD_LIBRARY_PATH, pointing at libkrucible/_install/lib or lib64).

const missingSpec = "/nonexistent/spec.json"

func builtVMM(t *testing.T) string {
	t.Helper()
	bin, err := filepath.Abs("../../bhatti-vmm")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(bin); err != nil {
		t.Skip("bhatti-vmm not built — run `make vmm`; skipping")
	}
	return bin
}

// vmmCmd prepares bin for the dev tree: libkrun is found through the library
// path, as the engine passes it to the helper.
func vmmCmd(t *testing.T, bin string, args ...string) (*exec.Cmd, *bytes.Buffer) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	t.Cleanup(cancel)
	cmd := exec.CommandContext(ctx, bin, args...)
	for _, sub := range []string{"lib64", "lib"} {
		p, err := filepath.Abs("../../libkrucible/_install/" + sub)
		if err != nil {
			continue
		}
		if m, _ := filepath.Glob(filepath.Join(p, "libkrun.*")); len(m) > 0 {
			cmd.Env = append(os.Environ(), "DYLD_FALLBACK_LIBRARY_PATH="+p, "LD_LIBRARY_PATH="+p)
			break
		}
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	return cmd, &stderr
}

// finishVMM waits for the started helper and returns its exit code and stderr.
// A helper that can't even be loaded here skips the test rather than failing it.
func finishVMM(t *testing.T, cmd *exec.Cmd, stderr *bytes.Buffer) (int, string) {
	t.Helper()
	err := cmd.Wait()
	out := stderr.String()
	if strings.Contains(out, "Library not loaded") || strings.Contains(out, "error while loading shared libraries") {
		t.Skipf("bhatti-vmm cannot load libkrun here: %s", out)
	}
	var ee *exec.ExitError
	if err == nil {
		return 0, out
	}
	// -1 is a signal, or the timeout killing a helper stuck at the gate.
	if !errors.As(err, &ee) || ee.ExitCode() < 0 {
		t.Fatalf("bhatti-vmm did not exit on its own: %v\n%s", err, out)
	}
	return ee.ExitCode(), out
}

// TestVMMLaunchGate: the helper reads the gate before anything else, so a gate
// that never opens ends it before it has looked at the spec, and an opened one
// lets it go on to the spec.
func TestVMMLaunchGate(t *testing.T) {
	bin := builtVMM(t)

	t.Run("closed without the byte", func(t *testing.T) {
		g, err := launchgate.New()
		if err != nil {
			t.Fatal(err)
		}
		defer g.Close()
		cmd, stderr := vmmCmd(t, bin, missingSpec)
		if err := g.Start(cmd); err != nil {
			t.Fatal(err)
		}
		g.Close()
		code, out := finishVMM(t, cmd, stderr)
		if code == 0 {
			t.Fatalf("exit 0, want a failure\n%s", out)
		}
		if !strings.Contains(out, "gate") {
			t.Fatalf("stderr = %q, want it to mention the gate", out)
		}
		if strings.Contains(out, "read spec") {
			t.Fatalf("stderr = %q: the spec was read before the gate", out)
		}
	})

	t.Run("opened", func(t *testing.T) {
		g, err := launchgate.New()
		if err != nil {
			t.Fatal(err)
		}
		defer g.Close()
		cmd, stderr := vmmCmd(t, bin, missingSpec)
		if err := g.Start(cmd); err != nil {
			t.Fatal(err)
		}
		if err := g.Open(); err != nil {
			t.Fatal(err)
		}
		code, out := finishVMM(t, cmd, stderr)
		if code == 0 {
			t.Fatalf("exit 0, want the missing spec to fail it\n%s", out)
		}
		if !strings.Contains(out, "read spec") {
			t.Fatalf("stderr = %q, want the helper to have gone on to read the spec", out)
		}
		if strings.Contains(out, "gate") {
			t.Fatalf("stderr = %q: an opened gate still stopped the helper", out)
		}
	})

	t.Run("without a gate", func(t *testing.T) {
		cmd, stderr := vmmCmd(t, bin, missingSpec)
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		code, out := finishVMM(t, cmd, stderr)
		if code == 0 || !strings.Contains(out, "read spec") {
			t.Fatalf("exit %d, stderr %q: want a failed read of the spec", code, out)
		}
	})

	t.Run("malformed gate descriptor", func(t *testing.T) {
		cmd, stderr := vmmCmd(t, bin, missingSpec)
		cmd.Env = append(cmd.Environ(), launchgate.Env+"=nope")
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		code, out := finishVMM(t, cmd, stderr)
		if code == 0 || !strings.Contains(out, launchgate.Env) || strings.Contains(out, "read spec") {
			t.Fatalf("exit %d, stderr %q: want a refusal naming the bad gate before any work", code, out)
		}
	})
}
