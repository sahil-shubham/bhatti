package krucible

// Moving named snapshots between hosts. ExportSnapshot writes a snapshot
// directory as one zstd-compressed tar; ImportSnapshot unpacks it on another
// host of the same OS and architecture, after checking that host can run it.
//
// The archive holds, in order:
//
//	bhatti-snapshot.json  the archive manifest (archiveManifest)
//	checkpoint.bin        memory snapshots: VM state, checked against this
//	memory.bin            host before the large files arrive; guest RAM
//	<disk file>           the root disk in its recorded format
//	<volume files>        frozen data volumes
//	layer-<n>.img         the nodes the disks are layered over (nodes.go)
//	base.img              the overlay's base image, with IncludeBase
//
// A disk the sandbox wrote since it was resumed holds only that: what it was
// layered over is a node, which arrives as an entry of its own, and an import
// links it into the node store and re-points the header above it. A qcow2 root
// disk and its layers hold what was written; the rest may come from a raw base
// image, which the manifest identifies by content (sha256). An importing host
// uses its own copy of that base when it has one, wherever it lives;
// otherwise the base must travel in the archive, and is kept under
// <DataDir>/images/bases/ for every snapshot that needs it. Integrity rests on
// zstd's frame checksum, checked by reading the stream to its end, plus the
// sha256 of checkpoint.bin (checked before it is used) and of the base.
// Every disk dependency and format is recorded in the archive metadata.
// Incoming backing paths are replaced by that graph before any node or base
// is kept. Raw payloads stay opaque so guest bytes cannot choose a disk driver.

import (
	"archive/tar"
	"bytes"
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/klauspost/compress/zstd"

	"github.com/sahil-shubham/bhatti/pkg/engine"
	"github.com/sahil-shubham/bhatti/pkg/gateway"
)

const (
	archiveFormat       = 3
	archiveManifestFile = "bhatti-snapshot.json"
	archiveBaseFile     = "base.img"
	checkpointFile      = "checkpoint.bin"
	memoryFile          = "memory.bin"
	// maxArchiveManifest bounds the manifest an import reads into memory.
	maxArchiveManifest = 1 << 20
	archiveBufSize     = 1 << 20
)

// archiveManifest is the first entry of a snapshot archive.
type archiveManifest struct {
	Format int    `json:"format"`
	Name   string `json:"name"` // the snapshot's name where it was exported
	OS     string `json:"os"`
	Arch   string `json:"arch"`
	// Snapshot is the engine's manifest, as the importing host will use it.
	Snapshot krucibleSnapManifest `json:"snapshot"`
	// Files are the entries after this one, in order.
	Files []archiveFile `json:"files"`
	// Base identifies the root disk's base image; nil when it has none.
	Base *archiveBase `json:"base,omitempty"`
	// Layers are the images the snapshot's disks are layered over, each an
	// entry named layer-<n>.img after the snapshot's own files: the root
	// disk's, nearest first, then each volume's.
	Layers []archiveLayer `json:"layers,omitempty"`
}

type archiveFile struct {
	Name string `json:"name"`
	Size int64  `json:"size"`
	// SHA256 is set for the files an import checks before using them:
	// checkpoint.bin and base.img.
	SHA256 string `json:"sha256,omitempty"`
}

type archiveBase struct {
	SHA256   string `json:"sha256"`
	Size     int64  `json:"size"`
	Name     string `json:"name"` // its file name on the exporting host, for messages
	Included bool   `json:"included"`
	// Over is the entry whose header names the base: the root disk, or the
	// deepest layer under it. Empty means the root disk.
	Over string `json:"over,omitempty"`
}

// archiveLayer is an image that another entry's header names as its backing
// file, and so has to be put where that header will find it.
type archiveLayer struct {
	Name   string `json:"name"` // its entry
	Format string `json:"format"`
	// Over is the entry whose header names this one: a disk of the snapshot,
	// or the layer above it. An entry names at most one.
	Over string `json:"over"`
}

// archiveLayerPrefix starts the name of every layer entry.
const archiveLayerPrefix = "layer-"

// archiveFileName is what a snapshot's own files may be called: plain names
// in its directory.
var archiveFileName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)

var sha256Hex = regexp.MustCompile(`^[0-9a-f]{64}$`)

func badArchive(format string, args ...any) error {
	return fmt.Errorf("%w: %s", engine.ErrBadSnapshotArchive, fmt.Sprintf(format, args...))
}

func incompatible(format string, args ...any) error {
	return fmt.Errorf("%w: %s", engine.ErrSnapshotIncompatible, fmt.Sprintf(format, args...))
}

// snapshotFiles lists a snapshot directory's files in archive order.
func snapshotFiles(m krucibleSnapManifest) []string {
	var names []string
	if m.Type == "memory" {
		names = append(names, checkpointFile, memoryFile)
	}
	names = append(names, m.DiskFile)
	for _, v := range m.Volumes {
		names = append(names, v.File)
	}
	return names
}

func archiveSnapshotFiles(m krucibleSnapManifest) ([]string, error) {
	if m.Type != "memory" && m.Type != "filesystem" {
		return nil, badArchive("unknown snapshot type %q", m.Type)
	}
	if m.Type == "filesystem" && len(m.Volumes) != 0 {
		return nil, badArchive("a filesystem snapshot has no saved volume device set")
	}
	if m.DiskFormat != "" && m.DiskFormat != "raw" && m.DiskFormat != "qcow2" {
		return nil, badArchive("root disk has unsupported format %q", m.DiskFormat)
	}
	for _, v := range m.Volumes {
		if v.Format != "" && v.Format != "raw" && v.Format != "qcow2" {
			return nil, badArchive("volume %s has unsupported format %q", v.File, v.Format)
		}
	}
	names := snapshotFiles(m)
	diskAt := 0
	if m.Type == "memory" {
		diskAt = 2
	}
	for i, name := range names {
		reserved := name == archiveManifestFile || name == archiveBaseFile || name == snapshotManifestFile ||
			strings.HasPrefix(name, archiveLayerPrefix) ||
			(i >= diskAt && (name == checkpointFile || name == memoryFile))
		if !archiveFileName.MatchString(name) || reserved || slices.Index(names, name) != i {
			return nil, badArchive("bad file name %q", name)
		}
	}
	return names, nil
}

// normalize fills the defaults ResumeFromManifestJSON applies, so both ends
// of an export agree on the files.
func (m *krucibleSnapManifest) normalize() {
	if m.Type == "" {
		m.Type = "memory"
	}
	if m.DiskFile == "" {
		m.DiskFile = "rootfs.qcow2"
	}
}

// ExportSnapshot implements engine.SnapshotPorter.
func (e *Engine) ExportSnapshot(ctx context.Context, w io.Writer, snapDir string, manifestJSON []byte, opts engine.SnapshotExport) error {
	unlock, err := lockBasesCtx(ctx, e.cfg.DataDir, false)
	if err != nil {
		return fmt.Errorf("export: lock nodes: %w", err)
	}
	// Open every input before releasing the lease: unlinked snapshot files
	// and ancestors stay readable without letting a slow client hold up GC.
	am, paths, err := e.planExport(snapDir, manifestJSON, opts)
	if err != nil {
		unlock()
		return err
	}
	sources := make([]*os.File, 0, len(paths))
	closeSources := func() {
		for _, src := range sources {
			src.Close()
		}
	}
	defer closeSources()
	for _, path := range paths {
		if err := ctx.Err(); err != nil {
			unlock()
			return err
		}
		src, err := os.Open(path)
		if err != nil {
			unlock()
			return fmt.Errorf("export: %w", err)
		}
		sources = append(sources, src)
	}
	unlock()
	// Cancellation must release inode pins even if the writer never returns.
	stopClose := context.AfterFunc(ctx, closeSources)
	defer stopClose()
	zw, err := zstd.NewWriter(w)
	if err != nil {
		return fmt.Errorf("export: %w", err)
	}
	if err := writeArchive(ctx, zw, am, sources); err != nil {
		// No end of frame: the stream must not pass for a whole one.
		zw.Reset(io.Discard)
		zw.Close()
		return err
	}
	return zw.Close()
}

// planExport checks the snapshot can be exported and lists its files, before
// anything is written.
func (e *Engine) planExport(snapDir string, manifestJSON []byte, opts engine.SnapshotExport) (archiveManifest, []string, error) {
	m, found, err := readSnapshotManifest(snapDir)
	if err != nil {
		return archiveManifest{}, nil, fmt.Errorf("export: read local snapshot manifest: %w", err)
	}
	if !found {
		if err := json.Unmarshal(manifestJSON, &m); err != nil {
			return archiveManifest{}, nil, fmt.Errorf("export: parse snapshot manifest: %w", err)
		}
		m.Backing = nil
		for i := range m.Volumes {
			m.Volumes[i].Backing = nil
		}
	}
	m.normalize()
	names, err := archiveSnapshotFiles(m)
	if err != nil {
		return archiveManifest{}, nil, fmt.Errorf("export: %w", err)
	}
	if len(m.Mounts) > 0 {
		return archiveManifest{}, nil, fmt.Errorf("%w: the snapshot binds host directories (--mount), which another host doesn't have", engine.ErrNotSupported)
	}
	owner, err := snapshotExportOwner(e.cfg.DataDir, snapDir)
	if err != nil {
		return archiveManifest{}, nil, fmt.Errorf("export: %w", err)
	}
	vm := &VM{UserID: owner, SandboxDir: snapDir}
	var owned map[fileKey]bool
	resolve := func(name, format string, backing []diskLayer) (string, []diskLayer, error) {
		path := filepath.Join(snapDir, name)
		if found && format != "" {
			if backing == nil {
				backing = []diskLayer{}
			}
			if err := e.validateDiskBacking(vm, VMSpec{RootDisk: path, RootDiskFormat: format, Backing: backing}); err != nil {
				return "", nil, err
			}
			return format, backing, nil
		}
		if owned == nil {
			var err error
			owned, err = ownedNodeKeys(e.cfg.DataDir, owner)
			if err != nil {
				return "", nil, err
			}
		}
		authorize := func(next string) error {
			if err := e.authorizedBacking(vm, next, owned); err != nil {
				return fmt.Errorf("%w: %v", engine.ErrNotSupported, err)
			}
			return nil
		}
		if format == "raw" {
			return format, nil, authorize(path)
		}
		chain, err := readDiskChain(path, "", authorize)
		if err != nil {
			return "", nil, err
		}
		format = "qcow2"
		if len(chain) == 0 && !maybeQcow2(path) {
			format = "raw"
		}
		return format, chain, err
	}
	if m.DiskFormat, m.Backing, err = resolve(m.DiskFile, m.DiskFormat, m.Backing); err != nil {
		return archiveManifest{}, nil, fmt.Errorf("export: root disk: %w", err)
	}
	for i := range m.Volumes {
		v := &m.Volumes[i]
		if v.Format, v.Backing, err = resolve(v.File, v.Format, v.Backing); err != nil {
			return archiveManifest{}, nil, fmt.Errorf("export: volume %s: %w", v.File, err)
		}
	}
	if m.Arch == "" {
		m.Arch = hostSnapshotArch()
	}
	am := archiveManifest{
		Format:   archiveFormat,
		Name:     opts.Name,
		OS:       runtime.GOOS,
		Arch:     hostSnapshotArch(),
		Snapshot: portableManifest(m),
	}
	var paths []string
	add := func(name, path, sum string) error {
		fi, err := os.Stat(path)
		if err != nil {
			return fmt.Errorf("export: %w", err)
		}
		if !fi.Mode().IsRegular() {
			return fmt.Errorf("export: %s is not a regular file", path)
		}
		am.Files = append(am.Files, archiveFile{Name: name, Size: fi.Size(), SHA256: sum})
		paths = append(paths, path)
		return nil
	}
	for _, name := range names {
		path := filepath.Join(snapDir, name)
		var sum string
		if name == checkpointFile {
			s, _, err := hashFile(path)
			if err != nil {
				return archiveManifest{}, nil, fmt.Errorf("export: %w", err)
			}
			sum = s
		}
		if err := add(name, path, sum); err != nil {
			return archiveManifest{}, nil, err
		}
	}
	nodes := nodesDir(e.cfg.DataDir)
	var layerFiles []string
	layer := func(over string, member diskLayer) string {
		name := fmt.Sprintf("%s%d.img", archiveLayerPrefix, len(am.Layers)+1)
		am.Layers = append(am.Layers, archiveLayer{Name: name, Format: member.Format, Over: over})
		layerFiles = append(layerFiles, member.Path)
		return name
	}
	var base string
	over := m.DiskFile
	for _, member := range m.Backing {
		if member.Format == "raw" && !within(nodes, member.Path) {
			base = member.Path
			break
		}
		if !within(nodes, member.Path) {
			return archiveManifest{}, nil, fmt.Errorf("%w: the root disk is layered over %q, which isn't one of this host's snapshot disks", engine.ErrNotSupported, member.Path)
		}
		over = layer(over, member)
	}
	baseOver := over
	for _, v := range m.Volumes {
		over := v.File
		for _, member := range v.Backing {
			if !within(nodes, member.Path) {
				return archiveManifest{}, nil, fmt.Errorf("%w: volume %s is a qcow2 overlay over %q, which isn't one of this host's snapshot disks; only standalone volumes and volumes over snapshot disks can be exported", engine.ErrNotSupported, v.File, member.Path)
			}
			over = layer(over, member)
		}
	}
	for i, path := range layerFiles {
		if err := add(am.Layers[i].Name, path, ""); err != nil {
			return archiveManifest{}, nil, err
		}
	}
	if base == "" {
		return am, paths, nil
	}
	sum, size, err := hashFileCached(base)
	if err != nil {
		return archiveManifest{}, nil, fmt.Errorf("export: the root disk's base image: %w", err)
	}
	am.Base = &archiveBase{SHA256: sum, Size: size, Name: filepath.Base(base), Included: opts.IncludeBase, Over: baseOver}
	if opts.IncludeBase {
		if err := add(archiveBaseFile, base, sum); err != nil {
			return archiveManifest{}, nil, err
		}
	}
	return am, paths, nil
}

func snapshotExportOwner(dataDir, snapDir string) (string, error) {
	snapshots, err := filepath.Abs(filepath.Join(dataDir, "snapshots"))
	if err != nil {
		return "", err
	}
	path, err := filepath.Abs(snapDir)
	if err != nil {
		return "", err
	}
	rel, err := filepath.Rel(snapshots, path)
	if err != nil {
		return "", err
	}
	parts := strings.Split(rel, string(filepath.Separator))
	if len(parts) != 2 || parts[0] == ".." || !archiveFileName.MatchString(parts[0]) || !archiveFileName.MatchString(parts[1]) {
		return "", nil
	}
	// A snapshots/ mount may be shared, but a tenant or snapshot symlink
	// cannot transfer another tenant's authority to its lexical owner.
	for _, dir := range []string{filepath.Join(snapshots, parts[0]), path} {
		fi, err := os.Lstat(dir)
		if err != nil {
			return "", err
		}
		if !fi.IsDir() {
			return "", fmt.Errorf("snapshot directory %s is not a real directory", dir)
		}
	}
	return parts[0], nil
}

// portableManifest strips host-local secrets and paths but retains the exact
// network policy, including the sibling opt-in, across export and import.
func portableManifest(m krucibleSnapManifest) krucibleSnapManifest {
	m.KernelImage = ""
	m.Mounts = nil
	m.Backing = nil
	m.Volumes = slices.Clone(m.Volumes)
	for i := range m.Volumes {
		m.Volumes[i].Backing = nil
	}
	if m.Type != "memory" {
		m.Token = "" // a cold boot mints its own
	}
	if p := m.NetPolicy; p != nil {
		m.NetPolicy = &gateway.NetPolicyWire{
			Default:    p.Default,
			Siblings:   p.Siblings,
			AllowHosts: slices.Clone(p.AllowHosts),
			AllowCIDRs: slices.Clone(p.AllowCIDRs),
		}
	}
	return m
}

func writeArchive(ctx context.Context, w io.Writer, am archiveManifest, sources []*os.File) error {
	tw := tar.NewWriter(w)
	manifest, err := json.MarshalIndent(am, "", "  ")
	if err != nil {
		return fmt.Errorf("export: %w", err)
	}
	hdr := &tar.Header{
		Name:     archiveManifestFile,
		Mode:     0o600,
		Size:     int64(len(manifest)),
		Typeflag: tar.TypeReg,
		ModTime:  time.Now().Truncate(time.Second),
	}
	if err := tw.WriteHeader(hdr); err != nil {
		return fmt.Errorf("export: %w", err)
	}
	if _, err := tw.Write(manifest); err != nil {
		return fmt.Errorf("export: %w", err)
	}
	buf := make([]byte, archiveBufSize)
	for i, f := range am.Files {
		if err := copyIntoArchive(ctx, tw, sources[i], f, buf); err != nil {
			return err
		}
	}
	if err := tw.Close(); err != nil {
		return fmt.Errorf("export: %w", err)
	}
	return nil
}

func copyIntoArchive(ctx context.Context, tw *tar.Writer, src *os.File, f archiveFile, buf []byte) error {
	fi, err := src.Stat()
	if err != nil {
		return fmt.Errorf("export: %w", err)
	}
	if fi.Size() != f.Size {
		return fmt.Errorf("export: %s changed size during the export", f.Name)
	}
	hdr := &tar.Header{
		Name:     f.Name,
		Mode:     0o600,
		Size:     f.Size,
		Typeflag: tar.TypeReg,
		ModTime:  fi.ModTime().Truncate(time.Second),
	}
	if err := tw.WriteHeader(hdr); err != nil {
		return fmt.Errorf("export: %s: %w", f.Name, err)
	}
	if n, err := io.CopyBuffer(tw, ctxReader{ctx, io.LimitReader(src, f.Size)}, buf); err != nil || n != f.Size {
		return fmt.Errorf("export: %s: copied %d of %d bytes: %v", f.Name, n, f.Size, err)
	}
	return nil
}

// ImportSnapshot implements engine.SnapshotPorter.
func (e *Engine) ImportSnapshot(ctx context.Context, r io.Reader, destDir string) (imp engine.SnapshotImport, err error) {
	// Our exports use an 8 MiB window; refuse streams that would make the
	// decoder hold much more.
	zr, err := zstd.NewReader(r, zstd.WithDecoderMaxWindow(128<<20))
	if err != nil {
		return imp, fmt.Errorf("import: %w", err)
	}
	defer zr.Close()
	tr := tar.NewReader(zr)
	am, err := readArchiveManifest(tr)
	if err != nil {
		return imp, err
	}
	files, err := e.admitArchive(&am)
	if err != nil {
		return imp, err
	}
	// From finding the base until the root disk names it, GC must leave it be.
	unlock, err := lockBases(e.cfg.DataDir, false)
	if err != nil {
		return imp, fmt.Errorf("import: %w", err)
	}
	defer unlock()
	base, err := e.findBase(am.Base)
	if err != nil {
		return imp, err
	}
	if am.Base != nil && base == "" && !am.Base.Included {
		return imp, incompatible("missing base image sha256:%s (%s, %d bytes); export the snapshot with --include-base",
			am.Base.SHA256, am.Base.Name, am.Base.Size)
	}

	if err := os.Mkdir(destDir, 0o700); err != nil {
		return imp, fmt.Errorf("import: %w", err)
	}
	var adopted []string
	defer func() {
		if err != nil {
			os.RemoveAll(destDir)
			// Received entries have fresh identities: these links belong only
			// to this failed import, never to another snapshot.
			for _, node := range adopted {
				if cleanupErr := removeNodeMetadata(node); cleanupErr != nil {
					err = errors.Join(err, fmt.Errorf("import: remove node metadata: %w", cleanupErr))
					continue
				}
				os.Remove(node)
			}
		}
	}()
	buf := make([]byte, archiveBufSize)
	for _, f := range files {
		hdr, err := tr.Next()
		if err != nil {
			return imp, badArchive("%s is missing: %v", f.Name, err)
		}
		if hdr.Name != f.Name || hdr.Typeflag != tar.TypeReg || hdr.Size != f.Size {
			return imp, badArchive("expected %s (%d bytes), found %q (%d bytes)", f.Name, f.Size, hdr.Name, hdr.Size)
		}
		// A base this host already has only needs reading past.
		keep := !(f.Name == archiveBaseFile && base != "")
		if err := receiveFile(ctx, tr, destDir, f, keep, buf); err != nil {
			return imp, err
		}
		// Before the large files arrive.
		if f.Name == checkpointFile {
			if err := e.checkCheckpointHost(ctx, destDir); err != nil {
				return imp, err
			}
		}
	}
	if hdr, err := tr.Next(); err != io.EOF {
		if err == nil {
			return imp, badArchive("unexpected entry %q", hdr.Name)
		}
		return imp, badArchive("%v", err)
	}
	// Reading on to the end of the zstd stream checks its checksum.
	if _, err := io.CopyBuffer(io.Discard, zr, buf); err != nil {
		return imp, badArchive("%v", err)
	}

	if err := validateArchiveDisks(destDir, &am); err != nil {
		return imp, err
	}
	needBase := am.Base != nil && base == ""
	if needBase {
		base = e.keptBasePath(am.Base.SHA256, am.Base.Name)
	}
	if adopted, err = e.rebuildChains(destDir, &am, base); err != nil {
		return imp, err
	}
	if needBase {
		if _, err := e.keepBase(filepath.Join(destDir, archiveBaseFile), am.Base.SHA256, am.Base.Name); err != nil {
			return imp, err
		}
	}
	localSpec := VMSpec{
		RootDisk: filepath.Join(destDir, am.Snapshot.DiskFile), RootDiskFormat: am.Snapshot.DiskFormat,
		Backing: am.Snapshot.Backing, Volumes: make([]VMVolume, len(am.Snapshot.Volumes)),
	}
	for i, v := range am.Snapshot.Volumes {
		localSpec.Volumes[i] = VMVolume{
			Path: filepath.Join(destDir, v.File), Format: v.Format, ReadOnly: v.ReadOnly, Backing: v.Backing,
		}
	}
	if err := e.validateDiskBacking(&VM{}, localSpec); err != nil {
		return imp, fmt.Errorf("import: validate rebuilt disk backing: %w", err)
	}
	manifestJSON, err := json.Marshal(am.Snapshot)
	if err != nil {
		return imp, fmt.Errorf("import: %w", err)
	}
	if err := writeSnapshotManifest(destDir, am.Snapshot); err != nil {
		return imp, fmt.Errorf("import: write local snapshot manifest: %w", err)
	}
	return engine.SnapshotImport{Name: am.Name, Type: am.Snapshot.Type, ManifestJSON: manifestJSON}, nil
}

// PublishSnapshotImport implements engine.SnapshotPorter.
func (e *Engine) PublishSnapshotImport(ctx context.Context, stagingDir, finalDir string) (err error) {
	// Import's completed managed staging manifest is already a stable GC
	// root between leases. Reacquiring here serializes its name change with
	// snapshot enumeration without holding a lease in the HTTP handler.
	unlock, err := lockBasesCtx(ctx, e.cfg.DataDir, false)
	if err != nil {
		return fmt.Errorf("import: lock publication: %w", err)
	}
	defer unlock()
	if err := os.Rename(stagingDir, finalDir); err != nil {
		return fmt.Errorf("import: publish snapshot: %w", err)
	}
	defer func() {
		if err != nil {
			if cleanupErr := os.RemoveAll(finalDir); cleanupErr != nil {
				err = errors.Join(err, fmt.Errorf("import: remove failed publication: %w", cleanupErr))
			}
		}
	}()
	finalParent := filepath.Dir(finalDir)
	if err := syncNodeDirectory(finalParent); err != nil {
		return fmt.Errorf("import: sync publication directory: %w", err)
	}
	if stagingParent := filepath.Dir(stagingDir); stagingParent != finalParent {
		if err := syncNodeDirectory(stagingParent); err != nil {
			return fmt.Errorf("import: sync staging directory: %w", err)
		}
	}
	return nil
}

// Only extracted entries are inspected and normalized. Archive metadata, not
// the presence or contents of incoming header paths, defines every dependency.
func validateArchiveDisks(dir string, am *archiveManifest) error {
	below := make(map[string]*archiveLayer, len(am.Layers))
	for i := range am.Layers {
		l := &am.Layers[i]
		below[l.Over] = l
	}
	baseOver := ""
	if am.Base != nil {
		baseOver = cmp.Or(am.Base.Over, am.Snapshot.DiskFile)
	}
	for i := 0; i <= len(am.Snapshot.Volumes); i++ {
		disk := snapVolume{File: am.Snapshot.DiskFile, Format: am.Snapshot.DiskFormat}
		if i > 0 {
			disk = am.Snapshot.Volumes[i-1]
		}
		format := disk.Format
		for cur, depth := disk.File, 0; ; {
			next := below[cur]
			hasBase := cur == baseOver
			if format == "raw" {
				if next != nil || hasBase {
					return badArchive("%s is raw but has a backing dependency", cur)
				}
				break
			}
			depth++
			if depth > maxChainDepth {
				return badArchive("%s has more than %d qcow2 images", disk.File, maxChainDepth)
			}
			path := filepath.Join(dir, cur)
			backingFormat, err := qcow2BackingFormat(path)
			if err != nil {
				return badArchive("%s: %v", cur, err)
			}
			if next == nil && !hasBase {
				if err := rewriteArchiveBacking(path, "", "", backingFormat); err != nil {
					return badArchive("%s: %v", cur, err)
				}
				break
			}
			expected := "raw"
			if next != nil {
				// Older archives recorded a layer's format only in its
				// qcow2 parent. Never probe the layer's guest-controlled bytes.
				if next.Format == "" {
					if backingFormat == "" {
						return badArchive("%s has no explicit backing format or archive layer format", cur)
					}
					next.Format = backingFormat
				}
				expected = next.Format
			}
			if backingFormat != "" && backingFormat != expected {
				return badArchive("%s names its backing as %s, but the archive records %s", cur, backingFormat, expected)
			}
			backing := archiveBaseFile
			if next != nil {
				backing = next.Name
			}
			if err := rewriteArchiveBacking(path, backing, expected, backingFormat); err != nil {
				return badArchive("%s: %v", cur, err)
			}
			if next == nil {
				break
			}
			cur, format = next.Name, next.Format
		}
	}
	return nil
}

// Extracted qcow2 headers are staging data, not provenance. Replacing their
// references before publication prevents either a missing link or a terminal
// source-host path from changing the archive graph; explicit formats prevent
// the eventual reader from probing raw guest bytes.
func rewriteArchiveBacking(path, backing, format, headerFormat string) error {
	if len(backing) > qcow2MaxBackingName || (backing != "" && format != "raw" && format != "qcow2") {
		return fmt.Errorf("invalid archive backing reference")
	}
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return err
	}
	defer f.Close()
	ref, err := readQcow2BackingRef(f)
	if err != nil {
		return err
	}
	if backing == "" && ref.offset == 0 && ref.size == 0 {
		return nil
	}
	var offset, extensionsEnd int64
	if backing != "" {
		extensionsEnd, err = qcow2ExtensionsEnd(f, ref.cluster)
		if err != nil {
			return err
		}
		offset = extensionsEnd
		if headerFormat == "" {
			offset += 16
		}
		if offset+int64(len(backing)) > ref.cluster {
			return fmt.Errorf("backing name %q doesn't fit the qcow2 header", backing)
		}
	}
	if ref.offset != 0 && ref.size != 0 {
		var zero [qcow2MaxBackingName]byte
		if _, err := f.WriteAt(zero[:ref.size], ref.offset); err != nil {
			return err
		}
	}
	var pointer [12]byte
	if backing != "" {
		if headerFormat == "" {
			var extension [24]byte
			binary.BigEndian.PutUint32(extension[:], qcow2ExtBackingFmt)
			binary.BigEndian.PutUint32(extension[4:], uint32(len(format)))
			copy(extension[8:], format)
			if _, err := f.WriteAt(extension[:], extensionsEnd-8); err != nil {
				return err
			}
		}
		if _, err := f.WriteAt([]byte(backing), offset); err != nil {
			return err
		}
		binary.BigEndian.PutUint64(pointer[:], uint64(offset))
		binary.BigEndian.PutUint32(pointer[8:], uint32(len(backing)))
	}
	if _, err := f.WriteAt(pointer[:], 8); err != nil {
		return err
	}
	return f.Sync()
}

// rebuildChains makes each of the snapshot's disks name what is on this host:
// the base image by its path here, and each layer by its place in the node
// store, where the layer is put. It goes from the bottom of each chain up, so
// a header is rewritten before the file is linked into the store, and a node
// is never changed once it is there. Each layer's file in destDir is gone
// afterwards: the store holds it, and the disk above names it.
func (e *Engine) rebuildChains(destDir string, am *archiveManifest, base string) (nodes []string, err error) {
	below := make(map[string]archiveLayer, len(am.Layers))
	for _, l := range am.Layers {
		below[l.Over] = l
	}
	baseOver := ""
	if am.Base != nil {
		baseOver = cmp.Or(am.Base.Over, am.Snapshot.DiskFile)
	}
	for disk := 0; disk <= len(am.Snapshot.Volumes); disk++ {
		name, format := am.Snapshot.DiskFile, am.Snapshot.DiskFormat
		record := &am.Snapshot.Backing
		if disk > 0 {
			v := &am.Snapshot.Volumes[disk-1]
			name, format, record = v.File, v.Format, &v.Backing
		}
		chain := []diskLayer{{Path: name, Format: format}}
		for cur := name; ; {
			next, ok := below[cur]
			if !ok {
				break
			}
			chain = append(chain, diskLayer{Path: next.Name, Format: next.Format})
			cur = next.Name
		}
		hasBase := chain[len(chain)-1].Path == baseOver
		for i := len(chain) - 1; i >= 0; i-- {
			path := filepath.Join(destDir, chain[i].Path)
			var backing string
			if i+1 < len(chain) {
				backing = chain[i+1].Path
			} else if hasBase {
				backing = base
			}
			if backing != "" {
				if err := setQcow2Backing(path, backing); err != nil {
					return nodes, fmt.Errorf("import: %w", err)
				}
			}
			if i == 0 {
				break
			}
			dependencies := make([]diskLayer, len(chain)-i-1)
			copy(dependencies, chain[i+1:])
			if hasBase {
				dependencies = append(dependencies, diskLayer{Path: base, Format: "raw"})
			}
			node, err := e.adoptNode(path, chain[i].Format, dependencies)
			if err != nil {
				return nodes, fmt.Errorf("import: %w", err)
			}
			nodes = append(nodes, node)
			if err := os.Remove(path); err != nil {
				return nodes, fmt.Errorf("import: %w", err)
			}
			chain[i].Path = node
		}
		*record = make([]diskLayer, len(chain)-1)
		copy(*record, chain[1:])
		if hasBase {
			*record = append(*record, diskLayer{Path: base, Format: "raw"})
		}
	}
	return nodes, nil
}

func readArchiveManifest(tr *tar.Reader) (archiveManifest, error) {
	hdr, err := tr.Next()
	if err == io.EOF {
		return archiveManifest{}, badArchive("it is empty")
	}
	if err != nil {
		return archiveManifest{}, badArchive("%v", err)
	}
	if hdr.Name != archiveManifestFile || hdr.Typeflag != tar.TypeReg || hdr.Size > maxArchiveManifest {
		return archiveManifest{}, badArchive("it starts with %q, not %s", hdr.Name, archiveManifestFile)
	}
	var am archiveManifest
	if err := json.NewDecoder(tr).Decode(&am); err != nil {
		return archiveManifest{}, badArchive("%s: %v", archiveManifestFile, err)
	}
	return am, nil
}

// admitArchive refuses an archive this host can't take in, from its manifest
// alone, and returns the files that must follow it. It fills in the defaults
// the manifest leaves out.
func (e *Engine) admitArchive(am *archiveManifest) ([]archiveFile, error) {
	if am.Format < 1 || am.Format > archiveFormat {
		return nil, badArchive("archive format %d (this bhatti reads formats 1 through %d)", am.Format, archiveFormat)
	}
	m := &am.Snapshot
	// An archive supplies disks, not authority over paths on the importing
	// host. Only the validated, rebuilt graph can populate the local record.
	m.Backing = nil
	for i := range m.Volumes {
		m.Volumes[i].Backing = nil
	}
	if m.DiskFormat == "" {
		if am.Format == archiveFormat {
			return nil, badArchive("root disk has no format")
		}
		m.DiskFormat = "qcow2"
	}
	m.normalize()
	if host := hostSnapshotArch(); am.Arch != host || (m.Arch != "" && m.Arch != host) {
		return nil, incompatible("it was taken on %s; this host is %s", am.Arch, host)
	}
	switch m.Type {
	case "memory":
		if am.OS != runtime.GOOS {
			return nil, incompatible("its memory was saved on %s; this host runs %s", am.OS, runtime.GOOS)
		}
		if !e.caps.Checkpoint {
			return nil, errNoCheckpoint
		}
	case "filesystem":
	default:
		return nil, badArchive("unknown snapshot type %q", m.Type)
	}
	if m.Vcpus == 0 || m.MemMiB == 0 || len(m.Mounts) > 0 {
		return nil, badArchive("the snapshot manifest isn't one an export writes")
	}
	if m.NetPolicy != nil {
		if err := gateway.ValidateWire(*m.NetPolicy); err != nil {
			return nil, badArchive("%v", err)
		}
	}
	names, err := archiveSnapshotFiles(*m)
	if err != nil {
		return nil, err
	}
	// Layers follow the snapshot's own files, named in order. A header names
	// one backing file, so no two layers hang from the same entry, and every
	// layer must hang from a disk of the snapshot (a loop of layers
	// wouldn't).
	disks := map[string]bool{m.DiskFile: true}
	for _, v := range m.Volumes {
		disks[v.File] = true
	}
	isLayer := map[string]bool{}
	for i, l := range am.Layers {
		if l.Format != "raw" && l.Format != "qcow2" && !(am.Format < archiveFormat && l.Format == "") {
			return nil, badArchive("layer %s has unsupported format %q", l.Name, l.Format)
		}
		if want := fmt.Sprintf("%s%d.img", archiveLayerPrefix, i+1); l.Name != want {
			return nil, badArchive("layer %d is named %q; expected %s", i+1, l.Name, want)
		}
		isLayer[l.Name] = true
		names = append(names, l.Name)
	}
	below := map[string]string{} // entry → the layer its header names
	for _, l := range am.Layers {
		if _, taken := below[l.Over]; taken || l.Over == l.Name || !(disks[l.Over] || isLayer[l.Over]) {
			return nil, badArchive("layer %s hangs from %q", l.Name, l.Over)
		}
		below[l.Over] = l.Name
	}
	for i := range m.Volumes {
		v := &m.Volumes[i]
		if v.Format != "" {
			continue
		}
		if am.Format == archiveFormat {
			return nil, badArchive("volume %s has no format", v.File)
		}
		// Legacy exporters omitted the raw-volume default; only a declared
		// dependency establishes that an untyped volume was an overlay.
		v.Format = "raw"
		if _, layered := below[v.File]; layered {
			v.Format = "qcow2"
		}
	}
	reached := 0
	for d := range disks {
		for cur, depth := d, 0; ; reached++ {
			next, ok := below[cur]
			if !ok {
				break
			}
			cur = next
			depth++
			if depth > maxChainDepth {
				return nil, badArchive("%s has a backing chain deeper than %d", d, maxChainDepth)
			}
		}
	}
	if reached != len(am.Layers) {
		return nil, badArchive("its layers don't hang from the snapshot's disks")
	}
	if b := am.Base; b != nil {
		if !sha256Hex.MatchString(b.SHA256) || b.Size <= 0 {
			return nil, badArchive("bad base image identity")
		}
		// The base is named by the end of the root disk's chain.
		end := m.DiskFile
		for {
			next, ok := below[end]
			if !ok {
				break
			}
			end = next
		}
		if over := cmp.Or(b.Over, m.DiskFile); over != end {
			return nil, badArchive("the base image is named by %q, but the root disk's chain ends at %q", over, end)
		}
		if b.Included {
			names = append(names, archiveBaseFile)
		}
	}
	if len(am.Files) != len(names) {
		return nil, badArchive("it lists %d files; the snapshot has %d", len(am.Files), len(names))
	}
	files := slices.Clone(am.Files)
	for i, name := range names {
		f := &files[i]
		if f.Name != name || f.Size < 0 {
			return nil, badArchive("file %d is %q; expected %s", i, f.Name, name)
		}
		switch name {
		case checkpointFile:
			if !sha256Hex.MatchString(f.SHA256) {
				return nil, badArchive("%s has no checksum", name)
			}
		case archiveBaseFile:
			if f.Size != am.Base.Size {
				return nil, badArchive("%s is %d bytes; the base image is %d", name, f.Size, am.Base.Size)
			}
			f.SHA256 = am.Base.SHA256
		default:
			f.SHA256 = ""
		}
	}
	return files, nil
}

// receiveFile reads f's contents from r: into dir when keep (leaving its
// zero blocks as holes), else only past them. A checksum, when f has one, is
// checked.
func receiveFile(ctx context.Context, r io.Reader, dir string, f archiveFile, keep bool, buf []byte) error {
	src := io.Reader(ctxReader{ctx, r})
	h := sha256.New()
	if f.SHA256 != "" {
		src = io.TeeReader(src, h)
	}
	if keep {
		dst, err := os.OpenFile(filepath.Join(dir, f.Name), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err != nil {
			return fmt.Errorf("import: %w", err)
		}
		err = writeSparse(dst, src, f.Size, buf)
		if cerr := dst.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			return archiveReadError(f.Name, err)
		}
	} else if _, err := io.CopyBuffer(io.Discard, src, buf); err != nil {
		return archiveReadError(f.Name, readError{err})
	}
	if f.SHA256 != "" && hex.EncodeToString(h.Sum(nil)) != f.SHA256 {
		return badArchive("%s doesn't match its checksum", f.Name)
	}
	return nil
}

// readError marks a failure to read the archive (as opposed to writing a
// file out).
type readError struct{ error }

func (e readError) Unwrap() error { return e.error }

func archiveReadError(name string, err error) error {
	var re readError
	switch {
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return err
	case errors.As(err, &re):
		return badArchive("%s: %v", name, re.error)
	}
	return fmt.Errorf("import: %s: %w", name, err)
}

// writeSparse copies exactly size bytes from r to dst, skipping the all-zero
// 4 KiB blocks, so a sparse source (guest RAM, disk images) stays sparse.
func writeSparse(dst *os.File, r io.Reader, size int64, buf []byte) error {
	const block = 4096
	var zero [block]byte
	for off := int64(0); off < size; {
		chunk := buf[:min(int64(len(buf)), size-off)]
		if _, err := io.ReadFull(r, chunk); err != nil {
			return readError{err}
		}
		for i := 0; i < len(chunk); {
			end := min(i+block, len(chunk))
			if bytes.Equal(chunk[i:end], zero[:end-i]) {
				i = end
				continue
			}
			// Write a run of non-zero blocks at once.
			for end < len(chunk) {
				next := min(end+block, len(chunk))
				if bytes.Equal(chunk[end:next], zero[:next-end]) {
					break
				}
				end = next
			}
			if _, err := dst.WriteAt(chunk[i:end], off+int64(i)); err != nil {
				return err
			}
			i = end
		}
		off += int64(len(chunk))
	}
	return dst.Truncate(size)
}

type ctxReader struct {
	ctx context.Context
	r   io.Reader
}

func (c ctxReader) Read(p []byte) (int, error) {
	if err := c.ctx.Err(); err != nil {
		return 0, err
	}
	return c.r.Read(p)
}

// findBase looks for a base image with b's contents among this host's
// system-wide images: the bases under images/bases, the engine's base, and
// the tier images in <DataDir>/images. Users' own images aren't candidates: a
// snapshot must not reach another user's image by naming its hash. It returns
// the base itself, never a tier symlink to it (bases.go); "" when there is
// none.
func (e *Engine) findBase(b *archiveBase) (string, error) {
	if b == nil {
		return "", nil
	}
	var cands, rest []string
	if entries, err := os.ReadDir(basesDir(e.cfg.DataDir)); err == nil {
		for _, de := range entries {
			name := de.Name()
			path := filepath.Join(basesDir(e.cfg.DataDir), name)
			switch {
			case strings.HasPrefix(name, "."):
			case strings.HasSuffix(name, "-"+b.SHA256[:16]+".ext4") || name == "sha256-"+b.SHA256+".img":
				cands = append(cands, path) // named for these contents: likely the one
			default:
				rest = append(rest, path)
			}
		}
	}
	cands = append(cands, rest...)
	cands = append(cands, e.cfg.BaseImage, filepath.Join(e.cfg.DataDir, "base.img"))
	if entries, err := os.ReadDir(filepath.Join(e.cfg.DataDir, "images")); err == nil {
		for _, de := range entries {
			cands = append(cands, filepath.Join(e.cfg.DataDir, "images", de.Name()))
		}
	}
	seen := map[string]bool{}
	for _, c := range cands {
		if c == "" {
			continue
		}
		c, err := resolveBasePath(c)
		if err != nil || seen[c] {
			continue
		}
		seen[c] = true
		fi, err := os.Stat(c)
		if err != nil || !fi.Mode().IsRegular() || fi.Size() != b.Size {
			continue
		}
		sum, _, err := hashFileCached(c)
		if err != nil {
			return "", fmt.Errorf("import: hash %s: %w", c, err)
		}
		if sum == b.SHA256 {
			return c, nil
		}
	}
	return "", nil
}

// keptBaseStem matches the plain names a base kept from an archive takes its
// name from: the base's name on the exporting host, less the content hash and
// extension a bases/ name carries (rootfs-minimal-amd64 for
// rootfs-minimal-amd64-0123456789abcdef.ext4).
var keptBaseStem = regexp.MustCompile(`^([a-z0-9][a-z0-9._-]{0,62}?)(-[0-9a-f]{16})?(\.ext4|\.img)?$`)

func (e *Engine) keptBasePath(sum, name string) string {
	stem := "base"
	if m := keptBaseStem.FindStringSubmatch(name); m != nil {
		stem = m[1]
	}
	return filepath.Join(basesDir(e.cfg.DataDir), baseName(stem, sum))
}

var linkBase = os.Link

// keepBase moves a base image that arrived in an archive (already checked
// against sum, its sha256) into the bases directory, where later imports find
// it. A base there is never replaced: one already under the name is used if it
// holds the same bytes.
func (e *Engine) keepBase(src, sum, name string) (string, error) {
	dir := basesDir(e.cfg.DataDir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("import: %w", err)
	}
	dst := e.keptBasePath(sum, name)
	// Readable like an installed base: sandboxes' VMMs open it read-only.
	if err := os.Chmod(src, 0o644); err != nil {
		return "", fmt.Errorf("import: %w", err)
	}
	// Link, not rename: it never replaces a file already there.
	err := linkBase(src, dst)
	if err != nil && !errors.Is(err, fs.ErrExist) {
		// Hard links unavailable: copy beside the bases, then publish by rename.
		tmp := filepath.Join(dir, "."+filepath.Base(dst)+".partial")
		if cerr := cloneFile(src, tmp); cerr != nil {
			os.Remove(tmp)
			return "", fmt.Errorf("import: keep base image: %w", errors.Join(err, cerr))
		}
		err = os.Rename(tmp, dst)
		os.Remove(tmp)
		if err != nil && !errors.Is(err, fs.ErrExist) {
			return "", fmt.Errorf("import: keep base image: %w", err)
		}
	}
	if err != nil { // dst was there already
		have, _, herr := hashFileCached(dst)
		if herr != nil {
			return "", fmt.Errorf("import: %w", herr)
		}
		if have != sum {
			return "", fmt.Errorf("import: %s already holds another image", dst)
		}
	}
	os.Remove(src)
	return dst, nil
}

// checkCheckpointHost asks bhatti-vmm whether this host can restore the
// checkpoint in dir (it reads only checkpoint.bin): libkrun's verdict, the one
// a restore here would reach.
func (e *Engine) checkCheckpointHost(ctx context.Context, dir string) (retErr error) {
	cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(cctx, e.cfg.VMMBinary, "check-checkpoint", dir)
	if e.cfg.LibDir != "" {
		cmd.Env = append(os.Environ(),
			"DYLD_FALLBACK_LIBRARY_PATH="+e.cfg.LibDir,
			"LD_LIBRARY_PATH="+e.cfg.LibDir,
		)
	}
	if e.confineVMM {
		policy, cleanup, err := e.confineCheckpoint(dir)
		if err != nil {
			return fmt.Errorf("check-checkpoint confinement: %w", err)
		}
		defer func() {
			if err := cleanup(); err != nil {
				retErr = errors.Join(retErr, fmt.Errorf("check-checkpoint cleanup: %w", err))
			}
		}()
		if cmd.Env == nil {
			cmd.Env = os.Environ()
		}
		cmd.Env = append(cmd.Env, VMMPolicyEnv+"="+policy)
	}
	var stderr strings.Builder
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		lines := strings.Split(strings.TrimSpace(stderr.String()), "\n")
		for _, line := range lines {
			if strings.HasPrefix(line, "vmm: confine: fatal: ") {
				return fmt.Errorf("check-checkpoint confinement: %s", line)
			}
		}
		// A missing Landlock ABI is a nonfatal warning for a probe, not
		// libkrun's verdict about the checkpoint.
		for i := len(lines) - 1; i >= 0; i-- {
			if why, ok := strings.CutPrefix(lines[i], "vmm: check-checkpoint: "); ok {
				return incompatible("%s", strings.TrimPrefix(why, "checkpoint: "))
			}
		}
		// Failure without a checkpoint verdict is not proof of incompatibility.
		return fmt.Errorf("check-checkpoint: %s: %w", strings.TrimSpace(stderr.String()), err)
	}
	return nil
}

func hashFile(path string) (string, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.CopyBuffer(h, f, make([]byte, archiveBufSize))
	if err != nil {
		return "", 0, err
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}

// baseHashes caches base image hashes by path and file identity: an export or
// an import would otherwise read a whole image each time.
var baseHashes = struct {
	sync.Mutex
	m map[string]fileHash
}{m: map[string]fileHash{}}

type fileHash struct {
	size  int64
	mtime time.Time
	ino   uint64
	sum   string
}

func hashFileCached(path string) (string, int64, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return "", 0, err
	}
	if !fi.Mode().IsRegular() {
		return "", 0, fmt.Errorf("%s is not a regular file", path)
	}
	id := fileHash{size: fi.Size(), mtime: fi.ModTime()}
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		id.ino = uint64(st.Ino)
	}
	baseHashes.Lock()
	c, ok := baseHashes.m[path]
	baseHashes.Unlock()
	if ok && c.size == id.size && c.mtime.Equal(id.mtime) && c.ino == id.ino {
		return c.sum, c.size, nil
	}
	sum, n, err := hashFile(path)
	if err != nil {
		return "", 0, err
	}
	if n != id.size {
		return "", 0, fmt.Errorf("%s changed while being hashed", path)
	}
	id.sum = sum
	baseHashes.Lock()
	baseHashes.m[path] = id
	baseHashes.Unlock()
	return sum, n, nil
}
