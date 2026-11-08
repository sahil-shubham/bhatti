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
// Guest-writable headers cannot be a dependency authority. Each captured disk
// carries the daemon's typed chain, including the terminal raw image, so GC
// and a helper's access policy agree without interpreting raw guest bytes.
// Legacy disks can be inspected only within their owner's existing authority.
//
// The same lock as bases (lockBases) orders all of it: a create holds it
// shared from picking a node until the overlay names it, and a sweep or a
// snapshot removal holds it exclusively.

import (
	"context"
	"encoding/json"
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
// and a file the helper holds open, so metadata validation and confinement
// enforce the same bound.
//
// A resume or fork that would go past it copies the node's own layer as the
// new disk instead of layering over it: the copy has the node's depth, not one
// more, and costs the size of that one layer, not of everything below it. So
// the cap never refuses anything; a long lineage just pays what every resume
// paid before nodes, once per generation past the cap.
const maxChainDepth = 8

// nodesDir is the node store.
func nodesDir(dataDir string) string { return filepath.Join(dataDir, "nodes") }

// nodeName is a node's name in the store: the identity of its file, which no
// other file has while this one exists, and the link itself keeps it so.
func nodeName(k fileKey) string { return fmt.Sprintf("%x-%x.img", k.dev, k.ino) }

type nodeMetadata struct {
	Format  string      `json:"format"`
	Backing []diskLayer `json:"backing"`
}

func nodeMetadataPath(node string) string {
	return filepath.Join(filepath.Dir(node), "."+filepath.Base(node)+".json")
}

func removeNodeMetadata(node string) error {
	err := os.Remove(nodeMetadataPath(node))
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	// Retrying a failed directory sync must persist even an already absent
	// sidecar before the last disk link can disappear and its inode be reused.
	syncErr := syncNodeDirectory(filepath.Dir(node))
	if errors.Is(err, fs.ErrNotExist) && errors.Is(syncErr, fs.ErrNotExist) {
		return nil
	}
	return syncErr
}

// The store can outlive a snapshot's managed name, including snapshots kept
// outside DataDir. Its daemon-only graph keeps that hard link meaningful
// without ever reinterpreting a raw node's guest-writable bytes.
func (e *Engine) recordNodeMetadata(node, format string, backing []diskLayer) error {
	if err := validateNodeMetadata(node, format, backing); err != nil {
		return err
	}
	return writeDaemonMetadata(nodeMetadataPath(node), nodeMetadata{Format: format, Backing: backing})
}

func validateNodeMetadata(node, format string, backing []diskLayer) error {
	if format != "raw" && format != "qcow2" {
		return fmt.Errorf("%s: unsupported node format %q", node, format)
	}
	if format == "raw" && len(backing) != 0 {
		return fmt.Errorf("%s: raw node has recorded backing files", node)
	}
	return nil
}

// errNoNodeLink says a disk can't be given a name in the node store.
var errNoNodeLink = errors.New("can't link the disk into the node store")

// linkNode is os.Link, for a test that needs a link to fail.
var linkNode = os.Link

// Publication barriers are injectable so tests can stop a publisher without
// running its cleanup, or fail a copied inode's sync before it is exposed.
var syncNodeDir = syncNodeDirectory
var syncNodeFile = flushFiles
var renameNode = os.Rename

func syncNodeDirectory(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

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
	if err := e.prepareNodeAccess(real); err != nil {
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
	// Another creator may have linked the same file but not synced it yet.
	// Reusing that name must publish it before this caller drops its source.
	if err := syncNodeDir(dir); err != nil {
		return "", err
	}
	return dst, nil
}

// A pending record is durable while its private inode pin still exists. The
// final sidecar therefore cannot outlive its inode and authorize a reused
// identity, even if publication stops before exposing the final name.
type nodePublication struct {
	Node    string      `json:"node"`
	Format  string      `json:"format"`
	Backing []diskLayer `json:"backing"`
}

func nodePendingPath(pin string) string { return pin + ".json" }

func nodePendingName(name string) bool {
	pin, ok := strings.CutSuffix(name, ".json")
	return ok && (numberedStage(pin, ".publish-", "") || numberedStage(pin, ".adopt-", ""))
}

func readNodePublication(path string) (nodePublication, error) {
	var p nodePublication
	found, err := readDaemonMetadata(path, &p)
	if err != nil {
		return p, err
	}
	if !found {
		return p, fs.ErrNotExist
	}
	if p.Node == "" || filepath.Base(p.Node) != p.Node || strings.HasPrefix(p.Node, ".") ||
		!strings.HasSuffix(p.Node, ".img") {
		return p, fmt.Errorf("%s: invalid pending node %q", path, p.Node)
	}
	return p, nil
}

// linkNodeMetadata is the production hard-link path. A failed first link has
// exposed no authority and can still use the caller's copy fallback.
func (e *Engine) linkNodeMetadata(src, format string, backing []diskLayer) (string, error) {
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
	if _, ok := keyOf(fi); !ok {
		return "", fmt.Errorf("%w: no file identity here", errNoNodeLink)
	}
	dir, err := filepath.Abs(nodesDir(e.cfg.DataDir))
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	if err := e.prepareNodeAccess(real); err != nil {
		return "", err
	}
	tmp, err := os.CreateTemp(dir, ".publish-*")
	if err != nil {
		return "", err
	}
	pin := tmp.Name()
	if err := tmp.Close(); err != nil {
		_ = os.Remove(pin)
		return "", err
	}
	if err := os.Remove(pin); err != nil {
		return "", err
	}
	if err := linkNode(real, pin); err != nil {
		return "", fmt.Errorf("%w: %v", errNoNodeLink, err)
	}
	// Received raw layers have not necessarily crossed any file barrier yet.
	if err := syncNodeFile(pin); err != nil {
		_ = os.Remove(pin)
		return "", err
	}
	return e.commitNode(pin, format, backing, false)
}

// commitNode owns pin, either a private hard link or a synced adopted copy.
// Once pending metadata may exist, only exclusive recovery removes leftovers:
// a failed shared-lock publisher must never unlink another publisher's node.
func (e *Engine) commitNode(pin, format string, backing []diskLayer, move bool) (string, error) {
	recoverable := false
	defer func() {
		if !recoverable {
			_ = os.Remove(pin)
		}
	}()
	fi, err := os.Lstat(pin)
	if err != nil {
		return "", err
	}
	k, ok := keyOf(fi)
	if !ok || !fi.Mode().IsRegular() {
		return "", fmt.Errorf("%s: no regular node identity", pin)
	}
	dir := filepath.Dir(pin)
	node := filepath.Join(dir, nodeName(k))
	if err := validateNodeMetadata(node, format, backing); err != nil {
		return "", err
	}
	if have, err := os.Lstat(node); err == nil {
		if !os.SameFile(fi, have) {
			return "", fmt.Errorf("node store entry %s is not %s", node, pin)
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return "", err
	}
	p := nodePublication{Node: filepath.Base(node), Format: format, Backing: backing}
	recoverable = true
	// This directory sync also persists the pin before any final sidecar.
	if err := writeDaemonMetadata(nodePendingPath(pin), p); err != nil {
		return "", err
	}
	if err := e.recordNodeMetadata(node, format, backing); err != nil {
		return "", err
	}
	if move {
		if have, err := os.Lstat(node); err == nil {
			if !os.SameFile(fi, have) {
				return "", fmt.Errorf("node store entry %s is not %s", node, pin)
			}
		} else if !errors.Is(err, fs.ErrNotExist) {
			return "", err
		} else if err := renameNode(pin, node); err != nil {
			return "", err
		}
	} else {
		switch err := linkNode(pin, node); {
		case err == nil:
		case errors.Is(err, fs.ErrExist):
			have, serr := os.Lstat(node)
			if serr != nil || !os.SameFile(fi, have) {
				return "", fmt.Errorf("node store entry %s is not %s", node, pin)
			}
		default:
			return "", err
		}
	}
	// EEXIST is not a durability barrier: each concurrent publisher must sync.
	if err := syncNodeDir(dir); err != nil {
		return "", err
	}
	if err := finishNodePublication(pin); err != nil {
		return "", err
	}
	return node, nil
}

func finishNodePublication(pin string) error {
	if err := os.Remove(nodePendingPath(pin)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	if err := os.Remove(pin); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return syncNodeDir(filepath.Dir(pin))
}

// Recover before deleting any fork/normalization stages: their pending nodes
// may be the only daemon-recorded graph for an external snapshot.
func reconcileNodePublications(dataDir string) error {
	dir := nodesDir(dataDir)
	entries, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	e := &Engine{cfg: Config{DataDir: dataDir}}
	for _, de := range entries {
		if !nodePendingName(de.Name()) {
			continue
		}
		pending := filepath.Join(dir, de.Name())
		p, err := readNodePublication(pending)
		if err != nil {
			return err
		}
		pin := strings.TrimSuffix(pending, ".json")
		node := filepath.Join(dir, p.Node)
		fi, err := os.Lstat(pin)
		if errors.Is(err, fs.ErrNotExist) {
			fi, err = os.Lstat(node)
			if errors.Is(err, fs.ErrNotExist) {
				if err := removeNodeMetadata(node); err != nil {
					return err
				}
				if err := finishNodePublication(pin); err != nil {
					return err
				}
				continue
			}
			if err == nil {
				k, ok := keyOf(fi)
				if !ok || nodeName(k) != p.Node {
					return fmt.Errorf("%s: pending node identity changed", pending)
				}
			}
		}
		if err != nil {
			return err
		}
		if !fi.Mode().IsRegular() {
			return fmt.Errorf("%s: pending node is not regular", pending)
		}
		if have, err := os.Lstat(node); err == nil {
			if !os.SameFile(fi, have) {
				return fmt.Errorf("node store entry %s is not %s", node, pin)
			}
		} else if !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		if err := e.recordNodeMetadata(node, p.Format, p.Backing); err != nil {
			return err
		}
		if _, err := os.Lstat(node); errors.Is(err, fs.ErrNotExist) {
			if err := renameNode(pin, node); err != nil {
				return err
			}
		} else if err != nil {
			return err
		}
		if err := syncNodeDir(dir); err != nil {
			return err
		}
		if err := finishNodePublication(pin); err != nil {
			return err
		}
	}
	return nil
}

// readDiskChain is used for newly constructed disks, never to replace a
// daemon-recorded chain. Unknown top formats retain legacy inference.
func readDiskChain(path, format string, authorize func(string) error) ([]diskLayer, error) {
	legacy := format == ""
	if format == "raw" {
		return []diskLayer{}, nil
	}
	if format != "" && format != "qcow2" {
		return nil, fmt.Errorf("%s: unsupported format %q", path, format)
	}
	if legacy && authorize != nil {
		if err := authorize(path); err != nil {
			return nil, err
		}
	}
	if legacy {
		format = "raw"
		if maybeQcow2(path) {
			format = "qcow2"
		}
	}
	chain := []diskLayer{}
	depth := 0
	for format == "qcow2" {
		depth++
		if depth > maxChainDepth {
			return nil, fmt.Errorf("%s: backing chain deeper than %d", path, maxChainDepth)
		}
		next, err := qcow2BackingPath(path)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		nextFormat, err := qcow2BackingFormat(path)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		if next == "" {
			return chain, nil
		}
		if authorize != nil {
			if err := authorize(next); err != nil {
				return nil, err
			}
		}
		if nextFormat == "" {
			if !legacy {
				return nil, fmt.Errorf("%s: backing file %s has no recorded format", path, next)
			}
			nextFormat = "raw"
			if maybeQcow2(next) {
				nextFormat = "qcow2"
			}
		}
		if nextFormat != "raw" && nextFormat != "qcow2" {
			return nil, fmt.Errorf("%s: unsupported backing format %q", path, nextFormat)
		}
		chain = append(chain, diskLayer{Path: next, Format: nextFormat})
		path, format = next, nextFormat
	}
	return chain, nil
}

// layerOn makes the disk at dst over the immutable disk at src and returns
// dst's format. The source's already-authorized metadata supplies format;
// guest bytes must never select a driver for an explicitly raw disk.
//
// It copies src instead where an overlay couldn't be made or wouldn't pay:
// src is as deep as a chain may be (the copy has src's depth, one less than an
// overlay would), src can't be linked into the node store (another file
// system), a relative backing name would change meaning in the store, or a
// raw src isn't a whole number of sectors (an overlay's size would be rounded
// up past the saved device). own says src is the caller's to move, a fork's
// staging copy, so the copy is a rename.
//
// The caller holds the bases lock shared, until dst names what it was made
// over.
func (e *Engine) layerOn(dst, src string, own bool, format string) (string, error) {
	real, err := resolveBasePath(src)
	if err != nil {
		return "", fmt.Errorf("disk %s: %w", src, err)
	}
	fi, err := os.Stat(real)
	if err != nil {
		return "", fmt.Errorf("disk %s: %w", src, err)
	}
	size, depth := uint64(fi.Size()), 0
	var chain []diskLayer
	switch format {
	case "qcow2":
		if size, err = qcow2VirtualSize(real); err != nil {
			return "", fmt.Errorf("disk %s: %w", src, err)
		}
		chain, err = readDiskChain(real, format, nil)
		if err != nil {
			return "", err
		}
		depth = 1 + len(chain)
		if len(chain) > 0 && chain[len(chain)-1].Format == "raw" {
			depth--
		}
		backing, err := qcow2Backing(real)
		if err != nil {
			return "", err
		}
		if backing != "" && !filepath.IsAbs(backing) {
			// A hard link cannot rebase a header without changing the frozen
			// source too. Only the new disk gets an absolute backing name.
			return format, moveOrClone(real, dst, own, format)
		}
	case "raw":
	default:
		return "", fmt.Errorf("disk %s: unsupported format %q", src, format)
	}
	switch {
	case depth+1 > maxChainDepth:
		slog.Debug("krucible.node.flatten", "disk", real, "depth", depth, "max", maxChainDepth)
		return format, moveOrClone(real, dst, own, format)
	case format == "raw" && size%512 != 0:
		return format, moveOrClone(real, dst, own, format)
	}
	node, err := e.linkNodeMetadata(real, format, chain)
	if errors.Is(err, errNoNodeLink) {
		slog.Warn("krucible.node.copy", "disk", real, "reason", err)
		return format, moveOrClone(real, dst, own, format)
	}
	if err != nil {
		return "", err
	}
	return "qcow2", createQcow2OverlayOn(dst, node, format, size)
}

// moveOrClone puts the disk at src at dst: moved if the caller owns it and the
// two share a file system, else copied. A relative backing name must keep its
// old meaning when the containing disk changes directories.
func moveOrClone(src, dst string, own bool, format string) error {
	var backing string
	if format == "qcow2" {
		name, err := qcow2Backing(src)
		if err != nil {
			return err
		}
		if name != "" && !filepath.IsAbs(name) {
			backing, err = filepath.Abs(filepath.Join(filepath.Dir(src), name))
			if err != nil {
				return err
			}
		}
	}
	moved := false
	if own {
		moved = os.Rename(src, dst) == nil
	}
	if !moved {
		if err := cloneFile(src, dst); err != nil {
			return err
		}
	}
	if backing != "" {
		return setQcow2Backing(dst, backing)
	}
	return nil
}

// walkNodeImages also follows linked disk directories: a data directory moved
// onto another disk, or an images/volume directory linked out of it, must not
// make the nodes its disks name look unused. Root-level daemon state is still
// never sniffed, for the same SQLite-lock reason as walkImages.
func walkNodeImages(dataDir string, fn func(path string, link bool) error) error {
	dataDir, err := filepath.Abs(dataDir)
	if err != nil {
		return err
	}
	return walkManagedDiskFiles(dataDir, dataDir, fn)
}

func walkImageMetadata(dataDir string, fn func(path, disk string) error) error {
	dataDir, err := filepath.Abs(dataDir)
	if err != nil {
		return err
	}
	images := filepath.Join(dataDir, "images")
	return walkManagedDiskFiles(dataDir, images, func(path string, _ bool) error {
		name := filepath.Base(path)
		if !strings.HasPrefix(name, ".") || !strings.HasSuffix(name, ".json") {
			return nil
		}
		disk := filepath.Join(filepath.Dir(path), strings.TrimSuffix(strings.TrimPrefix(name, "."), ".json"))
		return fn(path, disk)
	})
}

func managedSnapshotDirs(dataDir string) ([]string, error) {
	managedRoot := filepath.Join(dataDir, "snapshots")
	root, err := filepath.EvalSymlinks(managedRoot)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var dirs []string
	seen := map[string]bool{}
	add := func(dir string) error {
		rel, err := filepath.Rel(root, dir)
		if err != nil {
			return err
		}
		alias := filepath.Join(managedRoot, rel)
		if !seen[alias] {
			seen[alias] = true
			dirs = append(dirs, alias)
		}
		return nil
	}
	// The shared root may be relocated, but nested aliases must not turn a
	// helper-writable save directory into a source of daemon disk authority.
	err = filepath.WalkDir(root, func(path string, de fs.DirEntry, err error) error {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		if de.IsDir() {
			if filepath.Dir(filepath.Dir(path)) == root {
				return add(path) // Legacy owner/name directories can lack a manifest.
			}
			return nil
		}
		if de.Type().IsRegular() && de.Name() == snapshotManifestFile {
			return add(filepath.Dir(path))
		}
		return nil
	})
	return dirs, err
}

func walkManagedDiskFiles(dataDir, root string, fn func(path string, link bool) error) error {
	active := map[fileKey]bool{}
	var walk func(string, bool) error
	walk = func(dir string, linked bool) error {
		fi, err := os.Stat(dir)
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			return err
		}
		k, ok := keyOf(fi)
		if !ok {
			return fmt.Errorf("%s: no directory identity here", dir)
		}
		if active[k] {
			return nil
		}
		active[k] = true
		defer delete(active, k)
		// Lstat on a trailing "/." follows a directory symlink, unlike on
		// the directory name alone. Child paths retain the managed alias.
		return filepath.WalkDir(dir+string(filepath.Separator)+".", func(path string, de fs.DirEntry, err error) error {
			if err != nil {
				if errors.Is(err, fs.ErrNotExist) && filepath.Clean(path) != dataDir {
					return nil
				}
				return err
			}
			switch {
			case path == basesDir(dataDir):
				if de.IsDir() {
					return filepath.SkipDir
				}
				return nil
			case de.IsDir():
				return nil
			case filepath.Dir(path) == dataDir && de.Type()&fs.ModeSymlink == 0:
				return nil
			case de.Type()&fs.ModeSymlink != 0:
				fi, err := os.Stat(path)
				if errors.Is(err, fs.ErrNotExist) {
					return nil
				}
				if err != nil {
					return err
				}
				if fi.IsDir() {
					return walk(path, linked || !within(nodesDir(dataDir), path))
				}
				if fi.Mode().IsRegular() && filepath.Dir(path) != dataDir {
					return fn(path, true)
				}
			case de.Type().IsRegular():
				return fn(path, linked)
			}
			return nil
		})
	}
	return walk(root, false)
}

type diskMetadata struct {
	format  string
	backing []diskLayer
}

type diskRoot struct {
	path, holder, owner string
	metadata            diskMetadata
	legacy              bool
}

type diskCatalog struct {
	metadata    map[fileKey]diskMetadata
	storedNodes map[fileKey]*storedNode
	roots       []diskRoot
}

// The catalog reads daemon metadata only. Inode identity makes its format
// decision survive an alias outside the disk's managed directory.
func readDiskCatalog(dataDir string) (*diskCatalog, error) {
type storedNode struct {
	names []string
	count uint64
	links uint64
}

// Count physical regular names in the store exactly once. Directory aliases
// are roots, not additional hard links; hidden publication pins count too.
func readNodeStore(dataDir string) (map[fileKey]*storedNode, []string, error) {
	dir := nodesDir(dataDir)
	entries, err := os.ReadDir(dir)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, nil, err
	}
	nodes := map[fileKey]*storedNode{}
	var pending []string
	for _, de := range entries {
		path := filepath.Join(dir, de.Name())
		if nodePendingName(de.Name()) {
			pending = append(pending, path)
		}
		if !de.Type().IsRegular() {
			continue
		}
		fi, err := os.Lstat(path)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, nil, err
		}
		k, ok := keyOf(fi)
		st, statOK := fi.Sys().(*syscall.Stat_t)
		if !ok || !statOK || !fi.Mode().IsRegular() {
			continue
		}
		n := nodes[k]
		if n == nil {
			n = &storedNode{links: uint64(st.Nlink)}
			nodes[k] = n
		}
		n.count++
		if !strings.HasPrefix(de.Name(), ".") {
			n.names = append(n.names, path)
		}
	}
	return nodes, pending, nil
}

	dataDir, err := filepath.Abs(dataDir)
	if err != nil {
		return nil, err
	}
	metadata := map[fileKey]diskMetadata{}
	var roots []diskRoot
	register := func(path, format string, backing []diskLayer) error {
		for {
			if format != "raw" && format != "qcow2" {
				return fmt.Errorf("%s: unsupported disk format %q", path, format)
			}
			if format == "raw" && len(backing) != 0 {
				return fmt.Errorf("%s: raw disk has recorded backing files", path)
			}
			fi, err := os.Stat(path)
			if err != nil && !errors.Is(err, fs.ErrNotExist) {
				return err
			}
			if err == nil && fi.Mode().IsRegular() {
				if k, ok := keyOf(fi); ok {
					if _, exists := metadata[k]; !exists || format == "raw" {
						metadata[k] = diskMetadata{format: format, backing: backing}
					}
				}
			}
			if len(backing) == 0 {
				return nil
			}
			path, format, backing = backing[0].Path, backing[0].Format, backing[1:]
		}
	}
	storedNodes, pending, err := readNodeStore(dataDir)
	if err != nil {
		return nil, err
	}
	for _, n := range storedNodes {
		for _, path := range n.names {
			var m nodeMetadata
			found, err := readDaemonMetadata(nodeMetadataPath(path), &m)
			if err != nil {
				return nil, err
			}
			if found {
				if err := register(path, m.Format, m.Backing); err != nil {
					return nil, err
				}
			}
		}
	}
	// Dry runs cannot reconcile a crashed publisher. Its daemon record still
	// roots the pinned graph without promoting global sidecars to ownership.
	for _, path := range pending {
		p, err := readNodePublication(path)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		pin := strings.TrimSuffix(path, ".json")
		if _, err := os.Lstat(pin); errors.Is(err, fs.ErrNotExist) {
			pin = filepath.Join(nodesDir(dataDir), p.Node)
		} else if err != nil {
			return nil, err
		}
		if err := register(pin, p.Format, p.Backing); err != nil {
			return nil, err
		}
	}
	root := func(path, holder, owner, format string, backing []diskLayer, legacy bool) error {
		if path == "" {
			return nil
		}
		if !legacy {
			if err := register(path, format, backing); err != nil {
				return err
			}
		roots = append(roots, diskRoot{path: pin, holder: path,
			metadata: diskMetadata{format: p.Format, backing: p.Backing}})
		}
		roots = append(roots, diskRoot{path: path, holder: holder, owner: owner,
			metadata: diskMetadata{format: format, backing: backing}, legacy: legacy})
		return nil
	}
	snapshots, err := managedSnapshotDirs(dataDir)
	if err != nil {
		return nil, err
	}
	for _, dir := range snapshots {
		fi, err := os.Stat(dir)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if !fi.IsDir() {
			continue
		}
		m, found, err := readSnapshotManifest(dir)
		if err != nil {
			return nil, err
		}
		owner := ""
		rel, err := filepath.Rel(filepath.Join(dataDir, "snapshots"), dir)
		if err != nil {
			return nil, err
		}
		if first, _, nested := strings.Cut(rel, string(filepath.Separator)); nested {
			owner = first
		}
		disk := m.DiskFile
		if disk == "" {
			disk = "rootfs.qcow2"
		}
		if !archiveFileName.MatchString(disk) {
			return nil, fmt.Errorf("%s: invalid disk name %q", dir, disk)
		}
		if err := root(filepath.Join(dir, disk), filepath.Join(dir, disk), owner,
			m.DiskFormat, m.Backing, !found || m.DiskFormat == ""); err != nil {
			return nil, err
		}
		if !found {
			files, err := os.ReadDir(dir)
			if err != nil {
				return nil, err
			}
			for _, file := range files {
				if numberedStage(file.Name(), "vol", ".img") {
					m.Volumes = append(m.Volumes, snapVolume{File: file.Name()})
				}
			}
		}
		for _, v := range m.Volumes {
			if !archiveFileName.MatchString(v.File) {
				return nil, fmt.Errorf("%s: invalid volume name %q", dir, v.File)
			}
			legacy := v.Format == "" || (v.Format == "qcow2" && m.DiskFormat == "")
			if err := root(filepath.Join(dir, v.File), filepath.Join(dir, v.File), owner,
				v.Format, v.Backing, legacy); err != nil {
				return nil, err
			}
		}
	}
	records, err := filepath.Glob(filepath.Join(dataDir, "sandboxes", "*", stateFile))
	if err != nil {
		return nil, err
	}
	for _, path := range records {
		data, err := os.ReadFile(path)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		var rec vmRecord
		if err := json.Unmarshal(data, &rec); err != nil {
			return nil, fmt.Errorf("read disk roots from %s: %w", path, err)
		}
		spec := rec.BaseSpec
		format := spec.RootDiskFormat
		if format == "" {
			format = "raw" // The old root device API opened an omitted format as raw.
		}
		if err := root(spec.RootDisk, path, rec.UserID, format, spec.Backing,
			format == "qcow2" && spec.Backing == nil); err != nil {
			return nil, err
		}
		for _, v := range spec.Volumes {
			format := v.Format
			if format == "" {
				format = "raw"
			}
			if err := root(v.Path, path, rec.UserID, format, v.Backing,
				format == "qcow2" && v.Backing == nil); err != nil {
				return nil, err
			}
		}
	}
	err = walkImageMetadata(dataDir, func(path, disk string) error {
		fi, err := os.Stat(disk)
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		if !fi.Mode().IsRegular() {
			return nil
		}
		var m nodeMetadata
		found, err := readDaemonMetadata(path, &m)
		if err != nil || !found {
			return err
		}
		return register(disk, m.Format, m.Backing)
	})
	if err != nil {
		return nil, err
	}
	return &diskCatalog{metadata: metadata, storedNodes: storedNodes, roots: roots}, nil
}

// diskRefs uses the daemon's graph, not a guest's current headers. Metadata is
// indexed before walking aliases so raw nodes cannot be reclassified, and
// store-only nodes are never roots.
func diskRefs(dataDir string) (map[fileKey][]string, error) {
	dataDir, err := filepath.Abs(dataDir)
	if err != nil {
		return nil, err
	}
	catalog, err := readDiskCatalog(dataDir)
	if err != nil {
		return nil, err
	}
	metadata, storedNodes, roots := catalog.metadata, catalog.storedNodes, catalog.roots
	refs := map[fileKey][]string{}
	addFile := func(path, holder string) (bool, error) {
		fi, err := os.Stat(path)
		if errors.Is(err, fs.ErrNotExist) {
			return false, nil
		}
		if err != nil {
			return false, err
		}
		if !fi.Mode().IsRegular() {
			return false, nil
		}
		if k, ok := keyOf(fi); ok {
			refs[k] = append(refs[k], holder)
		}
		return true, nil
	}
	add := func(path, holder string, backing []diskLayer) error {
		if found, err := addFile(path, holder); err != nil || !found {
			return err
		}
		for _, layer := range backing {
			if _, err := addFile(layer.Path, holder); err != nil {
				return err
			}
		}
		return nil
	}
	e := &Engine{cfg: Config{DataDir: dataDir}}
	owners := map[string]map[fileKey]bool{}
	legacyChain := func(path, owner string, topRecorded bool) ([]diskLayer, error) {
		owned, ok := owners[owner]
		if !ok {
			var err error
			owned, err = ownedNodeKeys(dataDir, owner)
			if err != nil {
				return nil, err
			}
			owners[owner] = owned
		}
		vm := &VM{UserID: owner}
		return readDiskChain(path, "", func(next string) error {
			if topRecorded && next == path {
				return nil
			}
			return e.authorizedBacking(vm, next, owned)
		})
	}
	inspected := map[fileKey]bool{}
	for _, r := range roots {
		backing := r.metadata.backing
		known := !r.legacy
		if r.legacy {
			fi, err := os.Stat(r.path)
			if errors.Is(err, fs.ErrNotExist) {
				continue
			}
			if err != nil {
				return nil, err
			}
			if k, ok := keyOf(fi); ok {
				if m, found := metadata[k]; found {
					backing, known = m.backing, true
				}
			}
		}
		if !known {
			// Inferred edges never authorize another legacy disk and are
			// never promoted into a new provenance record.
			backing, err = legacyChain(r.path, r.owner, filepath.Base(r.holder) == stateFile)
			if errors.Is(err, fs.ErrNotExist) {
				continue
			}
			if err != nil {
				return nil, fmt.Errorf("%s: %w", r.path, err)
			}
			if fi, err := os.Stat(r.path); err == nil {
				if k, ok := keyOf(fi); ok {
					inspected[k] = true
				}
			}
		}
		if err := add(r.path, r.holder, backing); err != nil {
			return nil, err
		}
	}
	err = walkNodeImages(dataDir, func(path string, link bool) error {
		if strings.HasPrefix(filepath.Base(path), ".") {
			return nil
		}
		fi, err := os.Stat(path)
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		if k, ok := keyOf(fi); ok {
			if n := storedNodes[k]; !link && within(nodesDir(dataDir), path) && n != nil && n.links <= n.count {
				return nil
			}
			if m, found := metadata[k]; found {
				return add(path, path, m.backing)
			}
			if inspected[k] {
				return add(path, path, nil)
			}
			if n := storedNodes[k]; n != nil && len(n.names) != 0 {
				return fmt.Errorf("%s: live node has no daemon-recorded disk metadata", path)
			}
		}
		rel, err := filepath.Rel(dataDir, path)
		if err != nil {
			return err
		}
		parts := strings.Split(rel, string(filepath.Separator))
		if len(parts) >= 2 && parts[0] == "images" && e.vmmBaseAllowed(&VM{UserID: parts[1]}, path) {
			owner := ""
			if len(parts) > 2 && parts[1] != "bases" {
				owner = parts[1]
			}
			backing, err := legacyChain(path, owner, false)
			if err != nil {
				return err
			}
			return add(path, path, backing)
		}
		if link || (len(parts) >= 2 && (parts[0] == "nodes" || parts[0] == "volumes")) {
			// An untyped alias holds its inode, not whatever bytes in it
			// resemble a backing header.
			return add(path, path, nil)
		}
		if len(parts) >= 3 && parts[0] == "sandboxes" && filepath.Base(path) == "root.qcow2" {
			for _, r := range roots {
				if r.path == path {
					return nil
				}
			}
			backing, err := legacyChain(path, "", true)
			if err != nil {
				return err
			}
			return add(path, path, backing)
		}
		return nil
	})
	return refs, err
}

// numberedStage recognizes only the numeric suffixes os.CreateTemp and
// os.MkdirTemp generate, not similarly named directories a user may own.
func numberedStage(name, prefix, suffix string) bool {
	name, ok := strings.CutPrefix(name, prefix)
	if !ok {
		return false
	}
	name, ok = strings.CutSuffix(name, suffix)
	if !ok || name == "" {
		return false
	}
	for i := range len(name) {
		if name[i] < '0' || name[i] > '9' {
			return false
		}
	}
	return true
}

// cleanNodeStages runs under the exclusive bases lock. Fork, import and legacy
// normalization hold it shared until their stages are gone, so only crashed
// operations can leave these names. Extra hard links would otherwise pin a
// node after every committed disk over it was destroyed.
func cleanNodeStages(dataDir string) error {
	entries, err := os.ReadDir(dataDir)
	if err != nil {
		return err
	}
	for _, de := range entries {
		if de.IsDir() && numberedStage(de.Name(), "legacy-resume-", "") {
			dir := filepath.Join(dataDir, de.Name())
			files, err := os.ReadDir(dir)
			if errors.Is(err, fs.ErrNotExist) {
	if err := reconcileNodePublications(dataDir); err != nil {
		return err
	}
				continue
			}
			if err != nil {
				return err
			}
			recognized := true
			for _, file := range files {
				name := file.Name()
				if !file.Type().IsRegular() || (!numberedStage(name, "disk-", ".qcow2") && !numberedStage(name, ".manifest-", "")) {
					recognized = false
					break
				}
			}
			if recognized {
				if err := os.RemoveAll(dir); err != nil {
					return err
				}
			}
			continue
		}
		if !de.IsDir() || !numberedStage(de.Name(), "fork-", "") {
			continue
		}
		dir := filepath.Join(dataDir, de.Name())
		children, err := os.ReadDir(dir)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		if len(children) != 0 {
			if len(children) != 1 || children[0].Name() != "snap" || !children[0].IsDir() {
				continue
			}
			files, err := os.ReadDir(filepath.Join(dir, "snap"))
			if err != nil {
				return err
			}
			recognized := true
			for _, file := range files {
				name := file.Name()
				if !file.Type().IsRegular() || (name != "rootfs.qcow2" && name != checkpointFile &&
					name != memoryFile && name != "config.ext4" && name != snapshotManifestFile &&
					!numberedStage(name, ".manifest-", "") && !numberedStage(name, "vol", ".img")) {
					recognized = false
					break
				}
			}
			if !recognized {
				continue
			}
		}
		if err := os.RemoveAll(dir); err != nil {
			return err
		}
	}
	entries, err = os.ReadDir(nodesDir(dataDir))
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	for _, de := range entries {
		if de.Type().IsRegular() && (numberedStage(de.Name(), ".adopt-", "") ||
			numberedStage(de.Name(), ".publish-", "") || numberedStage(de.Name(), ".manifest-", "")) {
			if err := os.Remove(filepath.Join(nodesDir(dataDir), de.Name())); err != nil && !errors.Is(err, fs.ErrNotExist) {
				return err
			}
		}
	}
	return walkImageMetadata(dataDir, func(path, disk string) error {
		if _, err := os.Stat(disk); err == nil {
			return nil
		} else if !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		var m nodeMetadata
		found, err := readDaemonMetadata(path, &m)
		if err != nil || !found || (m.Format != "raw" && m.Format != "qcow2") {
			return nil
		}
		return removeNodeMetadata(disk)
	})
}

// sweepNodesLocked removes the nodes no managed root reaches and no other
// hard link holds. The caller holds the bases lock exclusively.
func sweepNodesLocked(dataDir string, dryRun bool) (removed []string, err error) {
	if !dryRun {
		if err := cleanNodeStages(dataDir); err != nil {
			return nil, err
		}
	}
	dir := nodesDir(dataDir)
	nodes, _, err := readNodeStore(dataDir)
	if err != nil {
		return nil, err
	}
	cand := map[fileKey][]string{}
	for k, n := range nodes {
		if len(n.names) != 0 && n.links <= n.count {
			cand[k] = n.names
		}
	}
	if len(cand) == 0 {
		return nil, nil
	}
	refs, err := diskRefs(dataDir)
	if err != nil {
		return nil, fmt.Errorf("sweep nodes: %w", err)
	}
	var garbage []string
	for k, names := range cand {
		if len(refs[k]) == 0 {
			garbage = append(garbage, names...)
		}
	}
	slices.Sort(garbage)
	if !dryRun {
		// Remove every alias's metadata before releasing any group's last pin.
		for _, path := range garbage {
			if err := removeNodeMetadata(path); err != nil {
				return removed, err
			}
			if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
				return removed, err
			}
		}
		removed = append(removed, path)
	}
	return removed, nil
}

		}
	}
	for _, path := range garbage {
		if !dryRun {
// SweepNodes removes the nodes nothing needs any more and returns them.
func SweepNodes(dataDir string, dryRun bool) ([]string, error) {
	unlock, err := lockBases(dataDir, true)
	if err != nil {
		return nil, err
	}
	if !dryRun && len(removed) != 0 {
		if err := syncNodeDir(dir); err != nil {
			return removed, err
		}
	}
	defer unlock()
	return sweepNodesLocked(dataDir, dryRun)
}

// scheduleNodeSweep sweeps in the background after a sandbox that had disks
// over nodes is gone (or never came to be). Requests that arrive during a
// sweep fold into one more run: tearing down a fleet of clones asks once per
// clone, and each sweep reads the managed disk metadata.
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
			case parts[0] == "snapshots" && len(parts) >= 3:
				snapshots[filepath.Dir(path)] = true
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
func (e *Engine) prepareNodeRoot(sandboxDir, disk string, own bool, sourceFormat string) (root, format, base string, err error) {
	if !rootQcow2() {
		return "", "", "", fmt.Errorf("a snapshot restores onto a qcow2 root, and KRUCIBLE_ROOT_RAW is set")
	}
	unlock, err := lockBases(e.cfg.DataDir, false)
	if err != nil {
		return "", "", "", fmt.Errorf("lock base images: %w", err)
	}
	defer unlock()
	root = filepath.Join(sandboxDir, "root.qcow2")
	if format, err = e.layerOn(root, disk, own, sourceFormat); err != nil {
		return "", "", "", fmt.Errorf("layer the root disk over the snapshot's: %w", err)
	}
	return root, format, disk, nil
}

// layerRestoredVolumes attaches each frozen volume of the snapshot being
// restored to spec: as a disk in sandboxDir over it, so none is copied and
// writes stay in this sandbox. A read-only volume gets one too: what the
// helper opens is then never the frozen file itself, and the overlay is what
// names it in the disk's recorded lifetime graph.
func (e *Engine) layerRestoredVolumes(sandboxDir string, spec *VMSpec, vols []restoreVol) error {
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
		format, err := e.layerOn(dst, rv.path, rv.owned, rv.format)
		if err != nil {
			return fmt.Errorf("restore volume %d: %w", idx, err)
		}
		spec.Volumes = append(spec.Volumes, VMVolume{BlockID: fmt.Sprintf("vol%d", idx), Path: dst, Format: format, ReadOnly: rv.readOnly})
	}
	return nil
}

// recordDiskBacking records only disks the daemon has just constructed.
// Captures retain and validate their source's metadata instead: re-recording a
// live header could turn a guest-written pointer into daemon authority.
func (e *Engine) recordDiskBacking(spec *VMSpec) error {
	var err error
	if spec.RootDisk != "" {
		if spec.RootDiskFormat != "raw" && spec.RootDiskFormat != "qcow2" {
			return fmt.Errorf("root disk: unsupported format %q", spec.RootDiskFormat)
		}
		spec.Backing, err = readDiskChain(spec.RootDisk, spec.RootDiskFormat, nil)
		if err != nil {
			return fmt.Errorf("root disk: %w", err)
		}
	}
	for i := range spec.Volumes {
		v := &spec.Volumes[i]
		if v.Format != "raw" && v.Format != "qcow2" {
			return fmt.Errorf("volume %s: unsupported format %q", v.BlockID, v.Format)
		}
		v.Backing, err = readDiskChain(v.Path, v.Format, nil)
		if err != nil {
			return fmt.Errorf("volume %s: %w", v.BlockID, err)
		}
	}
	return nil
}

// ownedNodeKeys is provenance, not reachability: only names and backing lists
// the daemon stored for this owner can authorize an otherwise untrusted
// header. Reading that header here would let it authorize its own pointers.
func ownedNodeKeys(dataDir, ownerID string) (map[fileKey]bool, error) {
	keys := map[fileKey]bool{}
	if ownerID == "" {
		return keys, nil
	}
	if ownerID == "." || ownerID == ".." || filepath.Base(ownerID) != ownerID {
		return nil, fmt.Errorf("invalid snapshot owner %q", ownerID)
	}
	add := func(path string, follow bool) error {
		var fi os.FileInfo
		var err error
		if follow {
			fi, err = os.Stat(path)
		} else {
			fi, err = os.Lstat(path)
		}
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		if fi.Mode().IsRegular() {
			if k, ok := keyOf(fi); ok {
				keys[k] = true
			}
		}
		return nil
	}
	addChain := func(format string, chain []diskLayer) error {
		if format == "" {
			return nil // Legacy header inference cannot confer provenance.
		}
		depth := 0
		for i := 0; ; i++ {
			switch format {
			case "raw":
				if i != len(chain) {
					return fmt.Errorf("raw disk has recorded backing files")
				}
			case "qcow2":
				depth++
				if depth > maxChainDepth {
					return fmt.Errorf("recorded backing chain deeper than %d", maxChainDepth)
				}
			default:
				return fmt.Errorf("unsupported recorded disk format %q", format)
			}
			if i == len(chain) {
				return nil
			}
			if err := add(chain[i].Path, true); err != nil {
				return err
			}
			format = chain[i].Format
		}
	}
	// A shared snapshots/ directory may be on another disk. An owner or
	// snapshot symlink, however, cannot confer the target owner's authority.
	snapshots, err := filepath.EvalSymlinks(filepath.Join(dataDir, "snapshots"))
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	if err == nil {
		ownerDir := filepath.Join(snapshots, ownerID)
		fi, err := os.Lstat(ownerDir)
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			return nil, err
		}
		if err == nil && fi.Mode()&fs.ModeSymlink != 0 {
			return nil, fmt.Errorf("snapshot owner directory %s is a symlink", ownerDir)
		}
		if err == nil {
			err = filepath.WalkDir(ownerDir, func(path string, de fs.DirEntry, err error) error {
				if errors.Is(err, fs.ErrNotExist) {
					return nil
				}
				if err != nil {
					return err
				}
				if !de.Type().IsRegular() {
					return nil
				}
				name := de.Name()
				if name == "rootfs.qcow2" || numberedStage(name, "vol", ".img") || numberedStage(name, archiveLayerPrefix, ".img") {
					return add(path, false)
				}
				if name != snapshotManifestFile {
					return nil
				}
				m, found, err := readSnapshotManifest(filepath.Dir(path))
				if err != nil || !found {
					return err
				}
				if m.DiskFile != "" {
					if !archiveFileName.MatchString(m.DiskFile) {
						return fmt.Errorf("%s: invalid disk name %q", path, m.DiskFile)
					}
					if err := add(filepath.Join(filepath.Dir(path), m.DiskFile), false); err != nil {
						return err
					}
				}
				for _, v := range m.Volumes {
					if !archiveFileName.MatchString(v.File) {
						return fmt.Errorf("%s: invalid volume name %q", path, v.File)
					}
					if err := add(filepath.Join(filepath.Dir(path), v.File), false); err != nil {
						return err
					}
					if err := addChain(v.Format, v.Backing); err != nil {
						return err
					}
				}
				if err := addChain(m.DiskFormat, m.Backing); err != nil {
					return err
				}
				return nil
			})
			if err != nil {
				return nil, err
			}
		}
	}
	// Only the shared images root may be relocated. Following owner, nested
	// directory, or image symlinks would let an alias confer another owner's
	// saved-image authority.
	images, err := filepath.EvalSymlinks(filepath.Join(dataDir, "images"))
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	if err == nil {
		ownerDir := filepath.Join(images, ownerID)
		fi, err := os.Lstat(ownerDir)
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			return nil, err
		}
		if err == nil && fi.Mode()&fs.ModeSymlink != 0 {
			return nil, fmt.Errorf("image owner directory %s is a symlink", ownerDir)
		}
		if err == nil && fi.IsDir() {
			err = filepath.WalkDir(ownerDir, func(path string, de fs.DirEntry, err error) error {
				if errors.Is(err, fs.ErrNotExist) {
					return nil
				}
				if err != nil {
					return err
				}
				name := de.Name()
				if !de.Type().IsRegular() || !strings.HasPrefix(name, ".") || !strings.HasSuffix(name, ".json") {
					return nil
				}
				disk := filepath.Join(filepath.Dir(path), strings.TrimSuffix(strings.TrimPrefix(name, "."), ".json"))
				fi, err := os.Lstat(disk)
				if errors.Is(err, fs.ErrNotExist) {
					return nil
				}
				if err != nil {
					return err
				}
				if !fi.Mode().IsRegular() {
					return nil
				}
				var m nodeMetadata
				found, err := readDaemonMetadata(path, &m)
				if err != nil || !found {
					return err
				}
				if m.Format == "" {
					return nil
				}
				if err := addChain(m.Format, m.Backing); err != nil {
					return err
				}
				if k, ok := keyOf(fi); ok {
					keys[k] = true
				}
				return nil
			})
			if err != nil {
				return nil, err
			}
		}
	}
	records, err := filepath.Glob(filepath.Join(dataDir, "sandboxes", "*", stateFile))
	if err != nil {
		return nil, err
	}
	for _, path := range records {
		fi, err := os.Lstat(path)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if !fi.Mode().IsRegular() {
			continue
		}
		data, err := os.ReadFile(path)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		var rec vmRecord
		if json.Unmarshal(data, &rec) != nil || rec.UserID != ownerID {
			continue
		}
		if err := addChain(rec.BaseSpec.RootDiskFormat, rec.BaseSpec.Backing); err != nil {
			return nil, err
		}
		for _, v := range rec.BaseSpec.Volumes {
			if err := addChain(v.Format, v.Backing); err != nil {
				return nil, err
			}
		}
	}
	return keys, nil
}

// adoptNode returns the name in the node store of the disk at path, which an
// import has unpacked and is done with: linked there, or copied in where a
// link can't reach.
func (e *Engine) adoptNode(path, format string, backing []diskLayer) (string, error) {
	node, err := e.linkNodeMetadata(path, format, backing)
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
	owned := true
	defer func() {
		if owned {
			_ = os.Remove(tmp.Name())
		}
	}()
	if err := tmp.Close(); err != nil {
		return "", err
	}
	if err := cloneFile(path, tmp.Name()); err != nil {
		return "", err
	}
	if err := e.prepareNodeAccess(tmp.Name()); err != nil {
		return "", err
	}
	// Syncing the source cannot make this new inode durable.
	if err := syncNodeFile(tmp.Name()); err != nil {
		return "", err
	}
	owned = false
	return e.commitNode(tmp.Name(), format, backing, true)
}
