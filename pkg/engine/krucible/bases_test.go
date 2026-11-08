package krucible

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sahil-shubham/bhatti/pkg/engine"
)

// overlayOn writes a qcow2 overlay at path whose header names backing, as a
// sandbox root made by an older bhatti names its tier image.
func overlayOn(t *testing.T, path, backing string, size int64) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := createQcow2Overlay(path, backing, uint64(size)); err != nil {
		t.Fatal(err)
	}
}

func backingOf(t *testing.T, path string) string {
	t.Helper()
	b, err := qcow2Backing(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func readlink(t *testing.T, path string) string {
	t.Helper()
	l, err := os.Readlink(path)
	if err != nil {
		t.Fatalf("%s is not a symlink: %v", path, err)
	}
	return l
}

// flipTier does what an install of a new tier image does once migration is
// done: publish the image as a base and re-point the tier name at it.
func flipTier(t *testing.T, dataDir, tier string, data []byte) string {
	t.Helper()
	sum := sha256.Sum256(data)
	name := baseName(strings.TrimSuffix(filepath.Base(tier), ".ext4"), hex.EncodeToString(sum[:]))
	writeFile(t, filepath.Join(basesDir(dataDir), name), data)
	tmp := tier + ".new"
	if err := os.Symlink(filepath.Join("bases", name), tmp); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(tmp, tier); err != nil {
		t.Fatal(err)
	}
	return filepath.Join(basesDir(dataDir), name)
}

// TestMigrateImagesKeepsDisksOnTheirBase: a data dir laid out by an older
// bhatti (a tier image as a plain file, every kind of disk naming it by its
// tier path, one by a relative path) is migrated so that publishing a new
// image for the tier leaves every disk on the bytes it was made on. The tier
// file becomes a base without its bytes being copied or touched, and a second
// run changes nothing.
func TestMigrateImagesKeepsDisksOnTheirBase(t *testing.T) {
	data := t.TempDir()
	tier := filepath.Join(data, "images", "rootfs-minimal-"+runtime.GOARCH+".ext4")
	old := writeBase(t, tier, 1)
	before, err := os.Stat(tier)
	if err != nil {
		t.Fatal(err)
	}
	disks := []string{
		filepath.Join(data, "sandboxes", "s1", "root.qcow2"),
		filepath.Join(data, "snapshots", "u1", "snap", "rootfs.qcow2"),
		filepath.Join(data, "images", "u1", "saved.ext4"),
	}
	for _, d := range disks {
		overlayOn(t, d, tier, int64(len(old)))
	}
	relative := filepath.Join(data, "sandboxes", "s2", "root.qcow2")
	overlayOn(t, relative, filepath.Join("..", "..", "images", filepath.Base(tier)), int64(len(old)))
	disks = append(disks, relative)
	// A disk on a plain file that isn't a tier is left as it is.
	other := filepath.Join(data, "images", "u1", "pulled.ext4")
	writeBase(t, other, 3)
	onOther := filepath.Join(data, "sandboxes", "s3", "root.qcow2")
	overlayOn(t, onOther, other, 3<<20)
	record, err := json.Marshal(vmRecord{ID: "s3", UserID: "u1", SandboxDir: filepath.Dir(onOther),
		BaseSpec: VMSpec{RootDisk: onOther, RootDiskFormat: "qcow2"}})
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, stateFilePath(filepath.Dir(onOther)), record)

	m, err := MigrateImages(data)
	if err != nil {
		t.Fatalf("migrate: %v (%+v)", err, m)
	}
	base := filepath.Join(basesDir(data), baseName("rootfs-minimal-"+runtime.GOARCH, fileSum(t, tier)))
	if l := readlink(t, tier); l != filepath.Join("bases", filepath.Base(base)) {
		t.Fatalf("tier links to %q, want bases/%s", l, filepath.Base(base))
	}
	if fi, err := os.Stat(base); err != nil || !os.SameFile(fi, before) {
		t.Fatalf("base %s isn't the tier's own file (%v): its bytes were copied", base, err)
	}
	for _, d := range disks {
		if got := backingOf(t, d); got != base {
			t.Fatalf("%s names %q, want the base %s", d, got, base)
		}
	}
	if got := backingOf(t, onOther); got != other {
		t.Fatalf("a disk on a plain image was re-pointed to %q", got)
	}
	if len(m.Moved) != 1 || len(m.Rewritten) != len(disks) || len(m.Problems) != 0 {
		t.Fatalf("migration report %+v", m)
	}

	snapshot := map[string][]byte{}
	for _, d := range append(disks, onOther) {
		b, _ := os.ReadFile(d)
		snapshot[d] = b
	}
	m, err = MigrateImages(data)
	if err != nil || len(m.Moved)+len(m.Rewritten)+len(m.Problems) != 0 {
		t.Fatalf("second migration: %v %+v", err, m)
	}
	for d, b := range snapshot {
		if now, _ := os.ReadFile(d); !bytes.Equal(now, b) {
			t.Fatalf("second migration changed %s", d)
		}
	}

	// The update this is for: a rebuilt image for the tier.
	newer := bytes.Repeat([]byte{0xab}, len(old))
	newBase := flipTier(t, data, tier, newer)
	for _, d := range disks {
		if got := backingOf(t, d); got != base {
			t.Fatalf("after the update %s names %q", d, got)
		}
	}
	if got, _ := os.ReadFile(base); !bytes.Equal(got, old) {
		t.Fatal("the old base changed")
	}
	if p, err := resolveBasePath(tier); err != nil || p != newBase {
		t.Fatalf("tier resolves to %q (%v), want %s", p, err, newBase)
	}
}

// TestMigrateImagesRefusesWhatItCantRepoint: a disk whose header has no room
// for the base's name is left byte-for-byte as it was, and the migration
// fails, so an installer doesn't re-point the tier under it.
func TestMigrateImagesRefusesWhatItCantRepoint(t *testing.T) {
	data := t.TempDir()
	tier := filepath.Join(data, "images", "rootfs-minimal-"+runtime.GOARCH+".ext4")
	writeBase(t, tier, 1)
	cramped := filepath.Join(data, "sandboxes", "s1", "root.qcow2")
	crampedQcow2(t, cramped, tier)
	before, _ := os.ReadFile(cramped)

	m, err := MigrateImages(data)
	if err == nil || len(m.Problems) != 1 || !strings.Contains(m.Problems[0], "doesn't fit") {
		t.Fatalf("migrate: %v %+v; want it to refuse the cramped disk", err, m)
	}
	if after, _ := os.ReadFile(cramped); !bytes.Equal(before, after) {
		t.Fatal("a refused disk was changed")
	}
}

// crampedQcow2 writes a qcow2 header with 512-byte clusters whose backing
// name (backing, padded with "/" to 300 bytes) leaves no room in the header
// cluster for a longer one either before or after it.
func crampedQcow2(t *testing.T, path, backing string) {
	t.Helper()
	name := strings.Repeat("/", 300-len(backing)) + backing
	if len(name) != 300 {
		t.Fatalf("backing %s too long for the cramped header", backing)
	}
	h := make([]byte, 512)
	be := binary.BigEndian
	copy(h, "QFI\xfb")
	be.PutUint32(h[4:], 3)
	be.PutUint64(h[8:], 200)
	be.PutUint32(h[16:], uint32(len(name)))
	be.PutUint32(h[20:], 9)
	be.PutUint64(h[24:], 3<<20)
	be.PutUint32(h[96:], 4)
	be.PutUint32(h[100:], qcow2HeaderLen)
	// Extensions end right after the header (an end marker at 104).
	copy(h[200:], name)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, h, 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestGCImagesKeepsWhatIsNamed: GC removes only the bases nothing names. A
// base stays while a tier symlink, the configured base, a sandbox's root, a
// saved image or a chain of backing files leads to it, and a dotfile (an
// install still writing) stays. A dry run removes nothing, and a disk GC
// can't read stops it before it removes anything.
func TestGCImagesKeepsWhatIsNamed(t *testing.T) {
	data := t.TempDir()
	bases := basesDir(data)
	base := func(name string, seed byte) string {
		p := filepath.Join(bases, name)
		writeBase(t, p, seed)
		return p
	}
	tierBase := base("rootfs-minimal-amd64-aaaaaaaaaaaaaaaa.ext4", 1)
	if err := os.Symlink(filepath.Join("bases", filepath.Base(tierBase)), filepath.Join(data, "images", "rootfs-minimal-amd64.ext4")); err != nil {
		t.Fatal(err)
	}
	sandboxBase := base("rootfs-minimal-amd64-bbbbbbbbbbbbbbbb.ext4", 2)
	overlayOn(t, filepath.Join(data, "sandboxes", "s1", "root.qcow2"), sandboxBase, 3<<20)
	savedBase := base("rootfs-browser-amd64-cccccccccccccccc.ext4", 3)
	overlayOn(t, filepath.Join(data, "images", "u1", "saved.ext4"), savedBase, 3<<20)
	// snapshot root -> a qcow2 node kept among the bases -> a raw base.
	chainBase := base("base-dddddddddddddddd.ext4", 4)
	node := filepath.Join(bases, "node-eeeeeeeeeeeeeeee.ext4")
	overlayOn(t, node, chainBase, 3<<20)
	snapshotRoot := filepath.Join(data, "snapshots", "u1", "snap", "rootfs.qcow2")
	if err := os.MkdirAll(filepath.Dir(snapshotRoot), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := createQcow2OverlayOn(snapshotRoot, node, "qcow2", 3<<20); err != nil {
		t.Fatal(err)
	}
	configured := base("rootfs-docker-amd64-ffffffffffffffff.ext4", 5)
	writing := base(".rootfs-minimal-amd64.ext4.tmp.123", 6)
	unused := base("rootfs-minimal-amd64-0000000000000000.ext4", 7)
	legacyUnused := base("sha256-"+strings.Repeat("1", 64)+".img", 8)

	keep := []string{configured}
	removed, err := GCImages(data, keep, true)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{legacyUnused, unused}
	slices.Sort(removed)
	slices.Sort(want)
	if !slices.Equal(removed, want) {
		t.Fatalf("dry run would remove %v, want %v", removed, want)
	}
	for _, p := range want {
		if _, err := os.Stat(p); err != nil {
			t.Fatalf("dry run removed %s", p)
		}
	}

	// A disk GC can't read might name any base: nothing goes.
	bad := filepath.Join(data, "sandboxes", "s9", "root.qcow2")
	writeFile(t, bad, append([]byte("QFI\xfb\x00\x00\x00\x09"), make([]byte, 64)...))
	if removed, err := GCImages(data, keep, false); err == nil || len(removed) != 0 {
		t.Fatalf("GC over an unreadable disk removed %v (err %v)", removed, err)
	}
	os.Remove(bad)

	removed, err = GCImages(data, keep, false)
	if err != nil {
		t.Fatal(err)
	}
	slices.Sort(removed)
	if !slices.Equal(removed, want) {
		t.Fatalf("removed %v, want %v", removed, want)
	}
	for _, p := range want {
		if _, err := os.Stat(p); err == nil {
			t.Fatalf("%s is still there", p)
		}
	}
	for _, p := range []string{tierBase, sandboxBase, savedBase, chainBase, node, configured, writing} {
		if _, err := os.Stat(p); err != nil {
			t.Fatalf("GC removed %s, which is in use: %v", p, err)
		}
	}
}

// A regular tier must become immutable before the root and its authoritative
// chain name it; later installer migration must not need to rewrite either.
func TestNewRootNamesTheBaseNotTheTier(t *testing.T) {
	for _, source := range []string{"default", "explicit", "growth"} {
		t.Run(source, func(t *testing.T) {
			data := t.TempDir()
			tier := filepath.Join(data, "images", "rootfs-minimal-"+runtime.GOARCH+".ext4")
			old := writeBase(t, tier, 1)
			base := filepath.Join(basesDir(data), baseName(strings.TrimSuffix(filepath.Base(tier), ".ext4"), fileSum(t, tier)))
			e := &Engine{cfg: Config{DataDir: data, BaseImage: tier, BlockRoot: true}}
			spec := engine.SandboxSpec{}
			if source != "default" {
				spec.BaseImage = tier
			}
			if source == "growth" {
				spec.DiskSizeMB = 64
			}
			dir := filepath.Join(data, "sandboxes", "new")
			if err := os.MkdirAll(dir, 0o700); err != nil {
				t.Fatal(err)
			}
			root, format, got, backing, err := e.prepareRootDisk(t.Context(), dir, spec)
			if err != nil {
				t.Fatal(err)
			}
			if format != "qcow2" || got != base || backingOf(t, root) != base ||
				len(backing) != 1 || backing[0] != (diskLayer{Path: base, Format: "raw"}) {
				t.Fatalf("root %s (%s) names %q, base %q, chain %+v; want %s", root, format, backingOf(t, root), got, backing, base)
			}
			if resolved, err := resolveBasePath(tier); err != nil || resolved != base {
				t.Fatalf("tier resolves to %q (%v), want %s", resolved, err, base)
			}
			if err := validateRecordedDisk(root, format, backing); err != nil {
				t.Fatalf("new root's recorded chain: %v", err)
			}
			if m, err := MigrateImages(data); err != nil || len(m.Moved)+len(m.Rewritten) != 0 {
				t.Fatalf("installer migration changed a new root: %+v, %v", m, err)
			}
			newBase := flipTier(t, data, tier, bytes.Repeat([]byte{7}, len(old)))
			if err := validateRecordedDisk(root, format, backing); err != nil || backingOf(t, root) != base {
				t.Fatalf("tier update changed the old root or its recorded chain: %v", err)
			}
			if got, err := os.ReadFile(base); err != nil || !bytes.Equal(got, old) {
				t.Fatalf("tier update changed the old base: %v", err)
			}
			freshDir := filepath.Join(data, "sandboxes", "fresh")
			if err := os.MkdirAll(freshDir, 0o700); err != nil {
				t.Fatal(err)
			}
			fresh, _, got, _, err := e.prepareRootDisk(t.Context(), freshDir, spec)
			if err != nil || got != newBase || backingOf(t, fresh) != newBase {
				t.Fatalf("fresh root didn't select updated base %s: %q, %v", newBase, got, err)
			}
		})
	}
}

func TestRootTierMigrationCollisionFailsPreparationAndCreate(t *testing.T) {
	for _, operation := range []string{"prepare", "create"} {
		t.Run(operation, func(t *testing.T) {
			data := t.TempDir()
			tier := filepath.Join(data, "images", "rootfs-test-"+runtime.GOARCH+".ext4")
			original := writeBase(t, tier, 1)
			base := filepath.Join(basesDir(data), baseName(strings.TrimSuffix(filepath.Base(tier), ".ext4"), fileSum(t, tier)))
			other := writeBase(t, base, 2)
			e := &Engine{cfg: Config{DataDir: data, SocketDir: filepath.Join(data, "sockets"), BaseImage: tier, BlockRoot: true}}
			dir := filepath.Join(data, "sandboxes", "new")
			if err := os.MkdirAll(dir, 0o700); err != nil {
				t.Fatal(err)
			}
			var err error
			if operation == "prepare" {
				_, _, _, _, err = e.prepareRootDisk(t.Context(), dir, engine.SandboxSpec{})
			} else {
				_, err = e.Create(t.Context(), engine.SandboxSpec{})
			}
			if err == nil || !strings.Contains(err.Error(), "migrate root tier") || !strings.Contains(err.Error(), "holds other contents") {
				t.Fatalf("tier migration collision was not the refusal reason: %v", err)
			}
			mustNotExist(t, filepath.Join(dir, "root.qcow2"))
			if got, err := os.ReadFile(tier); err != nil || !bytes.Equal(got, original) {
				t.Fatalf("failed tier migration changed the tier: %v", err)
			}
			if got, err := os.ReadFile(base); err != nil || !bytes.Equal(got, other) {
				t.Fatalf("failed tier migration changed the colliding base: %v", err)
			}
		})
	}
}

func TestSharedRootPreparationRefusesUnmigratedTier(t *testing.T) {
	data := t.TempDir()
	tier := filepath.Join(data, "images", "rootfs-test-"+runtime.GOARCH+".ext4")
	writeBase(t, tier, 1)
	e := &Engine{cfg: Config{DataDir: data, BaseImage: tier, BlockRoot: true}}
	dir := filepath.Join(data, "sandboxes", "new")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	unlock, err := lockBasesCtx(t.Context(), data, false)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	_, _, _, _, err = e.prepareRootDiskShared(t.Context(), dir, engine.SandboxSpec{})
	if err == nil || !strings.Contains(err.Error(), "unmigrated root tier") {
		t.Fatalf("already-shared preparation accepted a mutable tier: %v", err)
	}
	mustNotExist(t, filepath.Join(dir, "root.qcow2"))
}

func TestRootTierMigrationRequiresExclusiveLease(t *testing.T) {
	data := t.TempDir()
	tier := filepath.Join(data, "images", "rootfs-test-"+runtime.GOARCH+".ext4")
	writeBase(t, tier, 1)
	e := &Engine{cfg: Config{DataDir: data, BaseImage: tier, BlockRoot: true}}
	dir := filepath.Join(data, "sandboxes", "new")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	unlock, err := lockBasesCtx(t.Context(), data, false)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	if _, _, _, _, err := e.prepareRootDisk(ctx, dir, engine.SandboxSpec{}); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("tier migration didn't wait for the exclusive bases lease: %v", err)
	}
	mustNotExist(t, filepath.Join(dir, "root.qcow2"))
	if fi, err := os.Lstat(tier); err != nil || !fi.Mode().IsRegular() {
		t.Fatalf("tier changed under an existing shared lease: %v", err)
	}
}

// TestWalkImagesSkipsWhatVanishes: the daemon walks its data dir at startup
// while an installer may still be deleting its staging area there. A
// directory removed mid-walk is skipped, not an error that marks the
// migration failed.
func TestWalkImagesSkipsWhatVanishes(t *testing.T) {
	data := t.TempDir()
	for _, dir := range []string{"a", "b/inner"} {
		if err := os.MkdirAll(filepath.Join(data, dir), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink("nowhere", filepath.Join(data, dir, "link")); err != nil {
			t.Fatal(err)
		}
	}
	var seen []string
	err := walkImages(data, func(path string, link bool) error {
		seen = append(seen, path)
		if filepath.Base(filepath.Dir(path)) == "a" {
			return os.RemoveAll(filepath.Join(data, "b"))
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walkImages = %v, want the vanished directory skipped", err)
	}
	if len(seen) != 1 {
		t.Fatalf("visited %v, want only a/link", seen)
	}
}
