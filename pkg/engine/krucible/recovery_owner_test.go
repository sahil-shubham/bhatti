package krucible

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func writeOwnerlessLegacyVM(t *testing.T, e *Engine, id string) string {
	t.Helper()
	specPath, ctl := sandboxPaths(t, e, id)
	dir, socks := filepath.Dir(specPath), filepath.Dir(ctl)
	root := filepath.Join(dir, "root.qcow2")
	if err := createQcow2OverlayOn(root, e.cfg.BaseImage, "raw", 1<<20); err != nil {
		t.Fatal(err)
	}
	stripLegacyVMBackingFormat(t, root)
	var volumes []map[string]any
	for i, name := range []string{"data", "readonly"} {
		path := filepath.Join(dir, name+".img")
		writeFile(t, path, bytes.Repeat([]byte(id+" "+name), 512))
		volumes = append(volumes, map[string]any{
			"block_id": name, "path": path, "format": "raw", "read_only": i == 1,
		})
	}
	writeFile(t, filepath.Join(dir, "config.json"), []byte(`{"token":"legacy-token"}`))
	// v2.5.10 had neither persisted engine ownership nor typed disk authority.
	record := map[string]any{
		"id": id, "name": id, "sandbox_dir": dir, "sock_dir": socks,
		"control_uds": filepath.Join(socks, "c.sock"), "forward_uds": filepath.Join(socks, "f.sock"),
		"ctl_sock_uds": ctl, "mem_mib": 64, "thermal": "cold", "status": "stopped",
		"token": "legacy-token", "log_path": filepath.Join(dir, "vmm.log"),
		"net_policy": noNetworkSpec("").NetPolicy,
		"base_spec": map[string]any{
			"root_disk": root, "root_disk_format": "qcow2", "vcpus": 1, "mem_mib": 64,
			"pid1": true, "exec_path": "/init.krun", "vsock_control_uds": filepath.Join(socks, "c.sock"),
			"vsock_forward_uds": filepath.Join(socks, "f.sock"), "control_socket_uds": ctl, "volumes": volumes,
		},
	}
	data, err := json.MarshalIndent(record, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(data, []byte("user_id")) || bytes.Contains(data, []byte("disk_backing")) {
		t.Fatal("historical record unexpectedly contains ownership or typed disk authority")
	}
	writeFile(t, stateFilePath(dir), data)
	return dir
}

func recoverOwnerTestVM(t *testing.T) (*Engine, *VM) {
	t.Helper()
	e := fakeReseedEngine(t, "supported")
	writeOwnerlessLegacyVM(t, e, "historical")
	if err := e.recover(); err != nil {
		t.Fatal(err)
	}
	vm, err := e.getVM("historical")
	if err != nil {
		t.Fatal(err)
	}
	vm.mu.Lock()
	rec := vm.toRecordLocked()
	vm.mu.Unlock()
	if rec.UserID != "" || rec.BaseSpec.Backing != nil || rec.BaseSpec.Volumes[0].Backing != nil {
		t.Fatalf("recovery changed the historical record: %+v", rec)
	}
	return e, vm
}

func TestRecoverOwnerPersistsAcrossRecovery(t *testing.T) {
	e, vm := recoverOwnerTestVM(t)
	vm.mu.Lock()
	want := vm.toRecordLocked()
	vm.mu.Unlock()
	want.UserID = "owner-a"
	if err := e.RecoverOwner(vm.ID, want.UserID); err != nil {
		t.Fatal(err)
	}
	vm.mu.Lock()
	got := vm.toRecordLocked()
	vm.mu.Unlock()
	if !reflect.DeepEqual(got, want) || !reflect.DeepEqual(readRecord(t, vm.SandboxDir), want) {
		t.Fatalf("owner repair changed more than ownership or was not persisted: %+v", got)
	}
	reopened := &Engine{cfg: e.cfg, vms: map[string]*VM{}}
	if err := reopened.recover(); err != nil {
		t.Fatal(err)
	}
	recovered, err := reopened.getVM(vm.ID)
	if err != nil {
		t.Fatal(err)
	}
	recovered.mu.Lock()
	got = recovered.toRecordLocked()
	recovered.mu.Unlock()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("reopening lost the repaired owner: %+v", got)
	}
}

func assertRecoverOwnerUnchanged(t *testing.T, e *Engine, vm *VM, owner string, wantError bool) {
	t.Helper()
	path := stateFilePath(vm.SandboxDir)
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	beforeInfo, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	vm.mu.Lock()
	want := vm.toRecordLocked()
	vm.mu.Unlock()
	if err := e.RecoverOwner(vm.ID, owner); (err != nil) != wantError {
		t.Fatalf("RecoverOwner(%q): %v, want error %t", owner, err, wantError)
	}
	vm.mu.Lock()
	got := vm.toRecordLocked()
	vm.mu.Unlock()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("existing ownership changed in memory: %+v, want %+v", got, want)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	afterInfo, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	// writeRecord atomically replaces the inode even when its bytes match.
	if !bytes.Equal(after, before) || !os.SameFile(beforeInfo, afterInfo) {
		t.Fatal("existing owner's durable record was rewritten")
	}
}

func TestRecoverOwnerSameOwnerDoesNotRewriteRecord(t *testing.T) {
	for _, owner := range []string{"", "owner-a"} {
		t.Run("owner="+owner, func(t *testing.T) {
			e, vm := recoverOwnerTestVM(t)
			if err := e.RecoverOwner(vm.ID, owner); err != nil {
				t.Fatal(err)
			}
			assertRecoverOwnerUnchanged(t, e, vm, owner, false)
		})
	}
}

func TestRecoverOwnerRejectsDifferentOwnerWithoutChanges(t *testing.T) {
	e, vm := recoverOwnerTestVM(t)
	if err := e.RecoverOwner(vm.ID, "owner-a"); err != nil {
		t.Fatal(err)
	}
	for _, owner := range []string{"owner-b", ""} {
		assertRecoverOwnerUnchanged(t, e, vm, owner, true)
	}
}

func TestRecoverOwnerRejectsUnknownSandbox(t *testing.T) {
	if err := (&Engine{}).RecoverOwner("missing", "owner-a"); err == nil {
		t.Fatal("missing sandbox accepted an owner")
	}
}
