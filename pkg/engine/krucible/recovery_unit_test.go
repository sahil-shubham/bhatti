//go:build krucible

package krucible

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// These tests exercise the recovery LOGIC with no VM/libkrun, so they run on
// every OS/arch (macOS, linux/arm64, linux/amd64) to prove portability.

func TestClassifyRehydrate(t *testing.T) {
	cases := []struct {
		alive, hasBundle bool
		wantStatus       string
		wantThermal      string
	}{
		{alive: true, hasBundle: false, wantStatus: "running", wantThermal: ""},
		{alive: true, hasBundle: true, wantStatus: "running", wantThermal: ""},
		{alive: false, hasBundle: true, wantStatus: "stopped", wantThermal: "cold"},
		{alive: false, hasBundle: false, wantStatus: "stopped", wantThermal: ""},
	}
	for _, c := range cases {
		gotS, gotT := classifyRehydrate(c.alive, c.hasBundle)
		if gotS != c.wantStatus || gotT != c.wantThermal {
			t.Errorf("classifyRehydrate(alive=%v,bundle=%v) = (%q,%q), want (%q,%q)",
				c.alive, c.hasBundle, gotS, gotT, c.wantStatus, c.wantThermal)
		}
	}
}

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

func TestBundleHasCheckpoint(t *testing.T) {
	if bundleHasCheckpoint("") {
		t.Error("empty bundle dir should report no checkpoint")
	}
	dir := t.TempDir()
	if bundleHasCheckpoint(dir) {
		t.Error("empty dir should report no checkpoint")
	}
	os.WriteFile(filepath.Join(dir, "checkpoint.bin"), []byte("x"), 0600)
	if !bundleHasCheckpoint(dir) {
		t.Error("dir with checkpoint.bin should report a checkpoint")
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
		BundleDir: filepath.Join(dir, "bundle"), logPath: filepath.Join(dir, "vmm.log"),
		HelperPID: 4242,
		baseSpec: VMSpec{
			RootDisk: filepath.Join(dir, "root.img"), ConfigDrive: filepath.Join(dir, "config.ext4"),
			Vcpus: 1, MemMiB: 512, Pid1: true, ExecPath: "/init.krun",
		},
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
		"ID":          {orig.ID, got.ID},
		"Name":        {orig.Name, got.Name},
		"UserID":      {orig.UserID, got.UserID},
		"ControlUDS":  {orig.ControlUDS, got.ControlUDS},
		"Token":       {orig.Token, got.Token},
		"Thermal":     {orig.Thermal, got.Thermal},
		"Status":      {orig.Status, got.Status},
		"HelperPID":   {orig.HelperPID, got.HelperPID},
		"MemMiB":      {orig.MemMiB, got.MemMiB},
		"BundleDir":   {orig.BundleDir, got.BundleDir},
		"logPath":     {orig.logPath, got.logPath},
		"RootDisk":    {orig.baseSpec.RootDisk, got.baseSpec.RootDisk},
		"ConfigDrive": {orig.baseSpec.ConfigDrive, got.baseSpec.ConfigDrive},
		"ExecPath":    {orig.baseSpec.ExecPath, got.baseSpec.ExecPath},
	}
	for field, pair := range checks {
		if pair[0] != pair[1] {
			t.Errorf("%s: round-trip got %v, want %v", field, pair[1], pair[0])
		}
	}
}
