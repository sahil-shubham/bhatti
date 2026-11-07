package krucible

// Nodes. A snapshot's disk is a layer other disks are written over and never
// written itself: a sandbox resumed from the snapshot gets a qcow2 overlay
// whose backing file is that disk, so a resume costs a header however much
// the snapshot holds, and N resumes share one copy of it. A fork does the same
// with the copy of its source's overlay that it took while the source was
// paused. A disk made over a node can be copied (a snapshot of the resumed
// sandbox, a saved image) and the copy names the same node, so a chain of
// nodes grows with each generation, up to maxChainDepth.
//
// An overlay names a node through the node store, <DataDir>/nodes: a hard link
// to the disk, named by the file's identity, not through the snapshot
// directory. Two reasons:
//
//   - A helper must be able to open every file its root disk backs onto, and
//     the snapshot directory also holds the guest's RAM. vmmuser.go keeps that
//     directory the daemon's alone (a restore reads the checkpoint through
//     links in its own sandbox dir); the store holds disks only.
//   - The link keeps the disk's blocks alive whatever happens to the
//     snapshot directory, so deleting a snapshot can never break a disk made
//     over it. What the engine still has to decide is when a link is garbage:
//     when nothing but the store holds the file (no snapshot or saved image
//     has it under another name) and no disk's chain names it.
//
// The chain a disk names is the only record of what depends on what. It is
// read from the headers, as GCImages reads it for bases: a column in the
// store would miss every place a copy of an overlay carries its backing name
// to (a snapshot of a resumed sandbox, a saved image, a fork), and would
// disagree with the files after a crash between making a disk and recording
// it.
//
// The same lock as bases (lockBases) orders all of it: a create holds it
// shared from picking a node until the overlay names it, and a sweep or a
// snapshot removal holds it exclusively.

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/sahil-shubham/bhatti/pkg/engine"
)

// maxChainDepth is how many qcow2 images may make up one disk, the overlay at
// the top included. Every layer adds a lookup to a read that falls through it
// and a file the helper holds open, and the confinement policy (backingChain)
// and every scan here read the whole chain.
//
// A resume or fork that would go past it copies the node's own layer as the
// new disk instead of layering over it: the copy has the node's depth, not one
// more, and costs the size of that one layer, not of everything below it. So
// the cap never refuses anything; a long lineage just pays what every resume
// paid before nodes, once per generation past the cap.
const maxChainDepth = 8

// maxChainWalk bounds a walk of a chain that may loop (a header the guest
// rewrote), well past anything maxChainDepth admits.
const maxChainWalk = 64

// nodesDir is the node store.
func nodesDir(dataDir string) string { return filepath.Join(dataDir, "nodes") }

// nodeName is a node's name in the store: the identity of its file, which no
// other file has while this one exists, and the link itself keeps it so.
func nodeName(k fileKey) string { return fmt.Sprintf("%x-%x.img", k.dev, k.ino) }

// errNoNodeLink says a disk can't be given a name in the node store.
var errNoNodeLink = errors.New("can't link the disk into the node store")

// linkNode is os.Link, for a test that needs a link to fail.
var linkNode = os.Link

// nodeLink returns the name src has in the node store, linking it there
// first if it has none. The link is the same file, never a copy: it costs a
// directory entry whatever src holds, and src itself is not touched.
func (e *Engine) nodeLink(src string) (string, error) {
	real, err := resolveBasePath(src)
	if err != nil {
		return "", err
	}
	fi, err := os.Stat(real)
	if err != nil {
		return "", err
	}
	if !fi.Mode().IsRegular() {
		return "", fmt.Errorf("%s isn't a regular file", real)
	}
	k, ok := keyOf(fi)
	if !ok {
		return "", fmt.Errorf("%w: no file identity here", errNoNodeLink)
	}
	dir, err := filepath.Abs(nodesDir(e.cfg.DataDir))
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	dst := filepath.Join(dir, nodeName(k))
	switch err := linkNode(real, dst); {
	case err == nil:
	case errors.Is(err, fs.ErrExist):
		have, serr := os.Stat(dst)
		if serr != nil || !os.SameFile(fi, have) {
			return "", fmt.Errorf("node store entry %s is not %s", dst, real)
		}
	default:
		// Another file system (a link can't cross one), or one without links:
		// the caller copies instead.
		return "", fmt.Errorf("%w: %v", errNoNodeLink, err)
	}
	return dst, nil
}

// chainDepth counts the qcow2 images that make up the disk at path: itself,
// if it is one, and each qcow2 file it backs onto in turn. A raw image counts
// for none.
func chainDepth(path string) (int, error) {
	depth := 0
	for cur := path; ; {
		if !isQcow2(cur) {
			return depth, nil
		}
		if depth++; depth > maxChainWalk {
			return 0, fmt.Errorf("%s: backing chain deeper than %d or looping", path, maxChainWalk)
		}
		next, err := qcow2BackingPath(cur)
		if err != nil {
			return 0, fmt.Errorf("%s: %w", cur, err)
		}
		if next == "" {
			return depth, nil
		}
		cur = next
	}
}

// layerOn makes the disk at dst: a qcow2 overlay over the immutable disk at
// src, in O(1). src is a snapshot's disk, qcow2 (itself over a base or other
// nodes) or raw (a frozen data volume).
//
// It copies src instead where an overlay couldn't be made or wouldn't pay:
// src is as deep as a chain may be (the copy has src's depth, one less than an
// overlay would), src can't be linked into the node store (another file
// system), or a raw src isn't a whole number of sectors (an overlay's size
// would be rounded up past the device the guest was saved with). own says src
// is the caller's to move, a fork's staging copy, so the copy is a rename.
//
// The caller holds the bases lock shared, until dst names what it was made
// over.
func (e *Engine) layerOn(dst, src string, own bool) error {
	real, err := resolveBasePath(src)
	if err != nil {
		return fmt.Errorf("disk %s: %w", src, err)
	}
	fi, err := os.Stat(real)
	if err != nil {
		return fmt.Errorf("disk %s: %w", src, err)
	}
	format, size, depth := "raw", uint64(fi.Size()), 0
	if isQcow2(real) {
		format = "qcow2"
		if size, err = qcow2VirtualSize(real); err != nil {
			return fmt.Errorf("disk %s: %w", src, err)
		}
		if depth, err = chainDepth(real); err != nil {
			return err
		}
	}
	switch {
	case depth+1 > maxChainDepth:
		slog.Debug("krucible.node.flatten", "disk", real, "depth", depth, "max", maxChainDepth)
		return moveOrClone(real, dst, own)
	case format == "raw" && size%512 != 0:
		return moveOrClone(real, dst, own)
	}
	node, err := e.nodeLink(real)
	if errors.Is(err, errNoNodeLink) {
		slog.Warn("krucible.node.copy", "disk", real, "reason", err)
		return moveOrClone(real, dst, own)
	}
	if err != nil {
		return err
	}
	return createQcow2OverlayOn(dst, node, format, size)
}

// moveOrClone puts the disk at src at dst: moved if the caller owns it and the
// two share a file system, else copied.
func moveOrClone(src, dst string, own bool) error {
	if own {
		if err := os.Rename(src, dst); err == nil {
			return nil
		}
	}
	return cloneFile(src, dst)
}

// diskRefs maps each file that some qcow2 image under dataDir backs onto,
// directly or through other images, to the images that do. It reads headers
// only. An image it can't read stops it: that image might name anything.
func diskRefs(dataDir string) (map[fileKey][]string, error) {
	chains := map[string][]string{}
	var chainOf func(path string, depth int) ([]string, error)
	chainOf = func(path string, depth int) ([]string, error) {
		if c, ok := chains[path]; ok {
			return c, nil
		}
		if depth > maxChainWalk {
			return nil, fmt.Errorf("%s: backing chain deeper than %d or looping", path, maxChainWalk)
		}
		b, err := qcow2BackingPath(path)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		var c []string
		if b != "" {
			c = []string{b}
			if isQcow2(b) {
				rest, err := chainOf(b, depth+1)
				if err != nil {
					return nil, err
				}
				c = append(c, rest...)
			}
		}
		chains[path] = c
		return c, nil
	}

	refs := map[fileKey][]string{}
	err := walkImages(dataDir, func(path string, link bool) error {
		if link {
			return nil
		}
		chain, err := chainOf(path, 0)
		if errors.Is(err, fs.ErrNotExist) {
			return nil // destroyed while the walk ran: nothing of it to protect
		}
		if err != nil {
			return fmt.Errorf("%w (an image that can't be read might name any node)", err)
		}
		for _, member := range chain {
			fi, err := os.Stat(member)
			if err != nil {
				continue // names a file that is gone: nothing to protect either
			}
			if k, ok := keyOf(fi); ok {
				refs[k] = append(refs[k], path)
			}
		}
		return nil
	})
	return refs, err
}

// sweepNodesLocked removes the nodes nothing needs, in rounds: taking a node
// out frees the nodes only it was layered over. With dryRun it reports the
// first round only. The caller holds the bases lock exclusively.
func sweepNodesLocked(dataDir string, dryRun bool) (removed []string, err error) {
	dir := nodesDir(dataDir)
	for range maxChainDepth + 1 {
		entries, err := os.ReadDir(dir)
		if errors.Is(err, fs.ErrNotExist) {
			return removed, nil
		}
		if err != nil {
			return removed, err
		}
		// A node some snapshot or image holds under another name has more than
		// one link: that holder keeps it, whatever names it.
		cand := map[fileKey]string{}
		for _, de := range entries {
			if strings.HasPrefix(de.Name(), ".") || !de.Type().IsRegular() {
				continue
			}
			path := filepath.Join(dir, de.Name())
			fi, err := os.Lstat(path)
			if err != nil {
				continue
			}
			if st, ok := fi.Sys().(*syscall.Stat_t); ok && st.Nlink == 1 {
				if k, ok := keyOf(fi); ok {
					cand[k] = path
				}
			}
		}
		if len(cand) == 0 {
			return removed, nil
		}
		refs, err := diskRefs(dataDir)
		if err != nil {
			return removed, fmt.Errorf("sweep nodes: %w", err)
		}
		var garbage []string
		for k, path := range cand {
			if len(refs[k]) == 0 {
				garbage = append(garbage, path)
			}
		}
		if len(garbage) == 0 {
			return removed, nil
		}
		slices.Sort(garbage)
		for _, path := range garbage {
			if !dryRun {
				if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
					return removed, err
				}
			}
			removed = append(removed, path)
		}
		if dryRun {
			return removed, nil
		}
	}
	return removed, nil
}

// SweepNodes removes the nodes nothing needs any more and returns them.
func SweepNodes(dataDir string, dryRun bool) ([]string, error) {
	unlock, err := lockBases(dataDir, true)
	if err != nil {
		return nil, err
	}
	defer unlock()
	return sweepNodesLocked(dataDir, dryRun)
}

// scheduleNodeSweep sweeps in the background after a sandbox that had disks
// over nodes is gone (or never came to be). Requests that arrive during a
// sweep fold into one more run: tearing down a fleet of clones asks once per
// clone, and each sweep reads every header under the data dir.
func (e *Engine) scheduleNodeSweep() {
	e.sweepMu.Lock()
	if e.sweeping {
		e.sweepAgain = true
		e.sweepMu.Unlock()
		return
	}
	e.sweeping = true
	e.sweepMu.Unlock()
	go func() {
		for {
			if removed, err := SweepNodes(e.cfg.DataDir, false); err != nil {
				slog.Warn("krucible.nodes.sweep_failed", "error", err)
			} else if len(removed) > 0 {
				slog.Info("krucible.nodes.swept", "removed", len(removed))
			}
			e.sweepMu.Lock()
			if !e.sweepAgain {
				e.sweeping = false
				e.sweepMu.Unlock()
				return
			}
			e.sweepAgain = false
			e.sweepMu.Unlock()
		}
	}()
}

// waitNodeSweep returns once no background sweep is running, for tests that
// check what one did.
func (e *Engine) waitNodeSweep() {
	for {
		e.sweepMu.Lock()
		idle := !e.sweeping
		e.sweepMu.Unlock()
		if idle {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// diskUsers counts what has disks layered over a snapshot's.
type diskUsers struct {
	Sandboxes int
	Snapshots int
	Images    int
	Other     int
}

func (u diskUsers) any() bool { return u.Sandboxes+u.Snapshots+u.Images+u.Other > 0 }

// String reads as "2 sandboxes and 1 snapshot".
func (u diskUsers) String() string {
	var parts []string
	count := func(n int, one, many string) {
		switch {
		case n == 1:
			parts = append(parts, "1 "+one)
		case n > 1:
			parts = append(parts, fmt.Sprintf("%d %s", n, many))
		}
	}
	count(u.Sandboxes, "sandbox", "sandboxes")
	count(u.Snapshots, "snapshot", "snapshots")
	count(u.Images, "saved image", "saved images")
	count(u.Other, "other disk", "other disks")
	switch len(parts) {
	case 0:
		return "nothing"
	case 1:
		return parts[0]
	}
	return strings.Join(parts[:len(parts)-1], ", ") + " and " + parts[len(parts)-1]
}

// usersOf counts what has disks layered over any file in dir: the disks of
// sandboxes (counted once per sandbox, whatever it attaches), other
// snapshots, saved images. Nodes stand between and aren't counted; what holds
// them is. Files in dir itself don't count.
func usersOf(dataDir, dir string) (diskUsers, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return diskUsers{}, err
	}
	targets := map[fileKey]bool{}
	for _, de := range entries {
		if !de.Type().IsRegular() {
			continue
		}
		if fi, err := os.Stat(filepath.Join(dir, de.Name())); err == nil {
			if k, ok := keyOf(fi); ok {
				targets[k] = true
			}
		}
	}
	refs, err := diskRefs(dataDir)
	if err != nil {
		return diskUsers{}, err
	}
	sandboxes, snapshots := map[string]bool{}, map[string]bool{}
	var u diskUsers
	seen := map[string]bool{}
	for k := range targets {
		for _, path := range refs[k] {
			if seen[path] || within(dir, path) {
				continue
			}
			seen[path] = true
			rel, err := filepath.Rel(dataDir, path)
			parts := strings.Split(rel, string(filepath.Separator))
			switch {
			case err != nil || len(parts) < 2:
				u.Other++
			case parts[0] == "nodes":
			case parts[0] == "sandboxes":
				sandboxes[parts[1]] = true
			case parts[0] == "snapshots" && len(parts) >= 4:
				snapshots[parts[1]+"/"+parts[2]] = true
			case parts[0] == "images":
				u.Images++
			default:
				u.Other++
			}
		}
	}
	u.Sandboxes, u.Snapshots = len(sandboxes), len(snapshots)
	return u, nil
}

// RemoveSnapshot implements engine.SnapshotRemover: it deletes the snapshot
// directory snapDir unless a disk is layered over one of its files. A snapshot
// that is gone already is removed.
func (e *Engine) RemoveSnapshot(ctx context.Context, snapDir string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	dataDir, err := filepath.Abs(e.cfg.DataDir)
	if err != nil {
		return err
	}
	snapDir, err = filepath.Abs(snapDir)
	if err != nil {
		return err
	}
	// Exclusive from the check to the removal: a resume that has picked this
	// snapshot's disk finishes making its overlay first, and is counted; one
	// that comes after finds the directory gone.
	unlock, err := lockBasesCtx(ctx, dataDir, true)
	if err != nil {
		return err
	}
	defer unlock()
	if _, err := os.Stat(snapDir); err == nil {
		// Nodes nothing needs would otherwise count against the snapshot they
		// were layered over.
		if _, err := sweepNodesLocked(dataDir, false); err != nil {
			return err
		}
		users, err := usersOf(dataDir, snapDir)
		if err != nil {
			return fmt.Errorf("remove snapshot: %w", err)
		}
		if users.any() {
			return fmt.Errorf("%w by %s, whose disks are layered over this snapshot's; destroy them first", engine.ErrSnapshotInUse, users)
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	if err := os.RemoveAll(snapDir); err != nil {
		return err
	}
	// Its links are gone: the nodes it was the last to hold go too.
	if _, err := sweepNodesLocked(dataDir, false); err != nil {
		slog.Warn("krucible.nodes.sweep_failed", "error", err)
	}
	return nil
}

// prepareNodeRoot makes a resumed or forked sandbox's root disk in sandboxDir:
// an overlay over the snapshot's disk. It returns what prepareRootDisk does,
// with the snapshot's disk in the place of the base.
func (e *Engine) prepareNodeRoot(sandboxDir, disk string, own bool) (root, format, base string, err error) {
	if !rootQcow2() {
		return "", "", "", fmt.Errorf("a snapshot restores onto a qcow2 root, and KRUCIBLE_ROOT_RAW is set")
	}
	unlock, err := lockBases(e.cfg.DataDir, false)
	if err != nil {
		return "", "", "", fmt.Errorf("lock base images: %w", err)
	}
	defer unlock()
	root = filepath.Join(sandboxDir, "root.qcow2")
	if err := e.layerOn(root, disk, own); err != nil {
		return "", "", "", fmt.Errorf("layer the root disk over the snapshot's: %w", err)
	}
	return root, "qcow2", disk, nil
}

// layerRestoredVolumes attaches each frozen volume of the snapshot being
// restored to spec: as a disk in sandboxDir over it, so none is copied and
// writes stay in this sandbox. A read-only volume gets one too: what the
// helper opens is then never the frozen file itself, and the overlay is what
// names it, where the scan finds it.
func (e *Engine) layerRestoredVolumes(sandboxDir string, spec *VMSpec, vols []restoreVol, own bool) error {
	if len(vols) == 0 {
		return nil
	}
	unlock, err := lockBases(e.cfg.DataDir, false)
	if err != nil {
		return fmt.Errorf("lock base images: %w", err)
	}
	defer unlock()
	for _, rv := range vols {
		idx := len(spec.Volumes)
		dst := filepath.Join(sandboxDir, fmt.Sprintf("restorevol%d.img", idx))
		if err := e.layerOn(dst, rv.path, own); err != nil {
			return fmt.Errorf("restore volume %d: %w", idx, err)
		}
		format := "raw"
		if isQcow2(dst) {
			format = "qcow2"
		}
		spec.Volumes = append(spec.Volumes, VMVolume{BlockID: fmt.Sprintf("vol%d", idx), Path: dst, Format: format, ReadOnly: rv.readOnly})
	}
	return nil
}

// diskBacking lists the files the chains of spec's disks name that this host
// doesn't keep as images (VMSpec.Backing): the nodes layered over, and what
// they name in turn. The images under <DataDir>/images are admitted by
// where they are (vmmBaseAllowed), so a sandbox over a base alone has none.
func (e *Engine) diskBacking(spec VMSpec) ([]string, error) {
	images := filepath.Join(e.cfg.DataDir, "images")
	var out []string
	add := func(path string) error {
		if !isQcow2(path) {
			return nil
		}
		chain, err := backingChain(path)
		if err != nil {
			return err
		}
		for _, b := range chain {
			if !within(images, b) && !slices.Contains(out, b) {
				out = append(out, b)
			}
		}
		return nil
	}
	if spec.RootDiskFormat == "qcow2" {
		if err := add(spec.RootDisk); err != nil {
			return nil, err
		}
	}
	for _, v := range spec.Volumes {
		if err := add(v.Path); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// adoptNode returns the name in the node store of the disk at path, which an
// import has unpacked and is done with: linked there, or copied in where a
// link can't reach.
func (e *Engine) adoptNode(path string) (string, error) {
	node, err := e.nodeLink(path)
	if !errors.Is(err, errNoNodeLink) {
		return node, err
	}
	dir, err := filepath.Abs(nodesDir(e.cfg.DataDir))
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	tmp, err := os.CreateTemp(dir, ".adopt-*")
	if err != nil {
		return "", err
	}
	tmp.Close()
	defer os.Remove(tmp.Name())
	if err := cloneFile(path, tmp.Name()); err != nil {
		return "", err
	}
	return e.nodeLink(tmp.Name())
}
