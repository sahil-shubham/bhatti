// Package krucible implements engine.Engine on top of libkrun (the krucible
// fork). Unlike the firecracker engine, libkrun is an in-process, blocking
// library: krun_start_enter() turns the calling process INTO the VM and never
// returns. So the daemon never links libkrun — instead it spawns one small
// `bhatti vmm` helper (cmd/vmm) per sandbox, which links libkrun, becomes the
// VM, and is controlled out-of-band (vsock UDS for the agent; shutdown
// eventfd / control socket for lifecycle).
//
// Everything in this package is pure Go and cross-compiles to linux + darwin:
// it spawns the helper and talks to lohar over sockets. Only cmd/vmm needs cgo
// + libkrun + codesigning.
package krucible

// VMSpec is the sealed contract between the daemon (which writes it as JSON)
// and the per-VM helper (cmd/vmm, which reads it and configures libkrun).
//
// Kept deliberately small for P1: virtiofs root + TSI networking + vsock
// bridges, no snapshot/control-socket/egress yet (those land in P2+).
type VMSpec struct {
	// RootfsDir is a host directory exposed to the guest as the virtiofs root
	// (krun_set_root). For the POC there is no ext4/qcow2 image; qcow2 CoW
	// overlays arrive with snapshot in P3.
	RootfsDir string `json:"rootfs_dir"`

	Vcpus  uint8  `json:"vcpus"`
	MemMiB uint32 `json:"mem_mib"`

	// Pid1 disables libkrun's implicit init so ExecPath (lohar, placed at
	// /init.krun in the rootfs) boots as PID 1 — matching FC's
	// init=/usr/local/bin/lohar. Confirmed working in S0.
	Pid1     bool     `json:"pid1"`
	ExecPath string   `json:"exec_path"` // e.g. "/init.krun"
	Env      []string `json:"env,omitempty"`

	// Vsock bridges. listen=true on the libkrun side: the host (daemon) dials
	// these UDS paths; libkrun forwards to the guest vsock port where lohar
	// listens (1024 control, 1025 forward).
	VsockControlUDS string `json:"vsock_control_uds"`
	VsockForwardUDS string `json:"vsock_forward_uds"`

	// LogLevel is the libkrun log level (0=off .. 5=trace). 2=warn keeps the
	// guest console readable.
	LogLevel uint32 `json:"log_level"`
}
