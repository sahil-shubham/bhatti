package krucible

// Base images. A sandbox's root disk is a qcow2 overlay that names its raw
// base image by path, and its clusters only make sense over the exact bytes
// the base held when the overlay was made: put another image at that path and
// every sandbox on it becomes a corrupt filesystem. So a base, once complete,
// is never written again. Bases live in <DataDir>/images/bases/ under names
// that carry their content hash; a tier name (images/rootfs-<tier>-<arch>.ext4)
// is a symlink to the tier's current base; and an overlay names the base
// itself, never the symlink. Shipping a new tier image adds a base and
// re-points the symlink. Sandboxes made before keep theirs, which GCImages
// removes once nothing names it.

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

// basesDir is where base images live, each named by its contents.
func basesDir(dataDir string) string { return filepath.Join(dataDir, "images", "bases") }

// baseName is the bases/ name of an image with the given sha256, e.g.
// rootfs-minimal-amd64-0123456789abcdef.ext4 for stem rootfs-minimal-amd64.
// scripts/install.sh names the bases it installs the same way.
func baseName(stem, sum string) string { return stem + "-" + sum[:16] + ".ext4" }

// tierNames lists the tier names in dataDir/images, whatever they are now.
func tierNames(dataDir string) ([]string, error) {
	return filepath.Glob(filepath.Join(dataDir, "images", "rootfs-*.ext4"))
}

// resolveBasePath returns the absolute path of the file p leads to,
// following symlinks in its last element only: a tier name resolves to the
// base it points at now, while the directories on the way are kept as given,
// so a data dir reached through a symlink can still be moved by re-pointing
// that symlink.
func resolveBasePath(p string) (string, error) {
	p, err := filepath.Abs(p)
	if err != nil {
		return "", err
	}
	for range 40 {
		fi, err := os.Lstat(p)
		if err != nil {
			return "", err
		}
		if fi.Mode()&fs.ModeSymlink == 0 {
			return p, nil
		}
		t, err := os.Readlink(p)
		if err != nil {
			return "", err
		}
		if !filepath.IsAbs(t) {
			t = filepath.Join(filepath.Dir(p), t)
		}
		p = filepath.Clean(t)
	}
	return "", fmt.Errorf("%s: too many levels of symbolic links", p)
}

// lockBases takes the lock that keeps GC from removing a base something is
// about to name: GC and migration hold it exclusively; a create (from picking
// its base until its overlay names it) and an import hold it shared. An
// flock, so `bhatti admin gc-images` in another process honours it too.
func lockBases(dataDir string, exclusive bool) (unlock func(), err error) {
	dir := filepath.Join(dataDir, "images")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(filepath.Join(dir, ".bases.lock"), os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, err
	}
	how := syscall.LOCK_SH
	if exclusive {
		how = syscall.LOCK_EX
	}
	for {
		if err = syscall.Flock(int(f.Fd()), how); err != syscall.EINTR {
			break
		}
	}
	if err != nil {
		f.Close()
		return nil, fmt.Errorf("lock %s: %w", f.Name(), err)
	}
	return func() { f.Close() }, nil
}

// syncDir makes a rename or link in dir durable. Best effort: everything
// that renames into a directory here is redone by the next migration if the
// entry is lost.
func syncDir(dir string) {
	if d, err := os.Open(dir); err == nil {
		_ = d.Sync()
		d.Close()
	}
}

// ImageMigration is what MigrateImages did, and what it left alone.
type ImageMigration struct {
	Moved     []string // "<tier> -> <base>": tier names now symlinks to a base
	Rewritten []string // "<image> -> <base>": re-pointed from a symlink to its base
	Broken    []string // qcow2 images whose backing file was missing already
	Problems  []string // left unmigrated, with the reason
}

// MigrateImages brings dataDir to the immutable-base layout, idempotently.
// Each tier name that is still a regular file becomes a base under bases/ (a
// hard link: its bytes neither move nor change) and a symlink to it. Then
// each qcow2 image under dataDir whose backing file is named through a
// symlink is re-pointed at the file the symlink leads to now: the bytes it was
// made on. What it can't do is left untouched, listed in Problems, and makes
// the error non-nil: re-pointing a tier symlink while an image still reaches
// a base through it would corrupt that image.
func MigrateImages(dataDir string) (ImageMigration, error) {
	var m ImageMigration
	unlock, err := lockBases(dataDir, true)
	if err != nil {
		return m, err
	}
	defer unlock()

	tiers, err := tierNames(dataDir)
	if err != nil {
		return m, err
	}
	for _, tier := range tiers {
		fi, err := os.Lstat(tier)
		if err != nil || !fi.Mode().IsRegular() {
			continue // already a symlink
		}
		base, err := tierToBase(dataDir, tier, fi)
		if err != nil {
			m.Problems = append(m.Problems, fmt.Sprintf("%s: %v", tier, err))
			continue
		}
		m.Moved = append(m.Moved, tier+" -> "+base)
	}

	err = walkImages(dataDir, func(path string, link bool) error {
		if link {
			return nil
		}
		backing, err := qcow2BackingPath(path)
		if err != nil {
			m.Problems = append(m.Problems, fmt.Sprintf("%s: %v", path, err))
			return nil
		}
		if backing == "" {
			return nil
		}
		fi, err := os.Lstat(backing)
		if err != nil {
			m.Broken = append(m.Broken, fmt.Sprintf("%s: backing file %s: %v", path, backing, err))
			return nil
		}
		if fi.Mode()&fs.ModeSymlink == 0 {
			return nil
		}
		target, err := resolveBasePath(backing)
		if err != nil {
			m.Problems = append(m.Problems, fmt.Sprintf("%s: backing file %s: %v", path, backing, err))
			return nil
		}
		if err := setQcow2Backing(path, target); err != nil {
			m.Problems = append(m.Problems, err.Error())
			return nil
		}
		m.Rewritten = append(m.Rewritten, path+" -> "+target)
		return nil
	})
	if err != nil {
		m.Problems = append(m.Problems, err.Error())
	}
	if len(m.Problems) > 0 {
		return m, fmt.Errorf("%d image(s) could not be migrated", len(m.Problems))
	}
	return m, nil
}

// tierToBase turns the regular file at tier into a base under bases/ and
// tier into a symlink to it, returning the base. Every step leaves tier
// naming the same bytes, so an interrupted run is finished by the next one.
func tierToBase(dataDir, tier string, fi os.FileInfo) (string, error) {
	sum, _, err := hashFile(tier)
	if err != nil {
		return "", err
	}
	dir := basesDir(dataDir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	name := baseName(strings.TrimSuffix(filepath.Base(tier), ".ext4"), sum)
	dst := filepath.Join(dir, name)
	switch have, err := os.Lstat(dst); {
	case err == nil:
		// Linked by an interrupted run, or the same image installed already.
		if !os.SameFile(fi, have) {
			s, _, err := hashFile(dst)
			if err != nil {
				return "", err
			}
			if s != sum {
				return "", fmt.Errorf("%s holds other contents than %s", dst, tier)
			}
		}
	case errors.Is(err, fs.ErrNotExist):
		if err := os.Link(tier, dst); err != nil {
			return "", fmt.Errorf("keep as base: %w", err)
		}
		syncDir(dir)
	default:
		return "", err
	}
	images := filepath.Dir(tier)
	tmp := filepath.Join(images, "."+filepath.Base(tier)+".link")
	os.Remove(tmp)
	if err := os.Symlink(filepath.Join("bases", name), tmp); err != nil {
		return "", err
	}
	if err := os.Rename(tmp, tier); err != nil {
		os.Remove(tmp)
		return "", err
	}
	syncDir(images)
	return dst, nil
}

// qcow2BackingPath returns the path of the backing file the qcow2 image at
// path names ("" when it has none); a relative name is relative to the
// image's directory.
func qcow2BackingPath(path string) (string, error) {
	name, err := qcow2Backing(path)
	if err != nil || name == "" {
		return "", err
	}
	if !filepath.IsAbs(name) {
		name = filepath.Join(filepath.Dir(path), name)
	}
	return name, nil
}

// walkImages calls fn for every qcow2 image (link=false) and every symlink
// (link=true) under dataDir, except under bases/: a base is only ever
// reached through what names it. A file it can't read goes to fn as an
// image, whose header fn then fails to read: it might be one.
//
// Files directly in dataDir are never images, and are never opened: they
// are the daemon's own state, state.db and its WAL and shm among them. A
// process's POSIX record locks on a file all go when it closes any fd to that
// file, so sniffing state.db-shm here would silently release the daemon's
// SQLite locks, and the next process to open the database (any `bhatti user`
// command) would take itself for the only one and delete the live WAL.
func walkImages(dataDir string, fn func(path string, link bool) error) error {
	skip := basesDir(dataDir)
	return filepath.WalkDir(dataDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		switch {
		case d.IsDir():
			if path == skip {
				return filepath.SkipDir
			}
			return nil
		case filepath.Dir(path) == filepath.Clean(dataDir):
			return nil
		case d.Type()&fs.ModeSymlink != 0:
			return fn(path, true)
		case d.Type().IsRegular() && maybeQcow2(path):
			return fn(path, false)
		}
		return nil
	})
}

// maybeQcow2 is isQcow2 that says yes when it can't read the file.
func maybeQcow2(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return true
	}
	defer f.Close()
	var magic [4]byte
	switch _, err := io.ReadFull(f, magic[:]); err {
	case nil:
		return magic == [4]byte{'Q', 'F', 'I', 0xfb}
	case io.EOF, io.ErrUnexpectedEOF:
		return false // shorter than a qcow2 header
	default:
		return true
	}
}

type fileKey struct{ dev, ino uint64 }

func keyOf(fi os.FileInfo) (fileKey, bool) {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return fileKey{}, false
	}
	return fileKey{uint64(st.Dev), uint64(st.Ino)}, true
}

// GCImages removes the bases under dataDir/images/bases that nothing names:
// no tier name or other symlink under dataDir leads to one, no qcow2 image
// under dataDir reaches it through its chain of backing files, and it isn't
// one of keep (the configured base image). A qcow2 image it can't read stops
// it before anything is removed, since that image might name any base.
// Dotfiles (an install or import still writing) are never removed. With
// dryRun it only reports what it would remove.
func GCImages(dataDir string, keep []string, dryRun bool) (removed []string, err error) {
	unlock, err := lockBases(dataDir, true)
	if err != nil {
		return nil, err
	}
	defer unlock()

	used := map[fileKey]bool{}
	mark := func(path string) {
		if fi, err := os.Stat(path); err == nil {
			if k, ok := keyOf(fi); ok {
				used[k] = true
			}
		}
	}
	for _, k := range keep {
		if k != "" {
			mark(k)
		}
	}
	tiers, err := tierNames(dataDir)
	if err != nil {
		return nil, err
	}
	for _, t := range tiers {
		mark(t)
	}
	err = walkImages(dataDir, func(path string, link bool) error {
		if link {
			mark(path)
			return nil
		}
		seen := map[string]bool{}
		for cur := path; !seen[cur]; {
			seen[cur] = true
			backing, err := qcow2BackingPath(cur)
			if err != nil {
				return fmt.Errorf("%s: %w (an image GC can't read might name any base)", cur, err)
			}
			if backing == "" {
				break
			}
			mark(backing)
			if !isQcow2(backing) {
				break
			}
			cur = backing
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("gc images: %w", err)
	}

	dir := basesDir(dataDir)
	entries, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	for _, de := range entries {
		if strings.HasPrefix(de.Name(), ".") || !de.Type().IsRegular() {
			continue
		}
		path := filepath.Join(dir, de.Name())
		fi, err := os.Stat(path)
		if err != nil {
			continue
		}
		k, ok := keyOf(fi)
		if !ok || used[k] {
			continue
		}
		if !dryRun {
			if err := os.Remove(path); err != nil {
				return removed, err
			}
		}
		removed = append(removed, path)
	}
	return removed, nil
}
