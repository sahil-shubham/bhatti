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
	"runtime"
	"strconv"
	"unsafe"

	"github.com/sahil-shubham/bhatti/pkg/engine/krucible"
)

// defaultExtCmdline mirrors libkrun's bundled block-root cmdline for the
// external-kernel path (we supply it ourselves since libkrun won't auto-build
// one). x86 keeps clocksource=kvm-clock (the warm-clock freeze rewinds it);
// arm64 omits it (the arch timer is the clocksource there).
func defaultExtCmdline(initPath string) string {
	if initPath == "" {
		initPath = "/init.krun"
	}
	cmd := "reboot=k panic=-1 panic_print=0 nomodule console=hvc0 " +
		"root=/dev/vda rootfstype=ext4 rw quiet no-kvmapf"
	if runtime.GOARCH == "amd64" {
		cmd += " clocksource=kvm-clock"
	}
	return cmd + " init=" + initPath
}

func fail(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "vmm: "+format+"\n", args...)
	os.Exit(1)
}

func main() {
	if len(os.Args) < 2 {
		fail("usage: vmm <spec.json>  |  vmm create-overlay <overlay> <backing> <size_bytes>")
	}
	// Storage create primitive: the daemon (pure Go, never links libkrun) shells
	// to this to provision a qcow2 CoW overlay via libkrun/imago.
	if os.Args[1] == "create-overlay" {
		createOverlay(os.Args[2:])
		return
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

// createOverlay provisions a qcow2 copy-on-write overlay at args[0] backed by
// the raw image at args[1] (of args[2] bytes), via krun_create_disk_overlay
// (imago). Instant + host-FS-independent. Exits 0 on success.
func createOverlay(args []string) {
	if len(args) != 3 {
		fail("usage: vmm create-overlay <overlay> <backing> <size_bytes>")
	}
	size, err := strconv.ParseUint(args[2], 10, 64)
	if err != nil {
		fail("create-overlay: bad size %q: %v", args[2], err)
	}
	cOverlay := C.CString(args[0])
	defer C.free(unsafe.Pointer(cOverlay))
	cBacking := C.CString(args[1])
	defer C.free(unsafe.Pointer(cBacking))
	if r := C.krun_create_disk_overlay(cOverlay, cBacking, C.uint64_t(size)); r != 0 {
		fail("krun_create_disk_overlay: %d", int(r))
	}
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

	// External kernel: load our own (lean) kernel instead of libkrunfw's bundle.
	// Setting it makes krun_start_enter skip libkrunfw entirely (it only loads
	// krunfw when external_kernel + kernel_bundle are both unset). We supply the
	// full cmdline (root=/dev/vda + init=ExecPath), so the bundled implicit-init
	// and set_exec calls are skipped below.
	externalKernel := spec.KernelImage != ""
	if externalKernel {
		ckernel := C.CString(spec.KernelImage)
		defer C.free(unsafe.Pointer(ckernel))
		cmdline := spec.KernelCmdline
		if cmdline == "" {
			cmdline = defaultExtCmdline(spec.ExecPath)
		}
		ccmd := C.CString(cmdline)
		defer C.free(unsafe.Pointer(ccmd))
		// arm64 = raw Image (0); x86 = ELF vmlinux (1).
		format := C.uint32_t(0)
		if runtime.GOARCH == "amd64" {
			format = C.uint32_t(1)
		}
		if r := C.krun_set_kernel(cid, ckernel, format, nil, ccmd); r != 0 {
			fail("krun_set_kernel: %d", int(r))
		}
	}

	// PID-1 mode: stop libkrun injecting /init.krun so the rootfs's own
	// /init.krun (= lohar) boots as PID 1. Must precede krun_set_root. Only for
	// the bundled kernel — the external kernel boots init= from the cmdline.
	if spec.Pid1 && !externalKernel {
		if r := C.krun_disable_implicit_init(cid); r != 0 {
			fail("krun_disable_implicit_init: %d", int(r))
		}
	}

	// Root: a block image (raw ext4, or a qcow2 CoW overlay — the Phase-0
	// substrate spike) or a virtio-fs host dir (warm/dev fast path). qcow2 uses
	// krun_set_root_disk2 so it is still the *designated root* (kernel cmdline
	// gets root=/dev/vda) — unlike krun_add_disk2, which only adds a general
	// partition. The guest still sees a raw ext4 at /dev/vda; libkrun (imago)
	// translates qcow2 host-side.
	if spec.RootDisk != "" {
		cdisk := C.CString(spec.RootDisk)
		defer C.free(unsafe.Pointer(cdisk))
		if spec.RootDiskFormat == "qcow2" {
			// C.uint32_t(1) == KRUN_DISK_FORMAT_QCOW2
			if r := C.krun_set_root_disk2(cid, cdisk, C.uint32_t(1)); r != 0 {
				fail("krun_set_root_disk2(qcow2): %d", int(r))
			}
		} else if r := C.krun_set_root_disk(cid, cdisk); r != 0 {
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
	// volumes). Pairs with the root disk (root=/dev/vda). lohar mounts it
	// MS_RDONLY, so the RW device is harmless.
	if spec.ConfigDrive != "" {
		cconf := C.CString(spec.ConfigDrive)
		defer C.free(unsafe.Pointer(cconf))
		if r := C.krun_set_data_disk(cid, cconf); r != 0 {
			fail("krun_set_data_disk: %d", int(r))
		}
	}

	// virtio-fs --mount binds: expose host dirs to the guest, live + shared +
	// bidirectional. lohar mounts each tag at its guest path (from the config
	// drive). shm_size=0 → no DAX window (standard FUSE-over-virtio). Boot-time
	// only — the device set is fixed once the VM starts.
	for _, m := range spec.Mounts {
		ctag := C.CString(m.Tag)
		cpath := C.CString(m.HostPath)
		r := C.krun_add_virtiofs3(cid, ctag, cpath, C.uint64_t(0), C._Bool(m.ReadOnly))
		C.free(unsafe.Pointer(ctag))
		C.free(unsafe.Pointer(cpath))
		if r != 0 {
			fail("krun_add_virtiofs3(%s): %d", m.Tag, int(r))
		}
	}

	// Data volumes: block disks attached AFTER root (vda) + config (vdb), so they
	// enumerate as /dev/vdc+ in order. krun_add_disk2 composes with the root/data
	// setters (get_block_cfg). lohar mounts each at its guest path (config drive).
	for _, v := range spec.Volumes {
		cbid := C.CString(v.BlockID)
		cpath := C.CString(v.Path)
		format := C.uint32_t(0) // KRUN_DISK_FORMAT_RAW
		if v.Format == "qcow2" {
			format = C.uint32_t(1) // KRUN_DISK_FORMAT_QCOW2
		}
		r := C.krun_add_disk2(cid, cbid, cpath, format, C._Bool(v.ReadOnly))
		C.free(unsafe.Pointer(cbid))
		C.free(unsafe.Pointer(cpath))
		if r != 0 {
			fail("krun_add_disk2(%s): %d", v.BlockID, int(r))
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
	// by lohar. We still set it (with env) for parity / non-PID1 use. Skipped
	// for the external kernel, which carries init= in the cmdline.
	if !externalKernel {
		cexec := C.CString(spec.ExecPath)
		defer C.free(unsafe.Pointer(cexec))
		argv := cStrArray(nil)
		envp := cStrArray(spec.Env)
		if r := C.krun_set_exec(cid, cexec, argv, envp); r != 0 {
			fail("krun_set_exec: %d", int(r))
		}
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
