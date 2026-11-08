package krucible

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

// Node groups outlive sandboxes and netds. Never recycle one: an owner can
// retain a snapshot or a store-only ancestor after its last VM disappears.
const vmmNodeGIDBase = vmmNetGIDBase + vmmIDSlots

const (
	nodeGroupsFile = ".node-groups.json"
	nodeGroupsLock = ".node-access.lock"
)

type nodeGroupReservation struct {
	Identity string `json:"identity"`
	Owner    string `json:"owner"`
	GID      uint32 `json:"gid"`
}

type nodeGroupRegistry struct {
	Version int                    `json:"version"`
	Groups  []nodeGroupReservation `json:"groups"`
}

// Keep the logical identity within the OS account-name limit without needing
// named accounts. The full owner key is retained to detect even a hash collision.
func nodeOwnerIdentity(owner string) string {
	sum := sha256.Sum256([]byte(owner))
	return "bhn-" + hex.EncodeToString(sum[:14])
}

func nodeGroupSlot(owner string) uint32 {
	h := fnv.New32a()
	_, _ = h.Write([]byte(owner))
	return h.Sum32() % vmmIDSlots
}

func isNodeGID(gid uint32) bool {
	return gid >= vmmNodeGIDBase && gid < vmmNodeGIDBase+vmmIDSlots
}

func (r *nodeGroupRegistry) owner(gid uint32) string {
	for _, g := range r.Groups {
		if g.GID == gid {
			return g.Owner
		}
	}
	return ""
}

func validNodeOwner(owner string) bool {
	return len(owner) > 2 && (strings.HasPrefix(owner, "u:") || strings.HasPrefix(owner, "s:")) && !strings.ContainsRune(owner, 0)
}

func (r *nodeGroupRegistry) validate() error {
	if r.Version != 1 {
		return fmt.Errorf("unsupported node group registry version %d", r.Version)
	}
	identities := make(map[string]bool, len(r.Groups))
	gids := make(map[uint32]bool, len(r.Groups))
	for _, g := range r.Groups {
		if !validNodeOwner(g.Owner) || g.Identity != nodeOwnerIdentity(g.Owner) || !isNodeGID(g.GID) || identities[g.Identity] || gids[g.GID] {
			return fmt.Errorf("invalid or colliding node group reservation %q (%d)", g.Identity, g.GID)
		}
		identities[g.Identity], gids[g.GID] = true, true
	}
	return nil
}

func (r *nodeGroupRegistry) reserve(owner string) (uint32, bool, error) {
	if !validNodeOwner(owner) {
		return 0, false, fmt.Errorf("invalid node owner %q", owner)
	}
	identity := nodeOwnerIdentity(owner)
	for _, g := range r.Groups {
		if g.Identity == identity {
			if g.Owner != owner {
				return 0, false, fmt.Errorf("node owner identity collision for %q", identity)
			}
			return g.GID, false, nil
		}
	}
	taken := make(map[uint32]bool, len(r.Groups))
	for _, g := range r.Groups {
		taken[g.GID] = true
	}
	start := nodeGroupSlot(owner)
	for i := range uint32(vmmIDSlots) {
		gid := vmmNodeGIDBase + (start+i)%vmmIDSlots
		if !taken[gid] {
			r.Groups = append(r.Groups, nodeGroupReservation{Identity: identity, Owner: owner, GID: gid})
			return gid, true, nil
		}
	}
	return 0, false, fmt.Errorf("no bhatti-vmm node group left")
}

func nodeAccessRecord(fi os.FileInfo) bool {
	st, ok := fi.Sys().(*syscall.Stat_t)
	return ok && fi.Mode().IsRegular() && fi.Mode().Perm() == 0o600 &&
		fi.Mode()&(os.ModeSetuid|os.ModeSetgid|os.ModeSticky) == 0 &&
		st.Nlink == 1 && st.Uid == uint32(os.Geteuid())
}

// This lock is deliberately independent of .bases.lock: launch already holds
// a shared bases lock and must not try to upgrade it to reserve an owner GID.
func lockNodeGroups(dataDir string) (*os.File, error) {
	path := filepath.Join(dataDir, nodeGroupsLock)
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0o600)
	if err != nil {
		return nil, err
	}
	fi, err := f.Stat()
	if err == nil && !nodeAccessRecord(fi) {
		err = fmt.Errorf("%s is not a private daemon lock", path)
	}
	if err == nil {
		for {
			if err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != syscall.EINTR {
				break
			}
		}
	}
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	return f, nil
}

func readNodeGroups(dataDir string) (nodeGroupRegistry, error) {
	var r nodeGroupRegistry
	path := filepath.Join(dataDir, nodeGroupsFile)
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if errors.Is(err, os.ErrNotExist) {
		// A lost registry must not let a retained node's GID be reassigned.
		entries, err := os.ReadDir(nodesDir(dataDir))
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return r, err
		}
		for _, de := range entries {
			fi, err := de.Info()
			if err != nil {
				return r, err
			}
			if st, ok := fi.Sys().(*syscall.Stat_t); ok && isNodeGID(st.Gid) {
				return r, fmt.Errorf("%s is missing but retained node groups exist", path)
			}
		}
		r.Version = 1
		return r, nil
	}
	if err != nil {
		return r, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return r, err
	}
	if !nodeAccessRecord(fi) {
		return r, fmt.Errorf("%s is not a private daemon registry", path)
	}
	dec := json.NewDecoder(f)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&r); err != nil {
		return r, fmt.Errorf("%s: %w", path, err)
	}
	if err := dec.Decode(new(any)); err != io.EOF {
		return r, fmt.Errorf("%s: trailing registry data", path)
	}
	if err := r.validate(); err != nil {
		return r, fmt.Errorf("%s: %w", path, err)
	}
	return r, nil
}

func (e *Engine) nodeGIDFor(vm *VM) (uint32, error) {
	if !e.dropVMM {
		return 0, nil
	}
	vm.mu.Lock()
	owner := ownerKeyFor(vm.UserID, "anonymous")
	vm.mu.Unlock()
	lock, err := lockNodeGroups(e.cfg.DataDir)
	if err != nil {
		return 0, err
	}
	defer lock.Close()
	r, err := readNodeGroups(e.cfg.DataDir)
	if err != nil {
		return 0, err
	}
	gid, added, err := r.reserve(owner)
	if err != nil {
		return 0, err
	}
	if added {
		// This must be durable before any inode is assigned the reservation.
		if err := writeDaemonMetadata(filepath.Join(e.cfg.DataDir, nodeGroupsFile), r); err != nil {
			return 0, err
		}
	}
	return gid, nil
}

// setNodeAccess removes old universal/helper ownership before changing groups.
// Sync the inode before a caller can publish another link to it.
func setNodeAccess(path string, gid uint32) error {
	return setNodeAccessMode(path, gid, gid != 0)
}

type nodeAccessFile interface {
	Stat() (os.FileInfo, error)
	Chmod(os.FileMode) error
	Chown(int, int) error
	Sync() error
}

func setNodeAccessMode(path string, gid uint32, readable bool) error {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return err
	}
	defer f.Close()
	return applyNodeAccess(f, path, gid, readable)
}

func applyNodeAccess(f nodeAccessFile, path string, gid uint32, readable bool) error {
	fi, err := f.Stat()
	if err != nil {
		return err
	}
	if !fi.Mode().IsRegular() {
		return fmt.Errorf("%s is not a plain node file", path)
	}
	mode := os.FileMode(0o600)
	if readable {
		mode = 0o640
	}
	st := fi.Sys().(*syscall.Stat_t)
	current := fi.Mode() & (os.ModePerm | os.ModeSetuid | os.ModeSetgid | os.ModeSticky)
	if st.Uid == 0 && st.Gid == gid && current == mode {
		return nil
	}
	// Only a group transition needs to revoke the old group's access.
	if st.Gid != gid {
		if err := f.Chmod(0o600); err != nil {
			return err
		}
		current = 0o600
	}
	if st.Uid != 0 || st.Gid != gid {
		if err := f.Chown(0, int(gid)); err != nil {
			return err
		}
	}
	if current != mode {
		if err := f.Chmod(mode); err != nil {
			return err
		}
	}
	return f.Sync()
}

// prepareNodeAccess is called BEFORE linking or renaming an inode into nodes/.
// Reuse preserves a durably reserved owner group; new and unknown nodes stay
// daemon-only until a launch with trusted owner provenance grants them access.
func (e *Engine) prepareNodeAccess(path string) error {
	if !e.dropVMM {
		return nil
	}
	lock, err := lockNodeGroups(e.cfg.DataDir)
	if err != nil {
		return err
	}
	defer lock.Close()
	r, err := readNodeGroups(e.cfg.DataDir)
	if err != nil {
		return err
	}
	fi, err := os.Lstat(path)
	if err != nil {
		return err
	}
	gid := uint32(0)
	if st, ok := fi.Sys().(*syscall.Stat_t); ok && r.owner(st.Gid) != "" {
		gid = st.Gid
	}
	return setNodeAccess(path, gid)
}

// Inode comparison also covers legacy saved-image/kernel aliases outside the
// store. Path-based classification alone would let vmmReadable reopen a node
// to every helper under its other name.
func (e *Engine) privateNodeFile(path string, fi os.FileInfo) (bool, error) {
	if !fi.Mode().IsRegular() {
		return false, nil
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return false, fmt.Errorf("%s: no file identity", path)
	}
	if isNodeGID(st.Gid) || within(nodesDir(e.cfg.DataDir), path) {
		return true, nil
	}
	if st.Nlink < 2 {
		return false, nil
	}
	entries, err := os.ReadDir(nodesDir(e.cfg.DataDir))
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	for _, de := range entries {
		if strings.HasPrefix(de.Name(), ".") || !de.Type().IsRegular() {
			continue
		}
		node, err := de.Info()
		if err != nil {
			return false, err
		}
		if os.SameFile(fi, node) {
			return true, nil
		}
	}
	return false, nil
}

func (e *Engine) nodeReadable(path string, gid uint32) error {
	real, err := filepath.EvalSymlinks(path)
	if err != nil {
		return err
	}
	lock, err := lockNodeGroups(e.cfg.DataDir)
	if err != nil {
		return err
	}
	defer lock.Close()
	r, err := readNodeGroups(e.cfg.DataDir)
	if err != nil {
		return err
	}
	if r.owner(gid) == "" {
		return fmt.Errorf("node group %d has no durable owner reservation", gid)
	}
	fi, err := os.Stat(real)
	if err != nil {
		return err
	}
	st := fi.Sys().(*syscall.Stat_t)
	if isNodeGID(st.Gid) && st.Gid != gid {
		return fmt.Errorf("%s belongs to another owner's node group", path)
	}
	if err := setNodeAccess(real, gid); err != nil {
		return err
	}
	for _, dir := range []string{filepath.Dir(path), filepath.Dir(real)} {
		if err := e.vmmReachable(dir); err != nil {
			return err
		}
	}
	return nil
}

func (e *Engine) backingReadable(path string, gid uint32) error {
	fi, err := os.Stat(path)
	if err != nil {
		return err
	}
	private, err := e.privateNodeFile(path, fi)
	if err != nil {
		return err
	}
	if private {
		return e.nodeReadable(path, gid)
	}
	return e.vmmReadable(path)
}

// migrateNodeAccess runs before recovery/sweep. Existing helpers retain their
// open chain FDs; their next launch receives the new supplementary owner group.
// Unknown or conflicting provenance is quarantined rather than made universal.
func (e *Engine) migrateNodeAccess() error {
	if !e.dropVMM {
		return nil
	}
	unlock, err := lockBases(e.cfg.DataDir, true)
	if err != nil {
		return err
	}
	defer unlock()
	lock, err := lockNodeGroups(e.cfg.DataDir)
	if err != nil {
		return err
	}
	defer lock.Close()
	r, registryErr := readNodeGroups(e.cfg.DataDir)
	entries, err := os.ReadDir(nodesDir(e.cfg.DataDir))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return errors.Join(registryErr, err)
	}
	nodes := map[fileKey]string{}
	for _, de := range entries {
		path := filepath.Join(nodesDir(e.cfg.DataDir), de.Name())
		fi, err := de.Info()
		if err != nil {
			return errors.Join(registryErr, err)
		}
		if !fi.Mode().IsRegular() {
			return fmt.Errorf("%s is not a plain node-store file", path)
		}
		// Publication pins alias disk data, not private daemon metadata.
		if numberedStage(de.Name(), ".publish-", "") {
			continue
		}
		if strings.HasPrefix(de.Name(), ".") && fi.Sys().(*syscall.Stat_t).Nlink != 1 {
			_ = setNodeAccess(path, 0)
			return fmt.Errorf("%s has an alias to hidden node metadata", path)
		}
		gid := uint32(0)
		if registryErr == nil && !strings.HasPrefix(de.Name(), ".") {
			st := fi.Sys().(*syscall.Stat_t)
			if r.owner(st.Gid) != "" {
				gid = st.Gid
			}
		}
		if registryErr != nil && !strings.HasPrefix(de.Name(), ".") {
			// Retain evidence of a lost reservation, but revoke group access.
			// A second startup must not mistake it for an unused namespace.
			if st := fi.Sys().(*syscall.Stat_t); isNodeGID(st.Gid) {
				if err := setNodeAccessMode(path, st.Gid, false); err != nil {
					return errors.Join(registryErr, err)
				}
				continue
			}
		}
		if err := setNodeAccess(path, gid); err != nil {
			return errors.Join(registryErr, err)
		}
		if !strings.HasPrefix(de.Name(), ".") {
			if key, ok := keyOf(fi); ok {
				nodes[key] = path
			}
		}
	}
	if registryErr != nil {
		return registryErr
	}
	owners, err := e.nodeProvenance(nodes)
	if err != nil {
		return err
	}
	changed := false
	gids := map[fileKey]uint32{}
	for key, owner := range owners {
		if owner == "" {
			if err := setNodeAccess(nodes[key], 0); err != nil {
				return err
			}
			continue
		}
		gid, added, err := r.reserve(owner)
		if err != nil {
			return err
		}
		changed = changed || added
		gids[key] = gid
	}
	if changed {
		if err := writeDaemonMetadata(filepath.Join(e.cfg.DataDir, nodeGroupsFile), r); err != nil {
			return err
		}
	}
	for key, gid := range gids {
		path := nodes[key]
		fi, err := os.Stat(path)
		if err != nil {
			return err
		}
		old := fi.Sys().(*syscall.Stat_t).Gid
		if isNodeGID(old) && old != gid {
			gid = 0 // Conflicting durable assignment is not permission to reassign.
		}
		if err := setNodeAccess(path, gid); err != nil {
			return err
		}
	}
	return nil
}

func (e *Engine) nodeProvenance(nodes map[fileKey]string) (map[fileKey]string, error) {
	owners := map[fileKey]string{}
	add := func(key fileKey, owner string) {
		if _, stored := nodes[key]; !stored {
			return
		}
		if previous, exists := owners[key]; exists && previous != owner {
			owners[key] = ""
		} else if !exists {
			owners[key] = owner
		}
	}
	users := map[string]bool{}
	for _, root := range []string{"snapshots", "images"} {
		entries, err := os.ReadDir(filepath.Join(e.cfg.DataDir, root))
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
		for _, de := range entries {
			if !de.IsDir() || (root == "images" && de.Name() == "bases") {
				continue
			}
			if root == "snapshots" {
				files, err := os.ReadDir(filepath.Join(e.cfg.DataDir, root, de.Name()))
				if err != nil {
					return nil, err
				}
				snapshot := false
				for _, file := range files {
					name := file.Name()
					if name == snapshotManifestFile || name == "rootfs.qcow2" ||
						name == checkpointFile || name == memoryFile || name == "config.ext4" ||
						numberedStage(name, "vol", ".img") {
						snapshot = true
						break
					}
				}
				// A direct snapshot has no owner encoded in its directory name.
				// Its first authorized restore assigns the retained node group.
				if snapshot {
					continue
				}
			}
			users[de.Name()] = true
		}
	}
	records, err := filepath.Glob(filepath.Join(e.cfg.DataDir, "sandboxes", "*", stateFile))
	if err != nil {
		return nil, err
	}
	for _, path := range records {
		var rec vmRecord
		found, err := readDaemonMetadata(path, &rec)
		if err != nil {
			return nil, err
		}
		if !found {
			continue
		}
		owner := ownerKeyFor(rec.UserID, "anonymous")
		if !validNodeOwner(owner) {
			return nil, fmt.Errorf("%s: invalid node owner", path)
		}
		if rec.UserID != "" {
			users[rec.UserID] = true
		}
		addPath := func(path string) error {
			if path == "" {
				return nil
			}
			fi, err := os.Stat(path)
			if errors.Is(err, os.ErrNotExist) {
				return nil
			}
			if err != nil {
				return err
			}
			if key, ok := keyOf(fi); ok {
				add(key, owner)
			}
			return nil
		}
		if err := addPath(rec.BaseSpec.RootDisk); err != nil {
			return nil, err
		}
		for _, layer := range rec.BaseSpec.Backing {
			if err := addPath(layer.Path); err != nil {
				return nil, err
			}
		}
		for _, volume := range rec.BaseSpec.Volumes {
			if err := addPath(volume.Path); err != nil {
				return nil, err
			}
			for _, layer := range volume.Backing {
				if err := addPath(layer.Path); err != nil {
					return nil, err
				}
			}
		}
	}
	for user := range users {
		keys, err := ownedNodeKeys(e.cfg.DataDir, user)
		if err != nil {
			return nil, err
		}
		for key := range keys {
			add(key, ownerKeyFor(user, ""))
		}
	}
	return owners, nil
}
