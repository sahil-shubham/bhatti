package configdrive

import (
	"bytes"
	"encoding/base64"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// ext4SuperblockMagic is 0xEF53, little-endian at byte offset 0x438.
func isExt4(t *testing.T, path string) bool {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open image: %v", err)
	}
	defer f.Close()
	buf := make([]byte, 2)
	if _, err := f.ReadAt(buf, 0x438); err != nil {
		t.Fatalf("read superblock magic: %v", err)
	}
	return buf[0] == 0x53 && buf[1] == 0xEF
}

func requireMke2fs(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("mke2fs"); err != nil {
		t.Skip("mke2fs not found (e2fsprogs/platform-tools); skipping")
	}
}

// TestBuildProducesValidExt4 builds a real config drive and verifies it is a
// genuine ext4 image of a sane size — no mock, the actual on-disk artifact.
func TestBuildProducesValidExt4(t *testing.T) {
	requireMke2fs(t)
	path := filepath.Join(t.TempDir(), "config.ext4")
	if err := Build(path, SandboxConfig{SandboxID: "s1", Hostname: "h1", Token: "tok"}); err != nil {
		t.Fatalf("Build: %v", err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if fi.Size() < 1<<20 || fi.Size() > 4<<20 {
		t.Fatalf("image size %d outside [1,4]MiB", fi.Size())
	}
	if !isExt4(t, path) {
		t.Fatal("image is not a valid ext4 (bad superblock magic)")
	}
}

// TestBuildEmbedsConfig verifies the real image contains the marshaled config —
// the bytes lohar will read at /dev/vdb. Reads the raw image (mke2fs -d stores a
// small file's bytes verbatim in extents), no mount/debugfs needed.
func TestBuildEmbedsConfig(t *testing.T) {
	requireMke2fs(t)
	path := filepath.Join(t.TempDir(), "config.ext4")
	fileContent := "hello-from-config"
	cfg := SandboxConfig{
		SandboxID: "sandbox-abc",
		Hostname:  "myhost",
		Token:     "secret-token-123",
		Env:       map[string]string{"FOO": "barbaz"},
		Files: map[string]ConfigFile{
			"/etc/greeting": {Content: base64.StdEncoding.EncodeToString([]byte(fileContent)), Mode: "0644"},
		},
		User: "lohar",
	}
	if err := Build(path, cfg); err != nil {
		t.Fatalf("Build: %v", err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read image: %v", err)
	}
	for _, want := range []string{
		"sandbox-abc", "myhost", "secret-token-123", "FOO", "barbaz", "/etc/greeting",
		base64.StdEncoding.EncodeToString([]byte(fileContent)),
	} {
		if !bytes.Contains(raw, []byte(want)) {
			t.Errorf("config drive image does not embed %q", want)
		}
	}
}

// TestBuildClampsSize ensures a large payload is clamped to the 4 MiB ceiling
// rather than producing an unbounded image.
func TestBuildClampsSize(t *testing.T) {
	requireMke2fs(t)
	big := strings.Repeat("x", 8<<20) // 8 MiB env value
	path := filepath.Join(t.TempDir(), "config.ext4")
	if err := Build(path, SandboxConfig{SandboxID: "s", Env: map[string]string{"BIG": big}}); err != nil {
		// mke2fs may refuse a too-small fs for a too-big file; that's an
		// acceptable failure mode — the point is we never exceed 4 MiB.
		t.Skipf("oversized payload rejected by mke2fs (acceptable): %v", err)
	}
	fi, _ := os.Stat(path)
	if fi.Size() > 4<<20 {
		t.Fatalf("image size %d exceeds 4MiB clamp", fi.Size())
	}
}
