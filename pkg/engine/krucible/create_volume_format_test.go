package krucible

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/sahil-shubham/bhatti/pkg/engine"
)

func TestCreatePersistentVolumeKeepsGuestBytesRaw(t *testing.T) {
	f := newNodeFixture(t)
	secret := filepath.Join(f.data, "private", "not-a-backing-disk.img")
	writeFile(t, secret, []byte("host-private contents"))
	volume := filepath.Join(f.data, "volumes", "u1", "data.ext4")
	if err := os.MkdirAll(filepath.Dir(volume), 0o700); err != nil {
		t.Fatal(err)
	}
	// A guest owns every byte of a persistent raw disk, including bytes that
	// happen to encode a valid qcow2 header and an unauthorized backing path.
	if err := createQcow2Overlay(volume, secret, 1<<20); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(volume)
	if err != nil {
		t.Fatal(err)
	}
	vmm := filepath.Join(f.data, "fake-vmm")
	script := fmt.Sprintf("#!/bin/sh\nBHATTI_FAKE_OLD_LOHAR=1 exec %q -test.run=^TestOldAgentVMMProcess$ \"$1\"\n", os.Args[0])
	if err := os.WriteFile(vmm, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	e := &Engine{cfg: Config{DataDir: f.data, SocketDir: shortSockDir(t), BaseImage: f.base,
		VMMBinary: vmm, BlockRoot: true}, confineVMM: true, vms: map[string]*VM{}}
	t.Cleanup(func() {
		for id := range e.vms {
			_ = e.Destroy(context.Background(), id)
		}
	})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	info, err := e.Create(ctx, engine.SandboxSpec{Name: "raw-volume", UserID: "u1",
		ResolvedVolumes: []engine.ResolvedVolume{{FilePath: volume, Mount: "/data"}}})
	if err != nil {
		t.Fatalf("create with guest-controlled raw bytes: %v", err)
	}
	vm := e.vms[info.ID]
	data, err := os.ReadFile(vm.specPath())
	if err != nil {
		t.Fatal(err)
	}
	var helper VMSpec
	if err := json.Unmarshal(data, &helper); err != nil {
		t.Fatal(err)
	}
	for name, spec := range map[string]VMSpec{"daemon": vm.baseSpec, "helper": helper} {
		if len(spec.Volumes) != 1 || spec.Volumes[0].Format != "raw" {
			t.Fatalf("%s attached guest bytes as a format: %+v", name, spec.Volumes)
		}
		raw, err := json.Marshal(spec)
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Contains(raw, []byte(secret)) {
			t.Fatalf("%s spec granted the raw disk's forged backing: %s", name, raw)
		}
	}
	policy, err := os.ReadFile(filepath.Join(vm.SandboxDir, "vmm.policy"))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(policy, []byte(secret)) {
		t.Fatalf("helper policy granted the raw disk's forged backing: %q", policy)
	}
	after, err := os.ReadFile(volume)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatalf("attach changed raw disk bytes: %v", err)
	}
}
