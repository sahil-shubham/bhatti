//go:build krucible

package krucible

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// writeBundle lays down a minimal snapshot bundle (manifest + the two payload
// files) for the portability-gate tests.
func writeBundle(t *testing.T, dir, manifest string, withPayload bool) {
	t.Helper()
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "manifest.json"), []byte(manifest), 0600); err != nil {
		t.Fatal(err)
	}
	if withPayload {
		for _, f := range []string{"checkpoint.bin", "memory.img"} {
			if err := os.WriteFile(filepath.Join(dir, f), []byte("payload"), 0600); err != nil {
				t.Fatal(err)
			}
		}
	}
}

// TestValidateBundle exercises bhatti's cold/move portability gate: it must
// accept a well-formed same-arch bundle and refuse incompatible ones (wrong
// proto version, cross-arch, incomplete, malformed) before any helper spawns.
func TestValidateBundle(t *testing.T) {
	arch := hostSnapshotArch()
	good := fmt.Sprintf(`{"proto_ver":%d,"arch":%q,"vcpu_count":1,"mem":[]}`, krucibleProtoVer, arch)

	t.Run("accepts well-formed same-arch bundle", func(t *testing.T) {
		dir := t.TempDir()
		writeBundle(t, dir, good, true)
		if err := validateBundle(dir); err != nil {
			t.Fatalf("validateBundle rejected a good bundle: %v", err)
		}
	})

	t.Run("refuses cross-arch", func(t *testing.T) {
		other := "x86_64"
		if arch == "x86_64" {
			other = "aarch64"
		}
		dir := t.TempDir()
		writeBundle(t, dir, fmt.Sprintf(`{"proto_ver":%d,"arch":%q,"vcpu_count":1}`, krucibleProtoVer, other), true)
		if err := validateBundle(dir); err == nil {
			t.Fatal("validateBundle accepted a cross-arch bundle")
		}
	})

	t.Run("refuses wrong proto_ver", func(t *testing.T) {
		dir := t.TempDir()
		writeBundle(t, dir, fmt.Sprintf(`{"proto_ver":%d,"arch":%q}`, krucibleProtoVer+1, arch), true)
		if err := validateBundle(dir); err == nil {
			t.Fatal("validateBundle accepted a future proto_ver")
		}
	})

	t.Run("refuses incomplete bundle (no checkpoint/memory)", func(t *testing.T) {
		dir := t.TempDir()
		writeBundle(t, dir, good, false)
		if err := validateBundle(dir); err == nil {
			t.Fatal("validateBundle accepted a manifest-only bundle")
		}
	})

	t.Run("refuses malformed manifest", func(t *testing.T) {
		dir := t.TempDir()
		writeBundle(t, dir, `{not json`, true)
		if err := validateBundle(dir); err == nil {
			t.Fatal("validateBundle accepted a malformed manifest")
		}
	})
}
