package krucible

import (
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"syscall"
)

// Confined helpers. On Linux every bhatti-vmm runs confined from its first
// instruction (cmd/vmm/internal/confine): no_new_privs, seccomp and a Landlock
// policy (VMMPolicy) naming the only files it may open — its sandbox's root disk,
// volumes, save dir and spec, its socket dir, the images its root disk backs
// onto, the kernel, /dev/kvm, and any virtio-fs mount. When the daemon is root
// it also gives each sandbox's helper an unprivileged uid of its own
// (vmmUIDBase+n, with the same number as primary group): what the helper
// needs is made that uid's, and nothing else is.
//
// A uid per sandbox rather than one shared by every helper: Landlock doesn't
// mediate connect(2) on unix sockets, and only scopes signals from ABI 6, so
// the uid is what keeps one helper away from another's sockets (the boot
// config socket hands out the sandbox's token and secrets; the control socket
// pauses and SAVEs) and from signalling it — and, on a kernel without
// Landlock, from its disks. It costs a hash and a map.
//
// Every helper is also in vmmGroup, which shared read-only things open to:
// the directories on the way to the images (search only) and shared bases.
// An owner's helpers also get its ephemeral netd socket group and its durable
// node group. Private ancestors are daemon-owned and readable only by that
// owner's node group, even when Landlock is unavailable.
//
// The ids come from 0x70000000 (1879048192) up: that shared group, then 2^20
// per-sandbox ids, 2^20 ephemeral netd groups and 2^20 durable node groups —
// above subordinate/container ids, below systemd's foreign range, and named
// by no account.
const (
	vmmGroup      = 0x70000000
	vmmIDSlots    = 1 << 20
	vmmUIDBase    = vmmGroup + 1
	vmmNetGIDBase = vmmUIDBase + vmmIDSlots
)

// What a helper may do where (Landlock rights).
const (
	vmmReadFile = LandlockReadFile
	vmmRWFile   = LandlockReadFile | LandlockWriteFile | LandlockTruncate
	vmmDevice   = LandlockReadFile | LandlockWriteFile | LandlockIoctlDev
	vmmReadTree = LandlockReadFile | LandlockReadDir
	// Its socket dir: bind sockets there (removing stale ones), nothing else —
	// no symlink for the daemon to follow when it dials them.
	vmmSockDir = LandlockMakeSock | LandlockRemoveFile
	// Its save dir: SAVE makes a directory of plain files, writes and renames
	// them, and removes it all if it fails.
	vmmSaveDir = vmmRWFile | LandlockReadDir | LandlockMakeDir | LandlockMakeReg | LandlockRemoveDir | LandlockRemoveFile
	// A read-write virtio-fs mount: whatever a guest does in a directory, bar
	// device nodes.
	vmmMountRW = vmmSaveDir | LandlockMakeSock | LandlockMakeFifo | LandlockMakeSym | LandlockRefer
)

// The capabilities libkrun's virtio-fs server needs to act for the guest on a
// mount, as it did when the helper was root: on a read-write one, create files
// as the guest's uid and gid (it switches euid per request) and chown, chmod
// and set times as the guest's root may; a read-only one only reads. Landlock
// holds their file access to the mount and the sandbox's own files, but not
// connect(2): DAC_OVERRIDE or SETUID reaches any unix socket on the host, as a
// root helper could. A read-write mount stays a privileged feature (the
// server doesn't vet host paths either); without one a helper keeps nothing.
var (
	vmmMountRWCaps = []uint{0, 1, 2, 3, 4, 6, 7} // CHOWN DAC_OVERRIDE DAC_READ_SEARCH FOWNER FSETID SETGID SETUID
	vmmMountROCaps = []uint{2}                   // DAC_READ_SEARCH
)

// initConfinement decides how helpers run and readies what they share; New
// calls it once the data and socket dirs exist. A root daemon refuses to start
// if a helper couldn't reach them, rather than run its helpers as root.
func (e *Engine) initConfinement() error {
	if runtime.GOOS != "linux" {
		return nil
	}
	e.confineVMM = true
	if os.Geteuid() != 0 {
		return nil // the helpers run as the daemon's user, still confined
	}
	e.dropVMM = true
	e.vmmIDs = make(map[uint32]string)
	kvm, err := kvmGroup()
	if err != nil {
		return err
	}
	e.kvmGID = kvm
	// Searchable by the helpers, listable by nobody.
	sandboxes := filepath.Join(e.cfg.DataDir, "sandboxes")
	if err := os.Chown(sandboxes, 0, vmmGroup); err != nil {
		return err
	}
	if err := os.Chmod(sandboxes, 0o710); err != nil {
		return err
	}
	for _, dir := range []string{e.cfg.DataDir, e.cfg.SocketDir} {
		if err := e.vmmReachable(dir); err != nil {
			return fmt.Errorf("krucible: %w", err)
		}
	}
	return nil
}

// kvmGroup is the group to give helpers so they can open /dev/kvm: 0 when
// anyone may.
func kvmGroup() (uint32, error) {
	fi, err := os.Stat("/dev/kvm")
	if err != nil {
		return 0, fmt.Errorf("krucible: %w", err)
	}
	gid := fi.Sys().(*syscall.Stat_t).Gid
	switch m := fi.Mode().Perm(); {
	case m&0o006 == 0o006:
		return 0, nil
	case m&0o060 == 0o060 && gid != 0:
		return gid, nil
	}
	return 0, fmt.Errorf("krucible: bhatti-vmm runs unprivileged, and /dev/kvm (mode %v, group %d) is open to no group or everyone", fi.Mode().Perm(), gid)
}

// owns reports whether path is in the data or socket dir: ours to open to the
// helpers. Anything else they need must already be open to them.
func (e *Engine) owns(path string) bool {
	return within(e.cfg.DataDir, path) || within(e.cfg.SocketDir, path)
}

// within reports whether path is dir or beneath it (both cleaned, absolute).
func within(dir, path string) bool {
	dir, err1 := filepath.Abs(dir)
	path, err2 := filepath.Abs(path)
	return err1 == nil && err2 == nil &&
		(path == dir || strings.HasPrefix(path, dir+string(filepath.Separator)))
}

// shareWithVMMs makes path usable by every helper through "other" permission
// bit other, or through bit group with vmmGroup as its group. A path of ours
// gets vmmGroup and the bit (its owner keeps it); anything else has to have
// one of them already.
func (e *Engine) shareWithVMMs(path string, other, group os.FileMode) error {
	fi, err := os.Stat(path)
	if err != nil {
		return err
	}
	st := fi.Sys().(*syscall.Stat_t)
	if e.dropVMM && fi.Mode().IsRegular() {
		private, err := e.privateNodeFile(path, fi)
		if err != nil {
			return err
		}
		if private {
			return fmt.Errorf("%s is a private node, not a universally shared image", path)
		}
	}
	if fi.Mode()&other != 0 || (st.Gid == vmmGroup && fi.Mode()&group != 0) {
		return nil
	}
	if !e.owns(path) {
		return fmt.Errorf("bhatti-vmm runs unprivileged and can't reach %s (mode %v, owner %d:%d): open it to others", path, fi.Mode().Perm(), st.Uid, st.Gid)
	}
	if err := os.Chown(path, -1, vmmGroup); err != nil {
		return err
	}
	return os.Chmod(path, fi.Mode()&(os.ModePerm|os.ModeSetgid|os.ModeSticky)|group)
}

// vmmReachable makes every directory from / down to dir searchable by the
// helpers.
func (e *Engine) vmmReachable(dir string) error {
	dir, err := filepath.Abs(dir)
	if err != nil {
		return err
	}
	for d := dir; ; d = filepath.Dir(d) {
		if err := e.shareWithVMMs(d, 0o001, 0o010); err != nil {
			return err
		}
		if d == filepath.Dir(d) {
			return nil
		}
	}
}

// vmmReadable makes the file at path readable by the helpers, and every
// directory on the way to it, as named and as resolved, searchable.
func (e *Engine) vmmReadable(path string) error {
	real, err := filepath.EvalSymlinks(path)
	if err != nil {
		return err
	}
	if err := e.vmmReachable(filepath.Dir(path)); err != nil {
		return err
	}
	if err := e.vmmReachable(filepath.Dir(real)); err != nil {
		return err
	}
	return e.shareWithVMMs(real, 0o004, 0o040)
}

// vmmUIDFor returns vm's helper uid, reserving one the first time. The search
// starts at a hash of the sandbox id, so helpers seldom share a uid even
// across engines on one host; within one engine they never do.
func (e *Engine) vmmUIDFor(vm *VM) (uint32, error) {
	vm.mu.Lock()
	uid := vm.vmmUID
	vm.mu.Unlock()
	if uid != 0 {
		return uid, nil
	}
	h := fnv.New32a()
	h.Write([]byte(vm.ID))
	start := h.Sum32()
	e.mu.Lock()
	for i := uint32(0); i < vmmIDSlots; i++ {
		u := vmmUIDBase + (start+i)%vmmIDSlots
		if _, taken := e.vmmIDs[u]; !taken {
			e.vmmIDs[u] = vm.ID
			uid = u
			break
		}
	}
	e.mu.Unlock()
	if uid == 0 {
		return 0, fmt.Errorf("no bhatti-vmm uid left")
	}
	vm.mu.Lock()
	vm.vmmUID = uid
	vm.mu.Unlock()
	return uid, nil
}

// releaseVMM gives back what vm's helper was handed that outlives the
// sandbox: read-write volumes return to root (a later sandbox may draw the
// same uid), and the uid to the pool.
func (e *Engine) releaseVMM(vm *VM) {
	if !e.dropVMM {
		return
	}
	vm.mu.Lock()
	uid, dir, vols := vm.vmmUID, vm.SandboxDir, vm.baseSpec.Volumes
	vm.vmmUID = 0
	vm.mu.Unlock()
	if uid == 0 {
		return
	}
	for _, v := range vols {
		if v.ReadOnly || within(dir, v.Path) {
			continue
		}
		if fi, err := os.Stat(v.Path); err == nil && fi.Sys().(*syscall.Stat_t).Uid == uid {
			_ = os.Chown(v.Path, 0, 0)
			_ = os.Chmod(v.Path, 0o600)
		}
	}
	e.mu.Lock()
	if e.vmmIDs[uid] == vm.ID {
		delete(e.vmmIDs, uid)
	}
	e.mu.Unlock()
}

// handToVMM gives path to vm's helper: outright (owned), or as its group with
// the daemon keeping it. A no-op unless helpers get their own uid.
func (e *Engine) handToVMM(vm *VM, path string, owned bool, mode os.FileMode) error {
	if !e.dropVMM {
		return nil
	}
	vm.mu.Lock()
	uid := int(vm.vmmUID)
	vm.mu.Unlock()
	owner := 0
	if owned {
		owner = uid
	}
	if err := os.Lchown(path, owner, uid); err != nil {
		return err
	}
	return os.Chmod(path, mode)
}

// netdGIDLocked picks the group ownerKey's helpers get: the only one its
// bhatti-netd's socket opens to. Caller holds e.netdMu.
func (e *Engine) netdGIDLocked(ownerKey string) uint32 {
	if !e.dropVMM {
		return 0
	}
	h := fnv.New32a()
	h.Write([]byte(ownerKey))
	start := h.Sum32()
	taken := make(map[uint32]bool, len(e.netds))
	for _, inst := range e.netds {
		taken[inst.vmmGID] = true
	}
	for i := uint32(0); i < vmmIDSlots; i++ {
		if g := vmmNetGIDBase + (start+i)%vmmIDSlots; !taken[g] {
			return g
		}
	}
	return 0
}

// shareNetdSocket opens inst's socket to its owner's helpers alone. Its
// directory is search-only for everyone: its other entries keep their own
// modes (the broker socket is netd's, the control socket and state root's).
func (e *Engine) shareNetdSocket(inst *netdInstance) error {
	if !e.dropVMM || inst.vmmGID == 0 {
		return nil
	}
	if err := os.Chmod(inst.dir, 0o711); err != nil {
		return err
	}
	if err := os.Lchown(inst.sock, 0, int(inst.vmmGID)); err != nil {
		return err
	}
	return os.Chmod(inst.sock, 0o660)
}

// confineCheckpoint gives a short-lived checker the same unprivileged identity
// pool as a VM, without exposing a stored snapshot to every VM's shared group.
// The loader has already opened the binary and libraries before the constructor;
// libkrun's host probe reads checkpoint.bin and opens /dev/kvm for ioctls.
func (e *Engine) confineCheckpoint(dir string) (string, func() error, error) {
	checkpoint := filepath.Join(dir, checkpointFile)
	p := VMMPolicy{NoNetwork: true, Rules: []VMMRule{
		{checkpoint, vmmReadFile},
		{"/dev/kvm", vmmDevice},
	}}
	cleanup := func() error { return nil }
	fail := func(err error) (string, func() error, error) {
		return "", nil, errors.Join(err, cleanup())
	}

	if e.dropVMM {
		vm := &VM{ID: "check-checkpoint:" + dir}
		uid, err := e.vmmUIDFor(vm)
		if err != nil {
			return fail(err)
		}
		cleanup = func() error { e.releaseVMM(vm); return nil }
		p.UID, p.GID = uid, uid
		p.Groups = []uint32{vmmGroup}
		if e.kvmGID != 0 {
			p.Groups = append(p.Groups, e.kvmGID)
		}
		fi, err := os.Lstat(checkpoint)
		if err != nil {
			return fail(err)
		}
		if !fi.Mode().IsRegular() {
			return fail(fmt.Errorf("checkpoint: %s isn't a plain file", checkpoint))
		}
		real, err := filepath.EvalSymlinks(checkpoint)
		if err != nil {
			return fail(err)
		}
		for _, parent := range []string{filepath.Dir(checkpoint), filepath.Dir(real)} {
			if err := e.vmmReachable(parent); err != nil {
				return fail(err)
			}
		}
		st := fi.Sys().(*syscall.Stat_t)
		if err := os.Lchown(checkpoint, int(st.Uid), int(uid)); err != nil {
			return fail(err)
		}
		release := cleanup
		cleanup = func() error {
			err := errors.Join(
				os.Lchown(checkpoint, int(st.Uid), int(st.Gid)),
				os.Chmod(checkpoint, fi.Mode().Perm()),
			)
			return errors.Join(err, release())
		}
		if err := os.Chmod(checkpoint, fi.Mode().Perm()|0o040); err != nil {
			return fail(err)
		}
	}
	f, err := os.CreateTemp(dir, ".vmm-check-*.policy")
	if err != nil {
		return fail(err)
	}
	policy := f.Name()
	release := cleanup
	cleanup = func() error { return errors.Join(os.Remove(policy), release()) }
	if _, err := f.Write(p.Encode()); err != nil {
		f.Close()
		return fail(err)
	}
	if err := f.Close(); err != nil {
		return fail(err)
	}
	return policy, cleanup, nil
}

// confineLaunch readies vm's helper to run confined. It returns the spec to
// hand the helper — spec, but with volumes and a restore's checkpoint linked
// into the sandbox dir — the policy file to name in VMMPolicyEnv, and what to
// undo once the launch is over.
func (e *Engine) confineLaunch(ctx context.Context, vm *VM, spec VMSpec, specPath string) (VMSpec, string, func(), error) {
	cleanup := func() {}
	fail := func(err error) (VMSpec, string, func(), error) { cleanup(); return spec, "", func() {}, err }
	var uid, nodeGID uint32
	if e.dropVMM {
		var err error
		if uid, err = e.vmmUIDFor(vm); err != nil {
			return fail(err)
		}
		if nodeGID, err = e.nodeGIDFor(vm); err != nil {
			return fail(err)
		}
	}
	// give makes path the helper's. Without a root daemon the helper is the
	// daemon's user, which owns it already.
	give := func(path string, owner, group uint32, mode os.FileMode) error {
		if !e.dropVMM {
			return nil
		}
		fi, err := os.Stat(path)
		if err != nil {
			return err
		}
		private, err := e.privateNodeFile(path, fi)
		if err != nil {
			return err
		}
		if private {
			if mode&0o222 != 0 {
				return fmt.Errorf("%s is an immutable private node", path)
			}
			return e.nodeReadable(path, nodeGID)
		}
		if err := os.Lchown(path, int(owner), int(group)); err != nil {
			return err
		}
		return os.Chmod(path, mode)
	}
	dir := vm.SandboxDir
	p := VMMPolicy{Rules: []VMMRule{{specPath, vmmReadFile}, {"/dev/kvm", vmmDevice}}}
	if e.dropVMM {
		p.UID, p.GID = uid, uid
		p.Groups = []uint32{vmmGroup, nodeGID}
		if e.kvmGID != 0 {
			p.Groups = append(p.Groups, e.kvmGID)
		}
		e.netdMu.Lock()
		if inst := e.netds[vm.netdKey]; inst != nil && inst.vmmGID != 0 {
			p.Groups = append(p.Groups, inst.vmmGID)
		}
		e.netdMu.Unlock()
	}
	// The sandbox dir is the daemon's (state.json, config.json and the policy
	// itself are in it); its helper may only pass through.
	if err := give(dir, 0, uid, 0o710); err != nil {
		return fail(err)
	}

	if err := give(spec.RootDisk, uid, uid, 0o600); err != nil {
		return fail(err)
	}
	p.Rules = append(p.Rules, VMMRule{spec.RootDisk, vmmRWFile})
	// Only the daemon's format record can make a disk's bytes a header. Each
	// pointer is compared before its target is opened, so a rewritten header
	// cannot expand the helper's policy.
	backs := func(what, path, format string, recorded []diskLayer) error {
		if err := validateRecordedDisk(path, format, recorded); err != nil {
			return fmt.Errorf("%s: %w", what, err)
		}
		chain := recorded
		if chain == nil {
			var err error
			chain, err = e.legacyDiskChain(vm, path, format)
			if err != nil {
				return fmt.Errorf("%s: %w", what, err)
			}
		}
		for _, b := range chain {
			if e.dropVMM {
				if err := e.backingReadable(b.Path, nodeGID); err != nil {
					return err
				}
			}
			p.Rules = append(p.Rules, VMMRule{b.Path, vmmReadFile})
		}
		return nil
	}
	if spec.RootDiskFormat == "qcow2" {
		if err := backs("root disk", spec.RootDisk, spec.RootDiskFormat, spec.Backing); err != nil {
			return fail(err)
		}
	}
	if spec.KernelImage != "" {
		// A missing kernel fails the helper's start, which says so: no reason
		// to refuse here.
		if e.dropVMM {
			if err := e.vmmReadable(spec.KernelImage); err != nil && !errors.Is(err, os.ErrNotExist) {
				return fail(err)
			}
		}
		p.Rules = append(p.Rules, VMMRule{spec.KernelImage, vmmReadFile})
	}

	if len(spec.Volumes) > 0 {
		spec.Volumes = slices.Clone(spec.Volumes)
		vols := filepath.Join(dir, "vol")
		if err := os.MkdirAll(vols, 0o700); err != nil {
			return fail(err)
		}
		if err := give(vols, 0, uid, 0o710); err != nil {
			return fail(err)
		}
		for i := range spec.Volumes {
			v := &spec.Volumes[i]
			if !within(dir, v.Path) {
				path, err := linkInto(v.Path, filepath.Join(vols, strconv.Itoa(i)))
				if err != nil {
					return fail(fmt.Errorf("volume %s: %w", v.Path, err))
				}
				if path == v.Path && e.dropVMM { // on another filesystem
					if err := e.vmmReachable(filepath.Dir(path)); err != nil {
						return fail(err)
					}
				}
				v.Path = path
			}
			owner, group, mode, access := uid, uid, os.FileMode(0o600), vmmRWFile
			if v.ReadOnly {
				// May be attached to other sandboxes too.
				owner, group, mode, access = 0, vmmGroup, 0o640, vmmReadFile
			}
			if err := give(v.Path, owner, group, mode); err != nil {
				return fail(err)
			}
			p.Rules = append(p.Rules, VMMRule{v.Path, access})
			if v.Format == "qcow2" {
				if err := backs("volume "+v.BlockID, v.Path, v.Format, v.Backing); err != nil {
					return fail(err)
				}
			}
		}
	}

	// A restore reads the checkpoint through links in its own dir: the
	// snapshot's directory needn't open to any helper.
	if spec.SnapshotDir != "" {
		restore := filepath.Join(dir, "restore")
		if err := os.RemoveAll(restore); err != nil {
			return fail(err)
		}
		if err := os.Mkdir(restore, 0o700); err != nil {
			return fail(err)
		}
		cleanup = func() { os.RemoveAll(restore) }
		if err := give(restore, 0, uid, 0o710); err != nil {
			return fail(err)
		}
		for _, name := range []string{checkpointFile, memoryFile} {
			src, dst := filepath.Join(spec.SnapshotDir, name), filepath.Join(restore, name)
			if _, err := os.Lstat(src); errors.Is(err, os.ErrNotExist) {
				continue // an incomplete checkpoint: libkrun refuses it, and says so
			}
			if path, err := linkInto(src, dst); err != nil {
				return fail(err)
			} else if path == src { // on another filesystem
				if err := cloneFile(ctx, src, dst); err != nil {
					return fail(err)
				}
			}
			if err := give(dst, 0, vmmGroup, 0o640); err != nil {
				return fail(err)
			}
		}
		spec.SnapshotDir = restore
		p.Rules = append(p.Rules, VMMRule{restore, vmmReadTree})
	}

	save := filepath.Join(dir, "save")
	if err := os.MkdirAll(save, 0o700); err != nil {
		return fail(err)
	}
	if err := give(save, uid, uid, 0o700); err != nil {
		return fail(err)
	}
	p.Rules = append(p.Rules, VMMRule{save, vmmSaveDir})

	if err := give(vm.SockDir, uid, uid, 0o700); err != nil {
		return fail(err)
	}
	p.Rules = append(p.Rules, VMMRule{vm.SockDir, vmmSockDir})

	for _, m := range spec.Mounts {
		access, caps := vmmMountRW, vmmMountRWCaps
		if m.ReadOnly {
			access, caps = vmmReadTree, vmmMountROCaps
		}
		p.Mounts = append(p.Mounts, VMMRule{m.HostPath, access})
		if e.dropVMM {
			for _, c := range caps {
				if !slices.Contains(p.Caps, c) {
					p.Caps = append(p.Caps, c)
				}
			}
		}
	}

	policy := filepath.Join(dir, "vmm.policy")
	if err := os.WriteFile(policy, p.Encode(), 0o600); err != nil {
		return fail(err)
	}
	return spec, policy, cleanup, nil
}

// validateRecordedDisk never follows a pointer until it has matched the
// daemon's path AND format. In particular a raw terminal's bytes are opaque.
func validateRecordedDisk(path, format string, recorded []diskLayer) error {
	if format == "" {
		format = "raw"
	}
	if format != "raw" && format != "qcow2" {
		return fmt.Errorf("%s: unsupported disk format %q", path, format)
	}
	if format == "raw" {
		if len(recorded) != 0 {
			return fmt.Errorf("%s: raw disk has recorded backing", path)
		}
		return nil
	}
	// nil is reserved for VM records predating typed chains. A snapshot with
	// an explicit format supplies an exact (possibly empty) nonnil chain.
	if recorded == nil {
		return nil
	}
	if len(recorded) > maxChainDepth {
		return fmt.Errorf("%s: recorded chain deeper than %d", path, maxChainDepth)
	}
	for i := 0; ; i++ {
		next, err := qcow2BackingPath(path)
		if err != nil {
			return err
		}
		nextFormat, err := qcow2BackingFormat(path)
		if err != nil {
			return err
		}
		if next == "" {
			if i != len(recorded) {
				return fmt.Errorf("%s: backing chain is shorter than the recorded chain", path)
			}
			return nil
		}
		if i == len(recorded) || next != recorded[i].Path || nextFormat != recorded[i].Format {
			return fmt.Errorf("%s: backing %s (%s) does not match the daemon-recorded chain", path, next, nextFormat)
		}
		switch nextFormat {
		case "raw":
			if i+1 != len(recorded) {
				return fmt.Errorf("%s: recorded raw backing has ancestors", next)
			}
			return nil
		case "qcow2":
			path = next
		default:
			return fmt.Errorf("%s: unsupported recorded backing format %q", next, nextFormat)
		}
	}
}

func (e *Engine) validateDiskBacking(vm *VM, spec VMSpec) error {
	if err := validateRecordedDisk(spec.RootDisk, spec.RootDiskFormat, spec.Backing); err != nil {
		return fmt.Errorf("root disk: %w", err)
	}
	if spec.RootDiskFormat == "qcow2" && spec.Backing == nil {
		if _, err := e.legacyDiskChain(vm, spec.RootDisk, spec.RootDiskFormat); err != nil {
			return fmt.Errorf("root disk: %w", err)
		}
	}
	for _, v := range spec.Volumes {
		if err := validateRecordedDisk(v.Path, v.Format, v.Backing); err != nil {
			return fmt.Errorf("volume %s: %w", v.BlockID, err)
		}
		if v.Format == "qcow2" && v.Backing == nil {
			if _, err := e.legacyDiskChain(vm, v.Path, v.Format); err != nil {
				return fmt.Errorf("volume %s: %w", v.BlockID, err)
			}
		}
	}
	return nil
}

// Legacy inference is a compatibility read, never a new trust record. The
// authorizer runs before a named file can be opened or sniffed.
func (e *Engine) legacyDiskChain(vm *VM, path, format string) ([]diskLayer, error) {
	owned, err := ownedNodeKeys(e.cfg.DataDir, vm.UserID)
	if err != nil {
		return nil, err
	}
	return readLegacyDiskChain(path, format, func(next string) error {
		return e.authorizedBacking(vm, next, owned)
	})
}

func (e *Engine) validateSnapshotBacking(vm *VM, spec VMSpec) error {
	if _, err := e.legacyDiskChain(vm, spec.RootDisk, spec.RootDiskFormat); err != nil {
		return fmt.Errorf("root disk: %w", err)
	}
	for _, v := range spec.Volumes {
		if _, err := e.legacyDiskChain(vm, v.Path, v.Format); err != nil {
			return fmt.Errorf("volume %s: %w", v.BlockID, err)
		}
	}
	return nil
}

type backingAuthorizationRefusal struct {
	path string
}

func (e *backingAuthorizationRefusal) Error() string {
	return fmt.Sprintf("backs onto %s, which isn't an authorized image or this owner's snapshot node", e.path)
}

func (e *Engine) authorizedBacking(vm *VM, path string, owned map[fileKey]bool) error {
	if e.vmmBaseAllowed(vm, path) {
		return nil
	}
	fi, err := os.Stat(path)
	if err != nil {
		return err
	}
	if key, ok := keyOf(fi); ok && owned[key] {
		return nil
	}
	return &backingAuthorizationRefusal{path: path}
}

// vmmBaseAllowed reports whether a helper may read the image at path because
// its root disk backs onto it: one this host keeps for sandboxes — under
// <DataDir>/images (directly, a shared base, or one of the sandbox owner's
// own), the dev base, or the configured base.
func (e *Engine) vmmBaseAllowed(vm *VM, path string) bool {
	real, err := filepath.EvalSymlinks(path)
	if err != nil {
		return false
	}
	data, err := filepath.EvalSymlinks(e.cfg.DataDir)
	if err != nil {
		return false
	}
	if real == filepath.Join(data, "base.img") {
		return true
	}
	if e.cfg.BaseImage != "" {
		if base, err := filepath.EvalSymlinks(e.cfg.BaseImage); err == nil && real == base {
			return true
		}
	}
	rel, err := filepath.Rel(filepath.Join(data, "images"), real)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return false
	}
	first, _, nested := strings.Cut(rel, string(filepath.Separator))
	return !nested || first == "bases" || (vm.UserID != "" && first == vm.UserID)
}

// linkInto hard-links src at dst, replacing what's there, and returns dst —
// or src, untouched, when the two are on different filesystems.
func linkInto(src, dst string) (string, error) {
	if err := os.Remove(dst); err != nil && !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	if err := os.Link(src, dst); err != nil {
		if errors.Is(err, syscall.EXDEV) {
			return src, nil
		}
		return "", err
	}
	return dst, nil
}

// saveStage names a directory, not yet made, for vm's helper to SAVE into: its
// save dir is the only place it may create files.
func saveStage(vm *VM) (string, error) {
	dir := filepath.Join(vm.SandboxDir, "save")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	id, err := generateID()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "ckpt-"+id), nil
}

// takeSave moves the checkpoint a helper SAVEd at stage to dst, out of the
// helper's reach: dst ends up the daemon's, and holds only plain files — the
// helper wrote them, so they are checked before the daemon trusts them.
func (e *Engine) takeSave(stage, dst string) error {
	if err := os.Rename(stage, dst); err != nil {
		if !errors.Is(err, syscall.EXDEV) {
			return err
		}
		if err := copySave(stage, dst); err != nil {
			return err
		}
		os.RemoveAll(stage)
	}
	if e.dropVMM {
		if err := os.Lchown(dst, 0, 0); err != nil {
			return err
		}
		if err := os.Chmod(dst, 0o700); err != nil {
			return err
		}
	}
	entries, err := os.ReadDir(dst)
	if err != nil {
		return err
	}
	for _, ent := range entries {
		path := filepath.Join(dst, ent.Name())
		fi, err := os.Lstat(path)
		if err != nil {
			return err
		}
		if st, ok := fi.Sys().(*syscall.Stat_t); !ok || !fi.Mode().IsRegular() || st.Nlink != 1 {
			return fmt.Errorf("checkpoint: %s isn't a plain file", path)
		}
		if e.dropVMM {
			if err := os.Lchown(path, 0, 0); err != nil {
				return err
			}
			if err := os.Chmod(path, 0o600); err != nil {
				return err
			}
		}
	}
	return nil
}

// copySave copies a SAVEd checkpoint to dst on another filesystem, opening
// nothing a symlink leads to.
func copySave(stage, dst string) error {
	entries, err := os.ReadDir(stage)
	if err != nil {
		return err
	}
	if err := os.Mkdir(dst, 0o700); err != nil {
		return err
	}
	buf := make([]byte, 1<<20)
	for _, ent := range entries {
		if err := copyPlain(filepath.Join(stage, ent.Name()), filepath.Join(dst, ent.Name()), buf); err != nil {
			return err
		}
	}
	return nil
}

func copyPlain(src, dst string, buf []byte) error {
	in, err := os.OpenFile(src, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return err
	}
	defer in.Close()
	fi, err := in.Stat()
	if err != nil {
		return err
	}
	if !fi.Mode().IsRegular() {
		return fmt.Errorf("checkpoint: %s isn't a plain file", src)
	}
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if err := writeSparse(out, in, fi.Size(), buf); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}
