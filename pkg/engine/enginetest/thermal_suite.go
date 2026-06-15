package enginetest

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/sahil-shubham/bhatti/pkg/engine"
)

// thermalEngine is the optional thermal surface an engine implements. Mirrors
// pkg/server.ThermalEngine but defined locally so this package doesn't depend
// on server. P2 scope is the warm tier (hot↔warm via Pause/EnsureHot); cold is P3.
type thermalEngine interface {
	Pause(ctx context.Context, id string) error
	EnsureHot(ctx context.Context, id string) error
	ThermalState(id string) string
}

// RunThermalSuite asserts the warm-tier contract: ThermalState starts hot,
// Pause moves it to warm, EnsureHot brings it back to hot, and the VM is alive
// across both transitions (an exec round-trips after resume — proves vCPUs
// actually re-entered, not just that the state field flipped).
//
// Cross-arch/cross-VMM target: this same suite must pass on Mac/HVF and on
// Linux/KVM (x86_64 + arm64).
func RunThermalSuite(t *testing.T, newEngine NewEngine) {
	eng := newEngine(t)
	te, ok := eng.(thermalEngine)
	if !ok {
		t.Skip("engine does not implement thermal surface (Pause/EnsureHot/ThermalState)")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	info, err := eng.Create(ctx, engine.SandboxSpec{Name: "thermal", CPUs: 1, MemoryMB: 512})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	id := info.ID
	t.Cleanup(func() { eng.Destroy(context.Background(), id) })

	if got := te.ThermalState(id); got != "hot" {
		t.Fatalf("post-Create ThermalState = %q, want hot", got)
	}

	// Sanity: a hot VM execs and returns stdout.
	if r, err := eng.Exec(ctx, id, []string{"echo", "pre-pause"}); err != nil {
		t.Fatalf("hot Exec(echo): %v", err)
	} else if !strings.Contains(r.Stdout, "pre-pause") {
		t.Fatalf("hot Exec stdout = %q, want pre-pause", r.Stdout)
	}

	t.Run("Pause", func(t *testing.T) {
		if err := te.Pause(ctx, id); err != nil {
			t.Fatalf("Pause: %v", err)
		}
		if got := te.ThermalState(id); got != "warm" {
			t.Fatalf("post-Pause ThermalState = %q, want warm", got)
		}
		// Idempotency: Pause on warm is a no-op.
		if err := te.Pause(ctx, id); err != nil {
			t.Fatalf("Pause (idempotent): %v", err)
		}
	})

	t.Run("EnsureHot", func(t *testing.T) {
		if err := te.EnsureHot(ctx, id); err != nil {
			t.Fatalf("EnsureHot: %v", err)
		}
		if got := te.ThermalState(id); got != "hot" {
			t.Fatalf("post-EnsureHot ThermalState = %q, want hot", got)
		}
		// Idempotency: EnsureHot on hot is a no-op.
		if err := te.EnsureHot(ctx, id); err != nil {
			t.Fatalf("EnsureHot (idempotent): %v", err)
		}
	})

	t.Run("ExecAfterResume", func(t *testing.T) {
		// The real proof — the vCPU actually resumed and the guest is alive.
		r, err := eng.Exec(ctx, id, []string{"echo", "post-resume"})
		if err != nil {
			t.Fatalf("Exec after resume: %v", err)
		}
		if !strings.Contains(r.Stdout, "post-resume") {
			t.Fatalf("post-resume stdout = %q, want post-resume", r.Stdout)
		}
	})

	t.Run("PauseResumeCycle", func(t *testing.T) {
		// Cycle a few times — catches accidental drift in the state machine.
		for i := 0; i < 3; i++ {
			if err := te.Pause(ctx, id); err != nil {
				t.Fatalf("cycle %d Pause: %v", i, err)
			}
			if err := te.EnsureHot(ctx, id); err != nil {
				t.Fatalf("cycle %d EnsureHot: %v", i, err)
			}
		}
		if r, err := eng.Exec(ctx, id, []string{"true"}); err != nil {
			t.Fatalf("Exec after cycles: %v", err)
		} else if r.ExitCode != 0 {
			t.Fatalf("Exec after cycles exit = %d, want 0", r.ExitCode)
		}
	})
}
