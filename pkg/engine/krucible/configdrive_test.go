//go:build krucible

package krucible

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/sahil-shubham/bhatti/pkg/agent"
	"github.com/sahil-shubham/bhatti/pkg/engine"
)

// TestKrucibleConfigDrive boots a REAL block-root sandbox with a config drive
// (env + a file + an auth token) and verifies, end to end with no mocking:
//   - env from the config drive reaches an exec'd process (drive → lohar
//     configEnv → exec env merge);
//   - a file from the config drive is materialized in the guest filesystem;
//   - the per-sandbox token is enforced (a wrong-token client is rejected,
//     while the engine's own client — using the right token — works).
func TestKrucibleConfigDrive(t *testing.T) {
	eng := newBlockRootEngine(t) // skips if libkrun/vmm/mke2fs unavailable
	e := eng.(*Engine)

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	info, err := eng.Create(ctx, engine.SandboxSpec{
		Name:     "cfgdrive",
		CPUs:     1,
		MemoryMB: 512,
		Env:      map[string]string{"FOO": "barbaz", "AGENT_ID": "agent-7"},
		Files: map[string]engine.FileSpec{
			"/etc/greeting": {Content: []byte("hello-from-config-drive"), Mode: "0644"},
		},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	id := info.ID
	t.Cleanup(func() { eng.Destroy(context.Background(), id) })

	t.Run("EnvInjected", func(t *testing.T) {
		r, err := eng.Exec(ctx, id, []string{"printenv", "FOO"})
		if err != nil || strings.TrimSpace(r.Stdout) != "barbaz" {
			t.Fatalf("printenv FOO: err=%v out=%q (config-drive env not in exec)", err, r.Stdout)
		}
		r, err = eng.Exec(ctx, id, []string{"printenv", "AGENT_ID"})
		if err != nil || strings.TrimSpace(r.Stdout) != "agent-7" {
			t.Fatalf("printenv AGENT_ID: err=%v out=%q", err, r.Stdout)
		}
	})

	t.Run("FileInjected", func(t *testing.T) {
		var buf bytes.Buffer
		if _, _, err := e.FileRead(ctx, id, "/etc/greeting", &buf); err != nil {
			t.Fatalf("FileRead /etc/greeting: %v", err)
		}
		if got := strings.TrimSpace(buf.String()); got != "hello-from-config-drive" {
			t.Fatalf("/etc/greeting = %q, want hello-from-config-drive", got)
		}
	})

	t.Run("TokenEnforced", func(t *testing.T) {
		e.mu.RLock()
		vm := e.vms[id]
		e.mu.RUnlock()
		if vm == nil || vm.Token == "" {
			t.Fatal("expected a non-empty per-sandbox token on the block-root path")
		}
		// A client presenting the WRONG token must be rejected by lohar.
		bad := agent.NewKrucibleClient(vm.ControlUDS, vm.ForwardUDS, "deadbeef-wrong-token")
		if _, err := bad.Activity(ctx); err == nil {
			t.Fatal("wrong-token client was NOT rejected (token not enforced)")
		}
		// Sanity: the engine's own client (right token) still works.
		if _, err := eng.Exec(ctx, id, []string{"echo", "ok"}); err != nil {
			t.Fatalf("exec with the correct token failed: %v", err)
		}
	})
}
