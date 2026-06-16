//go:build krucible

// Command vmm is bhatti's per-VM libkrun helper.
//
// It links libkrun (the only bhatti component that does), reads a VMSpec, and
// calls krun_start_enter — at which point THIS PROCESS BECOMES THE VM and never
// returns (libkrun exit()s it with the workload's code when the guest shuts
// down). The bhatti daemon spawns one of these per sandbox and controls it
// out-of-band: the agent (lohar) over the bridged vsock UDS, and lifecycle via
// the shutdown eventfd / control socket (P2+).
//
// This is the proven S0 spike (originally C), promoted into bhatti in Go+cgo.
//
// Build: `make vmm` — cgo + libkrun via pkg-config; on macOS codesigned with
// the com.apple.security.hypervisor entitlement (required for HVF). At runtime
// libkrun dlopen()s libkrunfw by name, so the spawner must set
// DYLD_FALLBACK_LIBRARY_PATH to libkrun's lib dir (the krucible engine does).
package main

/*
#cgo pkg-config: libkrun
#include <stdlib.h>
#include <libkrun.h>
*/
import "C"

import (
	"encoding/json"
	"fmt"
	"os"
	"unsafe"

	"github.com/sahil-shubham/bhatti/pkg/engine/krucible"
)

func fail(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "vmm: "+format+"\n", args...)
	os.Exit(1)
}

func main() {
	if len(os.Args) < 2 {
		fail("usage: vmm <spec.json>")
	}
	data, err := os.ReadFile(os.Args[1])
	if err != nil {
		fail("read spec %q: %v", os.Args[1], err)
	}
	var spec krucible.VMSpec
	if err := json.Unmarshal(data, &spec); err != nil {
		fail("parse spec: %v", err)
	}
	run(spec)
	fail("krun_start_enter returned — boot failed") // only reached on error
}

func run(spec krucible.VMSpec) {
	C.krun_set_log_level(C.uint32_t(spec.LogLevel))

	ctx := C.krun_create_ctx()
	if ctx < 0 {
		fail("krun_create_ctx: %d", int(ctx))
	}
	cid := C.uint32_t(ctx)

	if r := C.krun_set_vm_config(cid, C.uint8_t(spec.Vcpus), C.uint32_t(spec.MemMiB)); r != 0 {
		fail("krun_set_vm_config: %d", int(r))
	}

	// PID-1 mode: stop libkrun injecting /init.krun so the rootfs's own
	// /init.krun (= lohar) boots as PID 1. Must precede krun_set_root.
	if spec.Pid1 {
		if r := C.krun_disable_implicit_init(cid); r != 0 {
			fail("krun_disable_implicit_init: %d", int(r))
		}
	}

	// Root: a raw ext4 block image (cold/fork tier — snapshot-friendly, no FUSE
	// inode map) or a virtio-fs host dir (warm/dev fast path).
	if spec.RootDisk != "" {
		cdisk := C.CString(spec.RootDisk)
		defer C.free(unsafe.Pointer(cdisk))
		if r := C.krun_set_root_disk(cid, cdisk); r != 0 {
			fail("krun_set_root_disk: %d", int(r))
		}
	} else {
		croot := C.CString(spec.RootfsDir)
		defer C.free(unsafe.Pointer(croot))
		if r := C.krun_set_root(cid, croot); r != 0 {
			fail("krun_set_root: %d", int(r))
		}
	}

	// Config drive: a RAW ext4 attached as the data disk (/dev/vdb), which lohar
	// mounts read-only at boot to read config.json (hostname, token, env, files,
	// volumes). Pairs with krun_set_root_disk (root=/dev/vda) on the block-root
	// path. lohar mounts it MS_RDONLY, so the RW device is harmless.
	if spec.ConfigDrive != "" {
		cconf := C.CString(spec.ConfigDrive)
		defer C.free(unsafe.Pointer(cconf))
		if r := C.krun_set_data_disk(cid, cconf); r != 0 {
			fail("krun_set_data_disk: %d", int(r))
		}
	}

	// TSI is auto-enabled (no NIC added). Bridge host<->guest vsock ports.
	// listen=true: the host dials the UDS, libkrun forwards to the guest port
	// where lohar listens.
	addVsock := func(port uint32, uds string) {
		if uds == "" {
			return
		}
		c := C.CString(uds)
		defer C.free(unsafe.Pointer(c))
		if r := C.krun_add_vsock_port2(cid, C.uint32_t(port), c, C._Bool(true)); r != 0 {
			fail("krun_add_vsock_port2(%d): %d", port, int(r))
		}
	}
	addVsock(1024, spec.VsockControlUDS)
	addVsock(1025, spec.VsockForwardUDS)

	// Warm-tier control socket (PAUSE/RESUME/STATUS). Optional; skipped when empty.
	if spec.ControlSocketUDS != "" {
		c := C.CString(spec.ControlSocketUDS)
		defer C.free(unsafe.Pointer(c))
		if r := C.krun_set_control_socket(cid, c); r != 0 {
			fail("krun_set_control_socket: %d", int(r))
		}
	}

	// Cold restore: boot from a snapshot bundle instead of cold-booting.
	if spec.SnapshotDir != "" {
		c := C.CString(spec.SnapshotDir)
		defer C.free(unsafe.Pointer(c))
		if r := C.krun_set_snapshot(cid, c); r != 0 {
			fail("krun_set_snapshot: %d", int(r))
		}
	}

	// In PID-1 mode the kernel boots ExecPath directly; KRUN_INIT is ignored
	// by lohar. We still set it (with env) for parity / non-PID1 use.
	cexec := C.CString(spec.ExecPath)
	defer C.free(unsafe.Pointer(cexec))
	argv := cStrArray(nil)
	envp := cStrArray(spec.Env)
	if r := C.krun_set_exec(cid, cexec, argv, envp); r != 0 {
		fail("krun_set_exec: %d", int(r))
	}

	fmt.Fprintf(os.Stderr, "vmm: start_enter pid1=%v vcpus=%d mem=%dMiB rootfs=%s\n",
		spec.Pid1, spec.Vcpus, spec.MemMiB, spec.RootfsDir)
	C.krun_start_enter(cid) // becomes the VM; returns only on error
}

// cStrArray builds a NULL-terminated C array of C strings. Intentionally not
// freed: krun_start_enter never returns, so the process exits with it live.
func cStrArray(ss []string) **C.char {
	n := len(ss)
	ptrSize := unsafe.Sizeof(uintptr(0))
	arr := C.malloc(C.size_t(uintptr(n+1) * ptrSize))
	slice := unsafe.Slice((**C.char)(arr), n+1)
	for i, s := range ss {
		slice[i] = C.CString(s)
	}
	slice[n] = nil
	return (**C.char)(arr)
}
