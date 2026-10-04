//go:build krucible

package krucible

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/sahil-shubham/bhatti/pkg/gateway"
)

// These tests exercise the recovery LOGIC with no VM/libkrun, so they run on
// every OS/arch (macOS, linux/arm64, linux/amd64) to prove portability.

func TestPidAlive(t *testing.T) {
	if !pidAlive(os.Getpid()) {
		t.Error("pidAlive(self) = false, want true")
	}
	// A very high pid is almost certainly free.
	if pidAlive(0) || pidAlive(-1) {
		t.Error("pidAlive(0/-1) should be false")
	}
	if pidAlive(2 << 30) {
		t.Error("pidAlive(huge pid) = true, want false")
	}
}

// TestStateRoundTrip persists a VM record to <sandboxDir>/state.json and reads
// it back — the durable contract recovery depends on, with no VM.
func TestStateRoundTrip(t *testing.T) {
	dir := t.TempDir()
	orig := &VM{
		ID: "abc123", Name: "demo", UserID: "u1",
		SandboxDir: dir, SockDir: "/tmp/s", ControlUDS: "/tmp/s/c.sock",
		ForwardUDS: "/tmp/s/f.sock", CtlSockUDS: "/tmp/s/k.sock",
		MemMiB: 512, Thermal: "warm", Status: "running", Token: "tok-xyz",
		logPath: filepath.Join(dir, "vmm.log"), HelperPID: 4242,
		baseSpec: VMSpec{
			RootDisk: filepath.Join(dir, "root.img"), VsockConfigUDS: "/tmp/s/cfg.sock",
			Vcpus: 1, MemMiB: 512, Pid1: true, ExecPath: "/init.krun",
		},
		netPolicy: &gateway.NetPolicyWire{Default: "deny", AllowHosts: []string{"api.example.com"}, AllowCIDRs: []string{"1.1.1.1/32"}},
	}
	orig.persist()

	data, err := os.ReadFile(stateFilePath(dir))
	if err != nil {
		t.Fatalf("state.json not written: %v", err)
	}
	var rec vmRecord
	if err := json.Unmarshal(data, &rec); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	got := vmFromRecord(rec)

	checks := map[string][2]any{
		"ID":             {orig.ID, got.ID},
		"Name":           {orig.Name, got.Name},
		"UserID":         {orig.UserID, got.UserID},
		"ControlUDS":     {orig.ControlUDS, got.ControlUDS},
		"Token":          {orig.Token, got.Token},
		"Thermal":        {orig.Thermal, got.Thermal},
		"Status":         {orig.Status, got.Status},
		"HelperPID":      {orig.HelperPID, got.HelperPID},
		"MemMiB":         {orig.MemMiB, got.MemMiB},
		"logPath":        {orig.logPath, got.logPath},
		"RootDisk":       {orig.baseSpec.RootDisk, got.baseSpec.RootDisk},
		"VsockConfigUDS": {orig.baseSpec.VsockConfigUDS, got.baseSpec.VsockConfigUDS},
		"ExecPath":       {orig.baseSpec.ExecPath, got.baseSpec.ExecPath},
	}
	for field, pair := range checks {
		if pair[0] != pair[1] {
			t.Errorf("%s: round-trip got %v, want %v", field, pair[1], pair[0])
		}
	}
	// The engine reboots a sandbox under its egress policy after a restart only
	// if state.json kept it: nil would push netd's public default.
	if !reflect.DeepEqual(got.netPolicy, orig.netPolicy) {
		t.Errorf("netPolicy: round-trip got %+v, want %+v", got.netPolicy, orig.netPolicy)
	}
}
