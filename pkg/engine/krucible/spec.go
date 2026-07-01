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
// VMFsMount is one virtio-fs host-dir bind as the VMM sees it: a tag + the host
// directory + read-only flag. The guest-side mount path travels in the config
// drive (configdrive.FsMountConfig), keyed by the same tag.
type VMFsMount struct {
	Tag      string `json:"tag"`
	HostPath string `json:"host_path"`
	ReadOnly bool   `json:"read_only,omitempty"`
}

// VMVolume is one block data disk as the VMM sees it: an id + host image path +
// format ("qcow2" | "raw") + read-only flag. The guest device name + mount path
// travel in the config drive (configdrive.VolumeMountConfig).
type VMVolume struct {
	BlockID  string `json:"block_id"`
	Path     string `json:"path"`
	Format   string `json:"format"`
	ReadOnly bool   `json:"read_only,omitempty"`
}

type VMSpec struct {
	// RootfsDir is a host directory exposed to the guest as the virtiofs root
	// (krun_set_root). For the POC there is no ext4/qcow2 image; qcow2 CoW
	// overlays arrive with snapshot in P3.
	RootfsDir string `json:"rootfs_dir,omitempty"`

	// RootDisk, if set, boots from a raw ext4 block image (krun_set_root_disk)
	// instead of a virtio-fs host dir. This is the cold/fork-tier root: its
	// snapshot state is just queue config (no FUSE inode map), and the image is
	// the portable cold artifact. Mutually exclusive with RootfsDir.
	RootDisk string `json:"root_disk,omitempty"`

	// RootDiskFormat selects how RootDisk is opened: "" / "raw" = a raw ext4
	// image via krun_set_root_disk (today's path); "qcow2" = a qcow2 CoW overlay
	// via krun_add_disk2 (the Phase-0 substrate spike). The add_disk family is
	// mutually exclusive with set_root_disk/set_data_disk, so on the qcow2 path
	// the config drive also moves to krun_add_disk (read-only).
	RootDiskFormat string `json:"root_disk_format,omitempty"`

	// ConfigDrive, if set, is a RAW ext4 image attached as a read-only block
	// device (/dev/vdb) carrying config.json (hostname, token, env, files,
	// volumes). lohar reads it at boot before listening. See pkg/configdrive
	// and lohar's loadConfigDrive.
	ConfigDrive string `json:"config_drive,omitempty"`

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

	// ControlSocketUDS, if set, is the host-side UDS the VMM serves for warm-tier
	// commands (PAUSE/RESUME/STATUS). One newline command in, one line out, then
	// close. See krun_set_control_socket.
	ControlSocketUDS string `json:"control_socket_uds,omitempty"`

	// SnapshotDir, if set, cold-restores the VM from a snapshot bundle
	// (memory.img + checkpoint.bin + manifest.json) instead of cold booting:
	// guest RAM, device, and vCPU state are loaded and the guest resumes from
	// the snapshot point. See krun_set_snapshot. macOS/HVF only.
	SnapshotDir string `json:"snapshot_dir,omitempty"`

	// KernelImage, if set, boots an external kernel (e.g. a lean one) via
	// krun_set_kernel instead of libkrunfw's bundled kernel — setting it makes
	// libkrun skip libkrunfw entirely. Block-root only: the cmdline roots on
	// /dev/vda. arm64 expects a raw `Image`; x86 an ELF vmlinux.
	KernelImage string `json:"kernel_image,omitempty"`

	// Mounts are live virtio-fs host-dir binds (create --mount). The VMM exposes
	// each Tag→HostPath via krun_add_virtiofs3; lohar mounts Tag at its guest path
	// (carried separately in the config drive). Boot-time only (libkrun device set
	// is fixed at boot).
	Mounts []VMFsMount `json:"mounts,omitempty"`

	// Volumes are block data disks attached AFTER the root (vda) + config drive
	// (vdb), so they enumerate as /dev/vdc+ in order. Each is added via
	// krun_add_disk2 (which composes with the root/data setters); the guest device
	// name + mount path travel in the config drive (VolumeMountConfig).
	Volumes []VMVolume `json:"volumes,omitempty"`
	// KernelCmdline overrides the default block-root cmdline used with
	// KernelImage. Empty = the built-in default (root=/dev/vda … init=ExecPath).
	KernelCmdline string `json:"kernel_cmdline,omitempty"`

	// LogLevel is the libkrun log level (0=off .. 5=trace). 2=warn keeps the
	// guest console readable.
	LogLevel uint32 `json:"log_level"`
}
