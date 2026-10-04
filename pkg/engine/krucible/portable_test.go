package krucible

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"syscall"
	"testing"

	"github.com/klauspost/compress/zstd"

	"github.com/sahil-shubham/bhatti/pkg/engine"
	"github.com/sahil-shubham/bhatti/pkg/gateway"
)

// portableEngine is an Engine with only what snapshot export and import use,
// on a data dir of its own: base is its base image, and check is the body of
// the shell script standing in for `bhatti-vmm check-checkpoint <dir>`. Each
// call logs the directory's listing to check.log next to the script.
func portableEngine(t *testing.T, base, check string) *Engine {
	t.Helper()
	root := t.TempDir()
	vmm := filepath.Join(root, "bhatti-vmm")
	script := "#!/bin/sh\nls \"$2\" >> " + filepath.Join(root, "check.log") + "\n" + check + "\n"
	if err := os.WriteFile(vmm, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	data := filepath.Join(root, "data")
	if err := os.MkdirAll(filepath.Join(data, "snapshots"), 0o700); err != nil {
		t.Fatal(err)
	}
	return &Engine{
		cfg:  Config{DataDir: data, BaseImage: base, VMMBinary: vmm},
		caps: VMMCapabilities{Pause: true, Checkpoint: true},
	}
}

func checkLog(e *Engine) string {
	b, _ := os.ReadFile(filepath.Join(filepath.Dir(e.cfg.VMMBinary), "check.log"))
	return string(b)
}

// writeBase writes a raw base image whose contents depend on seed.
func writeBase(t *testing.T, path string, seed byte) []byte {
	t.Helper()
	data := make([]byte, 3<<20)
	for i := range data {
		data[i] = byte(i*7) ^ seed
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	return data
}

func writeFile(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

// fakeSnapshot lays out a snapshot directory as Checkpoint leaves one: for a
// memory snapshot a checkpoint and a sparse RAM image, then a root overlay
// over base holding the sandbox's writes, and a data volume.
func fakeSnapshot(t *testing.T, base, typ string) (string, krucibleSnapManifest) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "dev-ready")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if typ == "memory" {
		writeFile(t, filepath.Join(dir, checkpointFile), append([]byte("KRUNCKPT"), bytes.Repeat([]byte{7, 1}, 2048)...))
		mem, err := os.Create(filepath.Join(dir, memoryFile))
		if err != nil {
			t.Fatal(err)
		}
		if err := mem.Truncate(64 << 20); err != nil {
			t.Fatal(err)
		}
		mem.WriteAt(bytes.Repeat([]byte("ram!"), 2048), 1<<20)
		mem.WriteAt(bytes.Repeat([]byte("tmpfs"), 800), 40<<20)
		mem.Close()
	}
	fi, err := os.Stat(base)
	if err != nil {
		t.Fatal(err)
	}
	disk := filepath.Join(dir, "rootfs.qcow2")
	if err := createQcow2Overlay(disk, base, uint64(fi.Size())); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(disk, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	f.WriteAt([]byte("what the sandbox wrote"), 5*qcow2ClusterSize)
	f.Close()
	writeFile(t, filepath.Join(dir, "vol0.img"), bytes.Repeat([]byte("volume"), 300_000))
	m := krucibleSnapManifest{
		Arch: hostSnapshotArch(), Type: typ, Vcpus: 2, MemMiB: 64,
		DiskFile: "rootfs.qcow2", ConfigFile: "config.ext4",
		Token: "agent-token", KernelImage: "/opt/bhatti/vmlinux",
		Volumes: []snapVolume{{File: "vol0.img", ReadOnly: true}},
	}
	if typ == "memory" {
		m.NetPolicy = &gateway.NetPolicyWire{Default: "deny", AllowHosts: []string{"api.example.com"}}
	}
	return dir, m
}

func exportArchive(t *testing.T, e *Engine, dir string, m krucibleSnapManifest, includeBase bool) []byte {
	t.Helper()
	raw, _ := json.Marshal(m)
	var buf bytes.Buffer
	if err := e.ExportSnapshot(context.Background(), &buf, dir, raw, engine.SnapshotExport{Name: "dev-ready", IncludeBase: includeBase}); err != nil {
		t.Fatalf("export: %v", err)
	}
	return buf.Bytes()
}

func importArchive(e *Engine, archive []byte) (string, engine.SnapshotImport, error) {
	dest := filepath.Join(e.cfg.DataDir, "snapshots", "import-1.tmp")
	imp, err := e.ImportSnapshot(context.Background(), bytes.NewReader(archive), dest)
	return dest, imp, err
}

func fileSum(t *testing.T, path string) string {
	t.Helper()
	sum, _, err := hashFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return sum
}

func mustNotExist(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("%s exists after a refused import (%v)", path, err)
	}
}

// TestSnapshotMovesToAnotherHost exports a memory snapshot and imports it on
// a host that keeps the same base image under another name: every file
// arrives intact (guest RAM still sparse), the root disk now backs onto that
// host's copy, the manifest keeps the network posture and drops host paths,
// and the CPU check ran before the RAM image arrived.
func TestSnapshotMovesToAnotherHost(t *testing.T) {
	baseA := filepath.Join(t.TempDir(), "images", "rootfs-minimal-amd64.ext4")
	data := writeBase(t, baseA, 1)
	a := portableEngine(t, baseA, "exit 0")
	dir, m := fakeSnapshot(t, baseA, "memory")
	archive := exportArchive(t, a, dir, m, false)

	b := portableEngine(t, "", "exit 0")
	baseB := filepath.Join(b.cfg.DataDir, "images", "rootfs-minimal-amd64-renamed.ext4")
	writeFile(t, baseB, data)
	dest, imp, err := importArchive(b, archive)
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	if imp.Name != "dev-ready" || imp.Type != "memory" {
		t.Fatalf("imported %+v", imp)
	}
	for _, name := range []string{checkpointFile, memoryFile, "vol0.img"} {
		if got, want := fileSum(t, filepath.Join(dest, name)), fileSum(t, filepath.Join(dir, name)); got != want {
			t.Fatalf("%s changed on the way", name)
		}
	}
	if got, err := qcow2Backing(filepath.Join(dest, "rootfs.qcow2")); err != nil || got != baseB {
		t.Fatalf("root disk backs onto %q (%v), want this host's %s", got, err, baseB)
	}
	src, _ := os.ReadFile(filepath.Join(dir, "rootfs.qcow2"))
	got, _ := os.ReadFile(filepath.Join(dest, "rootfs.qcow2"))
	if len(got) != len(src) || !bytes.Equal(got[qcow2ClusterSize:], src[qcow2ClusterSize:]) {
		t.Fatal("the root disk's clusters changed on the way")
	}
	fi, err := os.Stat(filepath.Join(dest, memoryFile))
	if err != nil {
		t.Fatal(err)
	}
	if st, ok := fi.Sys().(*syscall.Stat_t); ok && st.Blocks*512 > 8<<20 {
		t.Fatalf("memory.bin landed with %d bytes allocated for 2 pages of data", st.Blocks*512)
	}

	var got2 krucibleSnapManifest
	if err := json.Unmarshal(imp.ManifestJSON, &got2); err != nil {
		t.Fatal(err)
	}
	if got2.KernelImage != "" || got2.Token != "agent-token" || got2.Vcpus != 2 || got2.MemMiB != 64 ||
		got2.NetPolicy == nil || got2.NetPolicy.Default != "deny" || len(got2.NetPolicy.AllowHosts) != 1 ||
		len(got2.Volumes) != 1 || !got2.Volumes[0].ReadOnly {
		t.Fatalf("imported manifest %+v", got2)
	}
	if bytes.Contains(archiveManifestBytes(t, archive), []byte(baseA)) {
		t.Fatal("the archive names the exporting host's base image path")
	}
	if log := checkLog(b); log != checkpointFile+"\n" {
		t.Fatalf("checkpoint check saw %q; want it to run with only %s received", log, checkpointFile)
	}
}

func archiveManifestBytes(t *testing.T, archive []byte) []byte {
	t.Helper()
	zr, err := zstd.NewReader(bytes.NewReader(archive))
	if err != nil {
		t.Fatal(err)
	}
	defer zr.Close()
	tr := tar.NewReader(zr)
	if _, err := tr.Next(); err != nil {
		t.Fatal(err)
	}
	b, err := io.ReadAll(tr)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// TestSnapshotImportNeedsTheBaseImage: a host without the base is refused,
// naming the image; carried in the archive, the base is kept for later
// imports, which then find it by content.
func TestSnapshotImportNeedsTheBaseImage(t *testing.T) {
	baseA := filepath.Join(t.TempDir(), "rootfs.ext4")
	writeBase(t, baseA, 1)
	sum := fileSum(t, baseA)
	a := portableEngine(t, baseA, "exit 0")
	dir, m := fakeSnapshot(t, baseA, "filesystem")
	without := exportArchive(t, a, dir, m, false)

	other := filepath.Join(t.TempDir(), "rootfs.ext4")
	writeBase(t, other, 2) // same size, other contents
	b := portableEngine(t, other, "exit 0")
	dest, _, err := importArchive(b, without)
	if !errors.Is(err, engine.ErrSnapshotIncompatible) ||
		!strings.Contains(err.Error(), "missing base image sha256:"+sum+" (rootfs.ext4") ||
		!strings.Contains(err.Error(), "--include-base") {
		t.Fatalf("import without the base: %v", err)
	}
	mustNotExist(t, dest)

	if _, _, err := importArchive(b, exportArchive(t, a, dir, m, true)); err != nil {
		t.Fatalf("import with the base: %v", err)
	}
	kept := filepath.Join(b.cfg.DataDir, "images", "bases", "sha256-"+sum+".img")
	if got := fileSum(t, kept); got != sum {
		t.Fatalf("kept base has sha256 %s, want %s", got, sum)
	}
	if got, err := qcow2Backing(filepath.Join(dest, "rootfs.qcow2")); err != nil || got != kept {
		t.Fatalf("root disk backs onto %q (%v), want %s", got, err, kept)
	}
	os.RemoveAll(dest)
	if _, _, err := importArchive(b, without); err != nil {
		t.Fatalf("import without the base after one that carried it: %v", err)
	}
	if got, _ := qcow2Backing(filepath.Join(dest, "rootfs.qcow2")); got != kept {
		t.Fatalf("root disk backs onto %q, want the kept %s", got, kept)
	}
}

type archiveEntry struct {
	name string
	data []byte
}

// craftArchive packs entries after the manifest am, as an export would.
func craftArchive(t *testing.T, am archiveManifest, entries []archiveEntry) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw, err := zstd.NewWriter(&buf)
	if err != nil {
		t.Fatal(err)
	}
	tw := tar.NewWriter(zw)
	manifest, _ := json.Marshal(am)
	for _, e := range append([]archiveEntry{{archiveManifestFile, manifest}}, entries...) {
		if err := tw.WriteHeader(&tar.Header{Name: e.name, Mode: 0o600, Size: int64(len(e.data)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		tw.Write(e.data)
	}
	tw.Close()
	zw.Close()
	return buf.Bytes()
}

// smallArchive is a minimal, valid memory snapshot archive over base, as its
// manifest and entries, to damage one way at a time.
func smallArchive(t *testing.T, base string) (archiveManifest, []archiveEntry) {
	t.Helper()
	ckpt := []byte("KRUNCKPT checkpoint")
	sum := sha256.Sum256(ckpt)
	fi, _ := os.Stat(base)
	disk := filepath.Join(t.TempDir(), "rootfs.qcow2")
	if err := createQcow2Overlay(disk, base, uint64(fi.Size())); err != nil {
		t.Fatal(err)
	}
	diskData, _ := os.ReadFile(disk)
	entries := []archiveEntry{{checkpointFile, ckpt}, {memoryFile, make([]byte, 8192)}, {"rootfs.qcow2", diskData}}
	am := archiveManifest{
		Format: archiveFormat, Name: "small", OS: runtime.GOOS, Arch: hostSnapshotArch(),
		Snapshot: krucibleSnapManifest{Arch: hostSnapshotArch(), Type: "memory", Vcpus: 1, MemMiB: 1, DiskFile: "rootfs.qcow2", Token: "t"},
		Base:     &archiveBase{SHA256: fileSum(t, base), Size: fi.Size(), Name: "rootfs.ext4"},
	}
	for _, e := range entries {
		f := archiveFile{Name: e.name, Size: int64(len(e.data))}
		if e.name == checkpointFile {
			f.SHA256 = hex.EncodeToString(sum[:])
		}
		am.Files = append(am.Files, f)
	}
	return am, entries
}

// TestSnapshotImportRefusesDamagedArchives: whatever is wrong with an archive,
// the import fails as a bad archive and leaves nothing behind.
func TestSnapshotImportRefusesDamagedArchives(t *testing.T) {
	base := filepath.Join(t.TempDir(), "rootfs.ext4")
	writeBase(t, base, 1)
	e := portableEngine(t, base, "exit 0")
	am, entries := smallArchive(t, base)
	good := craftArchive(t, am, entries)
	if dest, _, err := importArchive(e, good); err != nil {
		t.Fatalf("the undamaged archive: %v", err)
	} else {
		os.RemoveAll(dest)
	}

	dir, m := fakeSnapshot(t, base, "memory")
	real := exportArchive(t, e, dir, m, false)
	flipped := bytes.Clone(real)
	flipped[len(flipped)*2/3] ^= 0x40
	var outOfPlace bytes.Buffer // a tar that doesn't start with the manifest
	zw, _ := zstd.NewWriter(&outOfPlace)
	tw := tar.NewWriter(zw)
	tw.WriteHeader(&tar.Header{Name: "rootfs.qcow2", Mode: 0o600, Size: 1, Typeflag: tar.TypeReg})
	tw.Write([]byte{1})
	tw.Close()
	zw.Close()
	cases := map[string][]byte{
		"empty":              nil,
		"not zstd":           []byte("this is not a snapshot archive"),
		"truncated":          real[:len(real)/2],
		"bit flipped":        flipped,
		"manifest not first": outOfPlace.Bytes(),
	}
	damage := map[string]func(am *archiveManifest, entries []archiveEntry) []archiveEntry{
		"a listed file is missing": func(_ *archiveManifest, en []archiveEntry) []archiveEntry { return en[:2] },
		"an unlisted entry follows": func(_ *archiveManifest, en []archiveEntry) []archiveEntry {
			return append(en, archiveEntry{"extra", []byte("x")})
		},
		"entries out of order": func(_ *archiveManifest, en []archiveEntry) []archiveEntry {
			return []archiveEntry{en[1], en[0], en[2]}
		},
		"checkpoint doesn't match its checksum": func(_ *archiveManifest, en []archiveEntry) []archiveEntry {
			en[0] = archiveEntry{checkpointFile, []byte("KRUNCKPT tampered!!")}
			return en
		},
		"a file isn't the size listed": func(am *archiveManifest, en []archiveEntry) []archiveEntry {
			am.Files[1].Size++
			return en
		},
		"path traversal": func(am *archiveManifest, en []archiveEntry) []archiveEntry {
			am.Snapshot.DiskFile = "../escape"
			am.Files[2].Name = "../escape"
			en[2].name = "../escape"
			return en
		},
		"unknown format": func(am *archiveManifest, en []archiveEntry) []archiveEntry {
			am.Format = 2
			return en
		},
		"bad egress policy": func(am *archiveManifest, en []archiveEntry) []archiveEntry {
			am.Snapshot.NetPolicy = &gateway.NetPolicyWire{Default: "everything"}
			return en
		},
	}
	for name, mutate := range damage {
		am2, _ := smallArchive(t, base)
		en := mutate(&am2, slices.Clone(entries))
		cases[name] = craftArchive(t, am2, en)
	}
	for name, archive := range cases {
		t.Run(name, func(t *testing.T) {
			dest, _, err := importArchive(e, archive)
			if !errors.Is(err, engine.ErrBadSnapshotArchive) {
				t.Fatalf("want a bad-archive refusal, got %v", err)
			}
			t.Log(err)
			mustNotExist(t, dest)
			mustNotExist(t, filepath.Join(e.cfg.DataDir, "images", "bases"))
		})
	}
}

// TestSnapshotImportRefusesHostsThatCantRunIt: another architecture or OS, no
// checkpoint support, or libkrun's CPU verdict refuse a memory snapshot,
// before any of its RAM arrives; a disk-only snapshot needs none of that.
func TestSnapshotImportRefusesHostsThatCantRunIt(t *testing.T) {
	base := filepath.Join(t.TempDir(), "rootfs.ext4")
	writeBase(t, base, 1)

	t.Run("architecture", func(t *testing.T) {
		e := portableEngine(t, base, "exit 0")
		am, entries := smallArchive(t, base)
		am.Arch, am.Snapshot.Arch = "riscv64", "riscv64"
		dest, _, err := importArchive(e, craftArchive(t, am, entries))
		if !errors.Is(err, engine.ErrSnapshotIncompatible) || !strings.Contains(err.Error(), "taken on riscv64; this host is "+hostSnapshotArch()) {
			t.Fatalf("got %v", err)
		}
		mustNotExist(t, dest)
	})
	t.Run("os", func(t *testing.T) {
		e := portableEngine(t, base, "exit 0")
		am, entries := smallArchive(t, base)
		am.OS = "plan9"
		if _, _, err := importArchive(e, craftArchive(t, am, entries)); !errors.Is(err, engine.ErrSnapshotIncompatible) || !strings.Contains(err.Error(), "saved on plan9") {
			t.Fatalf("got %v", err)
		}
	})
	t.Run("no checkpoint support", func(t *testing.T) {
		e := portableEngine(t, base, "exit 0")
		e.caps.Checkpoint = false
		am, entries := smallArchive(t, base)
		if _, _, err := importArchive(e, craftArchive(t, am, entries)); !errors.Is(err, engine.ErrNotSupported) {
			t.Fatalf("got %v", err)
		}
		// A disk-only snapshot cold-boots: no checkpoint support needed.
		dir, m := fakeSnapshot(t, base, "filesystem")
		if _, _, err := importArchive(e, exportArchive(t, e, dir, m, false)); err != nil {
			t.Fatalf("filesystem snapshot: %v", err)
		}
	})
	t.Run("cpu", func(t *testing.T) {
		const why = "CPU vendor differs: the checkpoint was taken on GenuineIntel family 6 model 158 stepping 13; this host is AuthenticAMD family 25 model 33 stepping 0"
		e := portableEngine(t, base, "echo 'vmm: check-checkpoint: checkpoint: "+why+"' >&2; exit 1")
		am, entries := smallArchive(t, base)
		dest, _, err := importArchive(e, craftArchive(t, am, entries))
		if !errors.Is(err, engine.ErrSnapshotIncompatible) || !strings.HasSuffix(err.Error(), ": "+why) {
			t.Fatalf("got %v", err)
		}
		mustNotExist(t, dest)
		if log := checkLog(e); log != checkpointFile+"\n" {
			t.Fatalf("the check saw %q; the refusal must come before the RAM image arrives", log)
		}
	})
}

// TestSnapshotExportRefusesBeforeWriting: what makes a snapshot unexportable
// is found before the first byte goes out, so the caller can still answer
// with an error instead of a broken archive.
func TestSnapshotExportRefusesBeforeWriting(t *testing.T) {
	base := filepath.Join(t.TempDir(), "rootfs.ext4")
	writeBase(t, base, 1)
	e := portableEngine(t, base, "exit 0")
	cases := map[string]func(t *testing.T, dir string, m *krucibleSnapManifest){
		"host directory mount": func(_ *testing.T, _ string, m *krucibleSnapManifest) {
			m.Mounts = []snapMount{{HostPath: "/srv/data"}}
		},
		"base image gone": func(t *testing.T, dir string, _ *krucibleSnapManifest) {
			disk := filepath.Join(dir, "rootfs.qcow2")
			if err := setQcow2Backing(disk, filepath.Join(t.TempDir(), "gone.ext4")); err != nil {
				t.Fatal(err)
			}
		},
		"checkpoint missing": func(t *testing.T, dir string, _ *krucibleSnapManifest) {
			os.Remove(filepath.Join(dir, checkpointFile))
		},
		"volume over a backing file": func(t *testing.T, dir string, _ *krucibleSnapManifest) {
			if err := createQcow2Overlay(filepath.Join(dir, "vol0.img"), base, 1<<20); err != nil {
				t.Fatal(err)
			}
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			dir, m := fakeSnapshot(t, base, "memory")
			mutate(t, dir, &m)
			raw, _ := json.Marshal(m)
			var buf bytes.Buffer
			err := e.ExportSnapshot(context.Background(), &buf, dir, raw, engine.SnapshotExport{Name: "x"})
			if err == nil || buf.Len() != 0 {
				t.Fatalf("export: err=%v after writing %d bytes; want an error before any", err, buf.Len())
			}
		})
	}
}
