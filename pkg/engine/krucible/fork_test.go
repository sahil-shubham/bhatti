//go:build krucible

package krucible

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sahil-shubham/bhatti/pkg/engine"
)

// TestKrucibleFork is the Phase-2 fork gate (the agent-swarm primitive behind
// `create --from`): fork a running sandbox into a new one that inherits its
// in-memory state (a tmpfs/RAM marker is present in the fork), then both run
// independently and diverge, with the source undisturbed.
func TestKrucibleFork(t *testing.T) {
	eng := newCheckpointEngine(t)
	ke := eng.(*Engine)
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Second)
	defer cancel()

	src, err := eng.Create(ctx, engine.SandboxSpec{Name: "forksrc", CPUs: 1, MemoryMB: 512})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	t.Cleanup(func() { eng.Destroy(context.Background(), src.ID) })

	// A marker in tmpfs (guest RAM) — the fork must inherit it (memory clone).
	const marker = "fork-ram-5e2"
	if err := ke.FileWrite(ctx, src.ID, "/tmp/forkmark", "0644", int64(len(marker)), strings.NewReader(marker)); err != nil {
		t.Fatalf("FileWrite marker: %v", err)
	}

	fork, err := ke.Fork(ctx, src.ID, "forked")
	if err != nil {
		t.Fatalf("Fork: %v", err)
	}
	t.Cleanup(func() { eng.Destroy(context.Background(), fork.ID) })

	// The fork inherited the source's in-memory state.
	var buf bytes.Buffer
	if _, _, err := ke.FileRead(ctx, fork.ID, "/tmp/forkmark", &buf); err != nil {
		t.Fatalf("FileRead marker in fork: %v", err)
	}
	if got := strings.TrimSpace(buf.String()); got != marker {
		t.Fatalf("fork tmpfs marker = %q, want %q (not a memory clone)", got, marker)
	}

	// Independent + diverging: a write to one must not appear in the other.
	if err := ke.FileWrite(ctx, src.ID, "/tmp/who", "0644", 3, strings.NewReader("src")); err != nil {
		t.Fatalf("write src marker: %v", err)
	}
	if err := ke.FileWrite(ctx, fork.ID, "/tmp/who", "0644", 4, strings.NewReader("fork")); err != nil {
		t.Fatalf("write fork marker: %v", err)
	}
	readWho := func(id string) string {
		t.Helper()
		var b bytes.Buffer
		if _, _, err := ke.FileRead(ctx, id, "/tmp/who", &b); err != nil {
			t.Fatalf("read who %s: %v", id, err)
		}
		return strings.TrimSpace(b.String())
	}
	if got := readWho(src.ID); got != "src" {
		t.Fatalf("source diverged wrong: /tmp/who = %q, want src", got)
	}
	if got := readWho(fork.ID); got != "fork" {
		t.Fatalf("fork diverged wrong: /tmp/who = %q, want fork", got)
	}
}

// TestKrucibleConcurrentFork forks one source three times in parallel. Every
// child must inherit the same frozen RAM yet have its own writable RAM/disk.
func TestKrucibleConcurrentFork(t *testing.T) {
	e := newCheckpointEngine(t).(*Engine)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	src, err := e.Create(ctx, engine.SandboxSpec{Name: "fork-concurrent", CPUs: 1, MemoryMB: 512})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = e.Destroy(context.Background(), src.ID) })
	const marker = "three-independent-children"
	if err := e.FileWrite(ctx, src.ID, "/tmp/fork-original", "0644", int64(len(marker)), strings.NewReader(marker)); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	var children [3]engine.SandboxInfo
	var errs [3]error
	for i := range children {
		wg.Add(1)
		go func() {
			defer wg.Done()
			children[i], errs[i] = e.Fork(ctx, src.ID, fmt.Sprintf("child-%d", i))
		}()
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("Fork %d: %v", i, err)
		}
		t.Cleanup(func() { _ = e.Destroy(context.Background(), children[i].ID) })
	}
	ids := map[string]bool{src.ID: true}
	for i, child := range children {
		if ids[child.ID] {
			t.Fatalf("duplicate fork ID %s", child.ID)
		}
		ids[child.ID] = true
		checkpointMarker(t, e, ctx, child.ID, "/tmp/fork-original", marker)
		value := fmt.Sprintf("child-%d", i)
		for _, path := range []string{"/tmp/diverged", "/workspace/diverged"} {
			if err := e.FileWrite(ctx, child.ID, path, "0644", int64(len(value)), strings.NewReader(value)); err != nil {
				t.Fatalf("write child %d %s: %v", i, path, err)
			}
		}
	}
	for i, child := range children {
		for _, path := range []string{"/tmp/diverged", "/workspace/diverged"} {
			checkpointMarker(t, e, ctx, child.ID, path, fmt.Sprintf("child-%d", i))
		}
	}
	checkpointMarker(t, e, ctx, src.ID, "/tmp/fork-original", marker)
	for _, path := range []string{"/tmp/diverged", "/workspace/diverged"} {
		var b bytes.Buffer
		if _, _, err := e.FileRead(ctx, src.ID, path, &b); err == nil {
			t.Fatalf("source acquired child-only data at %s: %q", path, b.String())
		}
	}
}
