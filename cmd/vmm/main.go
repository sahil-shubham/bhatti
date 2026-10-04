//go:build krucible

// Command vmm is bhatti's per-VM libkrun helper.
//
// It links libkrun (the only bhatti component that does), reads a VMSpec,
// builds a VMM through libkrun's builder API and runs it: krun_vmm_run never
// returns (libkrun exits this process with the guest's code when the guest shuts
// down). The bhatti daemon spawns one of these per sandbox and talks to the
// guest agent (lohar) over the bridged vsock UDSes.
//
// Build: `make vmm` (cgo + libkrun via pkg-config; on macOS codesigned with the
// com.apple.security.hypervisor entitlement, which HVF requires).
package main

/*
#cgo pkg-config: libkrun
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <libkrun.h>

static bool bv_push_stderr(void *self, KrunStr s) {
	(void)self;
	fwrite(s.data, 1, s.len, stderr);
	return true;
}
static KrunPushStrVtable bv_stderr_vt = { .drop = NULL, .push = bv_push_stderr };

// bv_report prints "vmm: <what>: <libkrun message>" and frees the error.
static void bv_report(const char *what, KrunError err) {
	KrunVtableHandle w = KRUN_VTABLE_HANDLE(KRUN_PUSH_STR_TYPE_TAG, bv_stderr_vt, NULL);
	fprintf(stderr, "vmm: %s: ", what);
	krun_error_message(err, &w);
	fputc('\n', stderr);
	krun_error_destroy(err);
}

static KrunStr bv_str(const char *s) { return KRUN_STR(s); }

// bv_errbuf collects a libkrun error message for a control-socket reply.
struct bv_errbuf { char text[512]; size_t len; };
static bool bv_push_buf(void *self, KrunStr s) {
	struct bv_errbuf *b = self;
	size_t room = sizeof(b->text) - 1 - b->len;
	size_t n = s.len < room ? s.len : room;
	memcpy(b->text + b->len, s.data, n);
	b->len += n;
	b->text[b->len] = 0;
	return true;
}
static KrunPushStrVtable bv_buf_vt = { .drop = NULL, .push = bv_push_buf };

// bv_vm_ctl runs pause (op 0) or resume (op 1) on h. Returns 0 on success;
// otherwise fills msg with libkrun's reason and returns -1.
static int bv_vm_ctl(KrunVmmHandle h, int op, struct bv_errbuf *msg) {
	KrunError err = NULL;
	if (op == 0) krun_vmm_handle_pause(h, &err);
	else krun_vmm_handle_resume(h, &err);
	if (!err) return 0;
	msg->len = 0; msg->text[0] = 0;
	KrunVtableHandle w = KRUN_VTABLE_HANDLE(KRUN_PUSH_STR_TYPE_TAG, bv_buf_vt, msg);
	krun_error_message(err, &w);
	krun_error_destroy(err);
	return -1;
}
*/
import "C"

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"runtime"
	"strings"
	"sync"

	"github.com/sahil-shubham/bhatti/pkg/engine/krucible"
)

// capabilities is what this VMM build supports, reported by `vmm capabilities`
// so the daemon can gate features at startup instead of failing per call.
// Checkpoint (snapshot/restore/fork) arrives with the checkpoint stack on top
// of upstream libkrun (docs/PLAN-libkrun-upstream-rebase.md, Phase 2).
var capabilities = krucible.VMMCapabilities{Pause: true, Checkpoint: false}

// defaultExtCmdline mirrors libkrun's bundled block-root cmdline for the
// external-kernel path. x86 pins clocksource=kvm-clock; arm64 uses the arch timer.
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
	if len(os.Args) != 2 {
		fail("usage: vmm <spec.json> | vmm capabilities")
	}
	if os.Args[1] == "capabilities" {
		if err := json.NewEncoder(os.Stdout).Encode(capabilities); err != nil {
			fail("capabilities: %v", err)
		}
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
	fail("krun_vmm_run returned") // only reached on error
}

// cstr allocates a C string that lives for the rest of the process: libkrun
// copies what it keeps, and the process ends inside krun_vmm_run.
func cstr(s string) C.KrunStr { return C.bv_str(C.CString(s)) }

// noErr exits with libkrun's message if a call reported an error.
func noErr(err C.KrunError, what string) {
	if err != nil {
		c := C.CString(what)
		C.bv_report(c, err)
		os.Exit(1)
	}
}

func run(spec krucible.VMSpec) {
	// Fail closed on features this build doesn't have: the daemon gates them via
	// `vmm capabilities`, so a spec that asks for one is a daemon bug.
	switch {
	case spec.SnapshotDir != "":
		fail("snapshot restore is not supported by this VMM build")
	case spec.KernelImage == "":
		fail("kernel_image is required (this VMM boots an external kernel only)")
	case spec.RootDisk == "":
		fail("root_disk is required (the external kernel boots a block root)")
	}

	var kerr C.KrunError
	C.krun_init_log(C.int(-1), C.uint32_t(spec.LogLevel), C.KRUN_LOG_STYLE_AUTO, 0, &kerr)
	noErr(kerr, "krun_init_log")

	b := C.krun_vmm_builder_new()
	C.krun_vmm_builder_vcpus(&b, C.uint8_t(spec.Vcpus), &kerr)
	noErr(kerr, "vcpus")
	C.krun_vmm_builder_ram_mib(&b, C.uint32_t(spec.MemMiB), &kerr)
	noErr(kerr, "ram_mib")

	cmdline := spec.KernelCmdline
	if cmdline == "" {
		cmdline = defaultExtCmdline(spec.ExecPath)
	}
	// arm64 boots a raw Image, x86 an ELF vmlinux.
	format := C.uint32_t(C.KRUN_KERNEL_FORMAT_RAW)
	if runtime.GOARCH == "amd64" {
		format = C.KRUN_KERNEL_FORMAT_ELF
	}
	payload := C.krun_payload_load_external(cstr(spec.KernelImage), format, C.KrunStr{}, cstr(cmdline), &kerr)
	noErr(kerr, "payload "+spec.KernelImage)
	C.krun_vmm_builder_payload(&b, payload)

	// Devices attach in the order added: the root block device first, so it
	// enumerates as /dev/vda (the cmdline roots on it), data volumes after it.
	devs := C.krun_mmio_device_manager_new()
	addBlock := func(id, path, fmtName string, readOnly bool) {
		f := C.uint32_t(C.KRUN_DISK_FORMAT_RAW)
		if fmtName == "qcow2" {
			f = C.KRUN_DISK_FORMAT_QCOW2
		}
		blk := C.krun_block_device_new(cstr(id), cstr(path), f, &kerr)
		noErr(kerr, "block "+id)
		C.krun_block_device_set_read_only(blk, C._Bool(readOnly))
		C.krun_mmio_device_manager_add(devs, C.KrunAttachDevice(blk))
	}
	addBlock("root", spec.RootDisk, spec.RootDiskFormat, false)
	for _, v := range spec.Volumes {
		addBlock(v.BlockID, v.Path, v.Format, v.ReadOnly)
	}

	// virtio-fs --mount binds; lohar mounts each tag at its guest path.
	for _, m := range spec.Mounts {
		var fs C.KrunFsDevice
		if m.ReadOnly {
			fs = C.krun_fs_device_new_read_only(cstr(m.Tag), cstr(m.HostPath), &kerr)
		} else {
			fs = C.krun_fs_device_new(cstr(m.Tag), cstr(m.HostPath), &kerr)
		}
		noErr(kerr, "virtiofs "+m.Tag)
		C.krun_mmio_device_manager_add(devs, C.KrunAttachDevice(fs))
	}

	// Console on hvc0, wired to our stdio (the daemon captures it to vmm.log).
	cb := C.krun_console_device_builder()
	C.krun_console_builder_add_default_console(cb, 0, 1, 2, &kerr)
	noErr(kerr, "console")
	console := C.krun_console_builder_build(cb, &kerr)
	noErr(kerr, "console build")
	C.krun_mmio_device_manager_add(devs, C.KrunAttachDevice(console))

	// vsock carries only the agent ports; the guest's inet (if any) goes over eth0.
	vsock := C.krun_vsock_device_new(3, 0, &kerr)
	noErr(kerr, "vsock")
	// listen=true: the host dials the UDS and libkrun forwards to the guest port
	// where lohar listens. listen=false (config, 1026): the guest dials out.
	for _, p := range []struct {
		port   uint32
		path   string
		listen bool
	}{
		{1024, spec.VsockControlUDS, true},
		{1025, spec.VsockForwardUDS, true},
		{1026, spec.VsockConfigUDS, false},
	} {
		if p.path != "" {
			C.krun_vsock_device_add_unix_port(vsock, C.uint32_t(p.port), cstr(p.path), C._Bool(p.listen))
		}
	}
	C.krun_mmio_device_manager_add(devs, C.KrunAttachDevice(vsock))

	// virtio-net to the owner's bhatti-netd gateway over a unixstream socket.
	// Absent for an egress-"none" sandbox: no network device at all.
	if spec.NetUDS != "" {
		mac, err := net.ParseMAC(spec.NetMAC)
		if err != nil || len(mac) != 6 {
			fail("bad net_mac %q: %v", spec.NetMAC, err)
		}
		cmac := C.CBytes(mac)
		// CSUM, GUEST_CSUM, GUEST_TSO4, GUEST_UFO, HOST_TSO4, HOST_UFO.
		const features = 1<<0 | 1<<1 | 1<<7 | 1<<10 | 1<<11 | 1<<14
		nic := C.krun_net_device_new_unixstream_path(cstr("eth0"), cstr(spec.NetUDS),
			C.KrunBytes{data: (*C.uint8_t)(cmac), len: 6}, features, 0, &kerr)
		noErr(kerr, "net "+spec.NetUDS)
		C.krun_mmio_device_manager_add(devs, C.KrunAttachDevice(nic))
	}

	C.krun_vmm_builder_devices(&b, devs)
	vmm := C.krun_vmm_builder_build(&b, &kerr)
	noErr(kerr, "build")

	// The handle must be taken before krun_vmm_run, which consumes the VMM.
	if spec.ControlSocketUDS != "" {
		h := C.krun_vmm_handle(vmm, &kerr)
		noErr(kerr, "handle")
		ln, err := net.Listen("unix", spec.ControlSocketUDS)
		if err != nil {
			fail("control socket %s: %v", spec.ControlSocketUDS, err)
		}
		go serveControl(ln, h)
	}

	fmt.Fprintf(os.Stderr, "vmm: run vcpus=%d mem=%dMiB root=%s\n", spec.Vcpus, spec.MemMiB, spec.RootDisk)
	C.krun_vmm_run(vmm) // becomes the VM; returns only on error
}

// serveControl answers the daemon's control commands, one per connection: a
// command line in, one line out ("OK ..." or "ERR <reason>").
//
//	PAUSE   park every vCPU (returns once they're parked); idempotent
//	RESUME  run them again; idempotent
//	STATUS  "OK running" or "OK paused"
func serveControl(ln net.Listener, h C.KrunVmmHandle) {
	var mu sync.Mutex // one command at a time; also guards paused
	paused := false
	for {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		go func() {
			defer conn.Close()
			line, err := bufio.NewReader(conn).ReadString('\n')
			if err != nil {
				return
			}
			mu.Lock()
			defer mu.Unlock()
			reply := "ERR unknown command"
			switch cmd := strings.TrimSpace(line); cmd {
			case "PAUSE", "RESUME":
				op, want := C.int(0), true
				if cmd == "RESUME" {
					op, want = 1, false
				}
				var msg C.struct_bv_errbuf
				if C.bv_vm_ctl(h, op, &msg) == 0 {
					paused = want
					reply = "OK"
				} else {
					reply = "ERR " + C.GoString(&msg.text[0])
				}
			case "STATUS":
				reply = "OK running"
				if paused {
					reply = "OK paused"
				}
			}
			fmt.Fprintln(conn, reply)
		}()
	}
}
