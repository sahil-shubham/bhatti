//go:build krucible

package krucible

import (
	"bytes"
	"context"
	"os"
	"strings"
	"testing"

	"github.com/sahil-shubham/bhatti/pkg/agent"
	"github.com/sahil-shubham/bhatti/pkg/engine"
)

// TestKrucibleConfigOverVsock is the §3.4 real-usage gate. It proves the flows
// that matter once the on-disk config drive is gone:
//
//  1. create SUCCEEDS with mke2fs unavailable — the exact macOS 500 ("mke2fs:
//     executable file not found") the config drive caused, and its fix;
//  2. env from the config reaches the guest over the config vsock (lohar
//     materialises it at /run/bhatti/config-env);
//  3. an injected file is written in the guest;
//  4. the per-sandbox token is delivered and enforced — a wrong-token agent
//     client is rejected, a correct-token one works. (A failed fetch would leave
//     lohar in no-auth mode and accept the wrong token, so this also proves the
//     config — hence the token — actually arrived.)
//
// Assertions read guest state over the AGENT (FileRead / auth), not via guest
// shell commands, so they hold on the bare block-root test rootfs.
func TestKrucibleConfigOverVsock(t *testing.T) {
	eng := newBlockRootEngine(t) // skips w/o libkrun / hypervisor / mke2fs
	ctx := context.Background()

	// Warm up once (mke2fs available) so the shared base ext4 image is built and
	// cached — a test-harness concern, distinct from per-create config handling.
	warm, err := eng.Create(ctx, engine.SandboxSpec{Name: "warmup", CPUs: 1, MemoryMB: 512})
	if err != nil {
		t.Fatalf("warmup create: %v", err)
	}
	_ = eng.Destroy(ctx, warm.ID)

	// (1) Make mke2fs UNAVAILABLE for the real create — reproducing the launchd
	// environment that 500'd. Pre-cutover Create built config.ext4 via mke2fs and
	// fails here; post-cutover it writes config.json + serves it over vsock.
	origPath := os.Getenv("PATH")
	os.Setenv("PATH", "/nonexistent")
	info, err := eng.Create(ctx, engine.SandboxSpec{
		Name:     "cfgvsock",
		CPUs:     1,
		MemoryMB: 512,
		Env:      map[string]string{"FOO": "bar", "SECRET_KEY": "sk-live-xyz"},
		Files: map[string]engine.FileSpec{
			"/etc/injected.conf": {Content: []byte("hello-from-config"), Mode: "0644"},
		},
	})
	os.Setenv("PATH", origPath)
	if err != nil {
		t.Fatalf("Create failed with mke2fs unavailable — the §3.4 config-drive removal regressed: %v", err)
	}
	id := info.ID
	t.Cleanup(func() { eng.Destroy(context.Background(), id) })

	e := eng.(*Engine)
	e.mu.Lock()
	vm := e.vms[id]
	e.mu.Unlock()
	if vm == nil {
		t.Fatal("vm not found in engine map")
	}

	readGuest := func(path string) string {
		t.Helper()
		var buf bytes.Buffer
		if _, _, err := vm.Agent.FileRead(ctx, path, &buf); err != nil {
			t.Fatalf("FileRead %s: %v", path, err)
		}
		return buf.String()
	}

	// (2) env delivered over vsock (lohar writes configEnv to config-env).
	if env := readGuest("/run/bhatti/config-env"); !strings.Contains(env, "FOO=bar") || !strings.Contains(env, "SECRET_KEY=sk-live-xyz") {
		t.Errorf("config-env = %q, want it to contain FOO=bar and SECRET_KEY=sk-live-xyz", env)
	}
	// (3) injected file materialised.
	if got := readGuest("/etc/injected.conf"); !strings.Contains(got, "hello-from-config") {
		t.Errorf("/etc/injected.conf = %q, want hello-from-config", got)
	}

	// (4) token delivered + enforced.
	if vm.Token == "" {
		t.Fatal("vm token is empty — no auth would be enforced")
	}
	bad := agent.NewKrucibleClient(vm.ControlUDS, vm.ForwardUDS, "wrong-"+vm.Token)
	if _, err := bad.Exec(ctx, []string{"true"}, nil, ""); err == nil {
		t.Error("exec with a WRONG token succeeded — the vsock-delivered token is not enforced (fetch likely failed → lohar in no-auth mode)")
	}
	good := agent.NewKrucibleClient(vm.ControlUDS, vm.ForwardUDS, vm.Token)
	if _, err := good.Exec(ctx, []string{"true"}, nil, ""); err != nil {
		t.Errorf("exec with the CORRECT token failed: %v", err)
	}
}
