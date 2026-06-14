// Package enginetest holds VMM-agnostic behavior assertions that any
// engine.Engine implementation must satisfy. The SAME suite runs against
// Firecracker and krucible (each provides a NewEngine factory) — that parity is
// the real correctness gate, not bespoke per-engine scripts. Engine/rootfs-
// specific surface (network backend, shell userland) stays in each engine's
// own tests.
package enginetest

import (
	"bytes"
	"context"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/sahil-shubham/bhatti/pkg/agent"
	"github.com/sahil-shubham/bhatti/pkg/agent/proto"
	"github.com/sahil-shubham/bhatti/pkg/engine"
)

// NewEngine builds a ready engine for the suite, or calls t.Skip if the engine
// can't run here (e.g. libkrun/KVM unavailable).
type NewEngine func(t *testing.T) engine.Engine

// fileEngine is the optional file surface both FC and krucible implement.
type fileEngine interface {
	FileWrite(ctx context.Context, id, path, mode string, size int64, r io.Reader) error
	FileRead(ctx context.Context, id, path string, w io.Writer, opts ...agent.FileReadOpts) (int64, string, error)
	FileStat(ctx context.Context, id, path string) (*proto.FileInfo, error)
	FileList(ctx context.Context, id, path string) ([]proto.FileInfo, error)
}

// RunAgentSuite boots one sandbox and asserts the VMM-agnostic core: status,
// list, exec (exit codes + stdout), and the file API. Commands used (true,
// false, echo) and lohar-internal file ops exist on any reasonable rootfs, so
// the suite is portable across engines.
func RunAgentSuite(t *testing.T, newEngine NewEngine) {
	eng := newEngine(t) // may t.Skip
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	info, err := eng.Create(ctx, engine.SandboxSpec{Name: "suite", CPUs: 1, MemoryMB: 512})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	id := info.ID
	t.Cleanup(func() { eng.Destroy(context.Background(), id) })
	if info.Status != "running" {
		t.Fatalf("Create status = %q, want running", info.Status)
	}

	t.Run("Status", func(t *testing.T) {
		s, err := eng.Status(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		if s.Status != "running" {
			t.Fatalf("status = %q, want running", s.Status)
		}
	})

	t.Run("List", func(t *testing.T) {
		list, err := eng.List(ctx)
		if err != nil {
			t.Fatal(err)
		}
		for _, s := range list {
			if s.ID == id {
				return
			}
		}
		t.Fatalf("sandbox %s not present in List (%d entries)", id, len(list))
	})

	t.Run("ExecExit0", func(t *testing.T) {
		r, err := eng.Exec(ctx, id, []string{"true"})
		if err != nil {
			t.Fatal(err)
		}
		if r.ExitCode != 0 {
			t.Fatalf("true exit = %d, want 0", r.ExitCode)
		}
	})

	t.Run("ExecExitNonZero", func(t *testing.T) {
		r, err := eng.Exec(ctx, id, []string{"false"})
		if err != nil {
			t.Fatal(err)
		}
		if r.ExitCode != 1 {
			t.Fatalf("false exit = %d, want 1", r.ExitCode)
		}
	})

	t.Run("ExecStdout", func(t *testing.T) {
		r, err := eng.Exec(ctx, id, []string{"echo", "suite-marker"})
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(r.Stdout, "suite-marker") {
			t.Fatalf("echo stdout = %q, want it to contain suite-marker", r.Stdout)
		}
	})

	t.Run("Files", func(t *testing.T) {
		fe, ok := eng.(fileEngine)
		if !ok {
			t.Skip("engine does not implement the file surface")
		}
		const content = "enginetest-file-roundtrip-42"
		if err := fe.FileWrite(ctx, id, "/tmp/suite.txt", "0644", int64(len(content)), strings.NewReader(content)); err != nil {
			t.Fatalf("FileWrite: %v", err)
		}
		var buf bytes.Buffer
		if _, _, err := fe.FileRead(ctx, id, "/tmp/suite.txt", &buf); err != nil {
			t.Fatalf("FileRead: %v", err)
		}
		if buf.String() != content {
			t.Fatalf("file roundtrip = %q, want %q", buf.String(), content)
		}
		if _, err := fe.FileStat(ctx, id, "/tmp/suite.txt"); err != nil {
			t.Fatalf("FileStat: %v", err)
		}
		if _, err := fe.FileList(ctx, id, "/tmp"); err != nil {
			t.Fatalf("FileList: %v", err)
		}
	})

	t.Run("Destroy", func(t *testing.T) {
		if err := eng.Destroy(ctx, id); err != nil {
			t.Fatalf("Destroy: %v", err)
		}
		list, err := eng.List(ctx)
		if err != nil {
			t.Fatal(err)
		}
		for _, s := range list {
			if s.ID == id {
				t.Fatalf("sandbox %s still present after Destroy", id)
			}
		}
	})
}
