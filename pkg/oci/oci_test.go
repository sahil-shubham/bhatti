package oci

import (
	"archive/tar"
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/tarball"
)

// makeLayer creates a v1.Layer from a list of tar entries.
func makeLayer(t *testing.T, entries []tarEntry) v1.Layer {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, e := range entries {
		switch {
		case e.dir:
			tw.WriteHeader(&tar.Header{Name: e.name, Typeflag: tar.TypeDir, Mode: 0755})
		case e.link != "":
			tw.WriteHeader(&tar.Header{Name: e.name, Typeflag: tar.TypeSymlink, Linkname: e.link})
		case e.hardlink != "":
			tw.WriteHeader(&tar.Header{Name: e.name, Typeflag: tar.TypeLink, Linkname: e.hardlink})
		default:
			hdr := &tar.Header{
				Name:     e.name,
				Typeflag: tar.TypeReg,
				Mode:     int64(e.mode),
				Size:     int64(len(e.content)),
				Uid:      e.uid,
				Gid:      e.gid,
			}
			if hdr.Mode == 0 {
				hdr.Mode = 0644
			}
			tw.WriteHeader(hdr)
			tw.Write([]byte(e.content))
		}
	}
	tw.Close()

	layer, err := tarball.LayerFromReader(&buf)
	if err != nil {
		t.Fatal(err)
	}
	return layer
}

type tarEntry struct {
	name     string
	content  string
	dir      bool
	link     string
	hardlink string
	mode     int
	uid, gid int
}

func readFileContent(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// --- Layer extraction tests ---

func TestExtractLayerBasic(t *testing.T) {
	dir := t.TempDir()
	layer := makeLayer(t, []tarEntry{
		{name: "etc/", dir: true},
		{name: "etc/config", content: "hello"},
		{name: "bin/", dir: true},
		{name: "bin/app", content: "binary", mode: 0755},
	})

	if err := extractLayer(layer, dir); err != nil {
		t.Fatal(err)
	}

	if got := readFileContent(t, filepath.Join(dir, "etc/config")); got != "hello" {
		t.Fatalf("expected 'hello', got %q", got)
	}
	info, _ := os.Stat(filepath.Join(dir, "bin/app"))
	if info.Mode()&0755 != 0755 {
		t.Fatalf("expected mode 0755, got %o", info.Mode())
	}
}

func TestExtractLayerWhiteoutFile(t *testing.T) {
	dir := t.TempDir()

	// Layer 1: create files
	l1 := makeLayer(t, []tarEntry{
		{name: "etc/config", content: "v1"},
		{name: "etc/keep", content: "keep"},
	})
	if err := extractLayer(l1, dir); err != nil {
		t.Fatal(err)
	}

	// Layer 2: delete etc/config via whiteout
	l2 := makeLayer(t, []tarEntry{
		{name: "etc/.wh.config"},
	})
	if err := extractLayer(l2, dir); err != nil {
		t.Fatal(err)
	}

	if _, err := os.Stat(filepath.Join(dir, "etc/config")); !os.IsNotExist(err) {
		t.Fatal("expected etc/config to be deleted by whiteout")
	}
	if readFileContent(t, filepath.Join(dir, "etc/keep")) != "keep" {
		t.Fatal("etc/keep should still exist")
	}
}

func TestExtractLayerWhiteoutOpaque(t *testing.T) {
	dir := t.TempDir()

	// Layer 1: create dir/a, dir/b, dir/c
	l1 := makeLayer(t, []tarEntry{
		{name: "dir/", dir: true},
		{name: "dir/a", content: "a"},
		{name: "dir/b", content: "b"},
		{name: "dir/c", content: "c"},
	})
	if err := extractLayer(l1, dir); err != nil {
		t.Fatal(err)
	}

	// Layer 2: opaque whiteout + new files d
	l2 := makeLayer(t, []tarEntry{
		{name: "dir/.wh..wh..opq"},
		{name: "dir/d", content: "d"},
	})
	if err := extractLayer(l2, dir); err != nil {
		t.Fatal(err)
	}

	// a, b, c should be gone; d should exist
	for _, f := range []string{"a", "b", "c"} {
		if _, err := os.Stat(filepath.Join(dir, "dir", f)); !os.IsNotExist(err) {
			t.Fatalf("expected dir/%s to be deleted by opaque whiteout", f)
		}
	}
	if readFileContent(t, filepath.Join(dir, "dir/d")) != "d" {
		t.Fatal("dir/d should exist")
	}
}

func TestExtractLayerSymlinks(t *testing.T) {
	dir := t.TempDir()
	layer := makeLayer(t, []tarEntry{
		{name: "real", content: "target"},
		{name: "link", link: "real"},
	})
	if err := extractLayer(layer, dir); err != nil {
		t.Fatal(err)
	}

	target, err := os.Readlink(filepath.Join(dir, "link"))
	if err != nil {
		t.Fatal(err)
	}
	if target != "real" {
		t.Fatalf("expected symlink to 'real', got %q", target)
	}
}

func TestExtractLayerPermissions(t *testing.T) {
	dir := t.TempDir()
	layer := makeLayer(t, []tarEntry{
		{name: "script", content: "#!/bin/sh", mode: 0755},
		{name: "secret", content: "key", mode: 0600},
	})
	if err := extractLayer(layer, dir); err != nil {
		t.Fatal(err)
	}

	info, _ := os.Stat(filepath.Join(dir, "script"))
	if info.Mode().Perm()&0755 != 0755 {
		t.Fatalf("expected 0755, got %o", info.Mode().Perm())
	}
	info, _ = os.Stat(filepath.Join(dir, "secret"))
	if info.Mode().Perm()&0600 != 0600 {
		t.Fatalf("expected 0600, got %o", info.Mode().Perm())
	}
}

func TestExtractLayerPathTraversal(t *testing.T) {
	dir := t.TempDir()
	// Create a layer with a path traversal attempt
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	tw.WriteHeader(&tar.Header{
		Name: "../../etc/passwd", Typeflag: tar.TypeReg, Size: 5, Mode: 0644,
	})
	tw.Write([]byte("pwned"))
	tw.Close()

	layer, _ := tarball.LayerFromReader(&buf)
	extractLayer(layer, dir)

	// The file should NOT exist outside the target dir
	if _, err := os.Stat(filepath.Join(dir, "../../etc/passwd")); err == nil {
		t.Fatal("path traversal should have been blocked")
	}
}

func TestExtractLayerEmpty(t *testing.T) {
	dir := t.TempDir()
	var buf bytes.Buffer
	tar.NewWriter(&buf).Close()
	layer, _ := tarball.LayerFromReader(&buf)

	if err := extractLayer(layer, dir); err != nil {
		t.Fatal(err)
	}
}

// --- Inject tests ---

func TestInjectLohar(t *testing.T) {
	root := t.TempDir()

	// Create a fake lohar binary
	loharPath := filepath.Join(t.TempDir(), "lohar")
	os.WriteFile(loharPath, []byte("#!/bin/sh\necho lohar"), 0755)

	if err := injectLohar(root, loharPath); err != nil {
		t.Fatal(err)
	}

	// Check lohar exists
	if _, err := os.Stat(filepath.Join(root, "usr/local/bin/lohar")); err != nil {
		t.Fatal("lohar should exist")
	}

	// Check boot directories
	for _, d := range []string{"proc", "sys", "dev", "dev/pts", "tmp", "run", "workspace"} {
		if _, err := os.Stat(filepath.Join(root, d)); err != nil {
			t.Fatalf("boot dir %s should exist", d)
		}
	}

	// Check resolv.conf
	data, _ := os.ReadFile(filepath.Join(root, "etc/resolv.conf"))
	if !strings.Contains(string(data), "1.1.1.1") {
		t.Fatal("resolv.conf should contain nameserver")
	}
}

func TestEnsureUser1000Exists(t *testing.T) {
	root := t.TempDir()
	os.MkdirAll(filepath.Join(root, "etc"), 0755)
	os.WriteFile(filepath.Join(root, "etc/passwd"),
		[]byte("root:x:0:0::/root:/bin/bash\nnode:x:1000:1000::/home/node:/bin/bash\n"), 0644)

	ensureUser1000(root)

	// Should NOT add another entry — uid 1000 already exists as 'node'
	data, _ := os.ReadFile(filepath.Join(root, "etc/passwd"))
	if strings.Count(string(data), ":1000:") != 1 {
		t.Fatal("should not duplicate uid 1000 entry")
	}
}

func TestEnsureUser1000Missing(t *testing.T) {
	root := t.TempDir()
	os.MkdirAll(filepath.Join(root, "etc"), 0755)
	os.WriteFile(filepath.Join(root, "etc/passwd"),
		[]byte("root:x:0:0::/root:/bin/bash\n"), 0644)

	ensureUser1000(root)

	data, _ := os.ReadFile(filepath.Join(root, "etc/passwd"))
	if !strings.Contains(string(data), "lohar:x:1000:1000:") {
		t.Fatal("should have added lohar user")
	}
	if _, err := os.Stat(filepath.Join(root, "home/lohar")); err != nil {
		t.Fatal("should have created /home/lohar")
	}
}

func TestEnsureUser1000NoPasswd(t *testing.T) {
	root := t.TempDir()
	// No /etc/passwd at all (scratch image)
	if err := ensureUser1000(root); err != nil {
		t.Fatalf("should skip gracefully: %v", err)
	}
}

// --- Validate tests ---

func TestValidateImageClean(t *testing.T) {
	root := t.TempDir()
	os.MkdirAll(filepath.Join(root, "bin"), 0755)
	os.WriteFile(filepath.Join(root, "bin/sh"), []byte("sh"), 0755)
	os.MkdirAll(filepath.Join(root, "usr/bin"), 0755)
	os.WriteFile(filepath.Join(root, "usr/bin/sudo"), []byte("sudo"), 0755)

	w := validateImage(root)
	if len(w) != 0 {
		t.Fatalf("expected no warnings, got %v", w)
	}
}

func TestValidateImageNoShell(t *testing.T) {
	root := t.TempDir()
	w := validateImage(root)
	found := false
	for _, msg := range w {
		if strings.Contains(msg, "/bin/sh") {
			found = true
		}
	}
	if !found {
		t.Fatal("expected warning about missing shell")
	}
}

func TestValidateImageSystemd(t *testing.T) {
	root := t.TempDir()
	os.MkdirAll(filepath.Join(root, "lib/systemd"), 0755)
	os.WriteFile(filepath.Join(root, "lib/systemd/systemd"), []byte("systemd"), 0755)

	w := validateImage(root)
	found := false
	for _, msg := range w {
		if strings.Contains(msg, "systemd") {
			found = true
		}
	}
	if !found {
		t.Fatal("expected warning about systemd")
	}
}

// --- Merge tests ---

func TestMergeDirOverwrite(t *testing.T) {
	src := t.TempDir()
	dst := t.TempDir()

	// Create dst/file with old content
	os.WriteFile(filepath.Join(dst, "file"), []byte("old"), 0644)
	// Create src/file with new content
	os.WriteFile(filepath.Join(src, "file"), []byte("new"), 0644)

	mergeDir(src, dst)

	got := readFileContent(t, filepath.Join(dst, "file"))
	if got != "new" {
		t.Fatalf("expected 'new', got %q", got)
	}
}

func TestMergeDirPreservesExisting(t *testing.T) {
	src := t.TempDir()
	dst := t.TempDir()

	os.WriteFile(filepath.Join(dst, "keep"), []byte("keep"), 0644)
	os.MkdirAll(filepath.Join(src, "newdir"), 0755)
	os.WriteFile(filepath.Join(src, "newdir/new"), []byte("new"), 0644)

	mergeDir(src, dst)

	if readFileContent(t, filepath.Join(dst, "keep")) != "keep" {
		t.Fatal("existing file should be preserved")
	}
	if readFileContent(t, filepath.Join(dst, "newdir/new")) != "new" {
		t.Fatal("new file should be merged")
	}
}

// Silence the unused import warning
var _ = io.EOF

// --- ImportFromTarball tests ---

// makeTarball creates a docker-save-style tarball from layers.
// This builds a minimal but valid OCI image tarball that
// tarball.ImageFromPath can read.
func makeTarball(t *testing.T, layers []v1.Layer) string {
	t.Helper()
	// Build a v1.Image from the layers
	img, err := mutate.AppendLayers(empty.Image, layers...)
	if err != nil {
		t.Fatal(err)
	}
	// Write as a docker-save tarball
	path := filepath.Join(t.TempDir(), "image.tar")
	tag, _ := name.NewTag("test:latest")
	if err := tarball.WriteToFile(path, tag, img); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestImportFromTarball(t *testing.T) {
	// Create a layer with known content
	layer := makeLayer(t, []tarEntry{
		{name: "etc/", dir: true},
		{name: "etc/os-release", content: "NAME=TestOS"},
		{name: "bin/", dir: true},
		{name: "bin/sh", content: "#!/bin/sh\necho hello", mode: 0755},
		{name: "usr/", dir: true},
		{name: "usr/local/", dir: true},
		{name: "usr/local/bin/", dir: true},
	})

	tarPath := makeTarball(t, []v1.Layer{layer})

	// Create a fake lohar binary for injection
	loharDir := t.TempDir()
	loharPath := filepath.Join(loharDir, "lohar")
	os.WriteFile(loharPath, []byte("#!/bin/sh\necho lohar"), 0755)

	outputPath := filepath.Join(t.TempDir(), "output.ext4")

	ctx := t.Context()
	config, err := ImportFromTarball(ctx, tarPath, outputPath, loharPath)
	if err != nil {
		// mke2fs may not be available on macOS/CI — that's ok,
		// the extraction + injection still ran
		if strings.Contains(err.Error(), "mke2fs") {
			t.Skipf("mke2fs not available: %v", err)
		}
		t.Fatal(err)
	}

	if config == nil {
		t.Fatal("config should not be nil")
	}
	if config.TotalSize == 0 {
		t.Error("expected non-zero TotalSize")
	}
	t.Logf("imported: size=%d", config.TotalSize)
}

func TestImportPreservesConfig(t *testing.T) {
	layer := makeLayer(t, []tarEntry{
		{name: "bin/", dir: true},
		{name: "bin/sh", content: "shell", mode: 0755},
		{name: "usr/", dir: true},
		{name: "usr/local/", dir: true},
		{name: "usr/local/bin/", dir: true},
	})

	// Build image with config
	img, _ := mutate.AppendLayers(empty.Image, layer)
	cfg, _ := img.ConfigFile()
	cfg.Config.Env = []string{"FOO=bar", "PATH=/usr/bin"}
	cfg.Config.WorkingDir = "/workspace"
	cfg.Config.Cmd = []string{"/bin/sh"}
	img, _ = mutate.ConfigFile(img, cfg)

	path := filepath.Join(t.TempDir(), "image.tar")
	tag, _ := name.NewTag("test:latest")
	tarball.WriteToFile(path, tag, img)

	loharDir := t.TempDir()
	loharPath := filepath.Join(loharDir, "lohar")
	os.WriteFile(loharPath, []byte("fake"), 0755)

	outputPath := filepath.Join(t.TempDir(), "output.ext4")

	config, err := ImportFromTarball(t.Context(), path, outputPath, loharPath)
	if err != nil {
		if strings.Contains(err.Error(), "mke2fs") {
			t.Skipf("mke2fs not available: %v", err)
		}
		t.Fatal(err)
	}

	if config.Env["FOO"] != "bar" {
		t.Errorf("expected FOO=bar, got %v", config.Env)
	}
	if config.WorkingDir != "/workspace" {
		t.Errorf("expected /workspace, got %q", config.WorkingDir)
	}
	if len(config.Cmd) == 0 || config.Cmd[0] != "/bin/sh" {
		t.Errorf("expected [/bin/sh], got %v", config.Cmd)
	}
}
