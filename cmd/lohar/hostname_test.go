//go:build linux

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestWriteHostnameFiles verifies all hostname-shaped surfaces are written
// with the expected contents. Regression for #16: pre-fix lohar wrote
// /etc/hosts but not /etc/hostname or /etc/mailname, so the latter two
// retained whatever debootstrap baked in at image build time.
func TestWriteHostnameFiles(t *testing.T) {
	etcRoot := t.TempDir()

	writeHostnameFiles(etcRoot, "my-sandbox")

	cases := []struct {
		path string
		want string
	}{
		{"hostname", "my-sandbox\n"},
		{"hosts", "127.0.0.1 localhost my-sandbox\n::1 localhost my-sandbox\n"},
		{"mailname", "my-sandbox\n"},
	}
	for _, c := range cases {
		got, err := os.ReadFile(filepath.Join(etcRoot, c.path))
		if err != nil {
			t.Errorf("/etc/%s: %v", c.path, err)
			continue
		}
		if string(got) != c.want {
			t.Errorf("/etc/%s = %q, want %q", c.path, got, c.want)
		}
	}
}

// TestWriteHostnameFiles_Overwrites simulates the actual bug condition:
// the rootfs already has a leaked /etc/hostname (e.g. "runnervmeorf1"
// from the GHA build runner). A fresh boot must overwrite it cleanly.
func TestWriteHostnameFiles_Overwrites(t *testing.T) {
	etcRoot := t.TempDir()

	// Pre-seed leaked values, as the rootfs would arrive from a
	// debootstrap build whose host had a different hostname.
	for _, name := range []string{"hostname", "mailname"} {
		if err := os.WriteFile(filepath.Join(etcRoot, name),
			[]byte("runnervmeorf1\n"), 0644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(etcRoot, "hosts"),
		[]byte("127.0.0.1 localhost runnervmeorf1\n"), 0644); err != nil {
		t.Fatal(err)
	}

	writeHostnameFiles(etcRoot, "vm-0")

	// All three must now reflect the boot-time name, not the leak.
	for _, name := range []string{"hostname", "mailname", "hosts"} {
		got, err := os.ReadFile(filepath.Join(etcRoot, name))
		if err != nil {
			t.Fatalf("/etc/%s: %v", name, err)
		}
		if strings.Contains(string(got), "runnervmeorf1") {
			t.Errorf("/etc/%s still contains leaked hostname: %q", name, got)
		}
		if !strings.Contains(string(got), "vm-0") {
			t.Errorf("/etc/%s missing new hostname: %q", name, got)
		}
	}
}

// TestWriteHostnameFiles_SelfConsistent asserts the surfaces agree with
// each other. This is the test we should have had originally — it would
// have caught the missing /etc/hostname write the day it was introduced,
// regardless of the build host's specific hostname.
func TestWriteHostnameFiles_SelfConsistent(t *testing.T) {
	etcRoot := t.TempDir()
	const name = "consistent-test"

	writeHostnameFiles(etcRoot, name)

	hostnameFile := readTrim(t, filepath.Join(etcRoot, "hostname"))
	mailnameFile := readTrim(t, filepath.Join(etcRoot, "mailname"))
	hostsFile := readTrim(t, filepath.Join(etcRoot, "hosts"))

	if hostnameFile != name {
		t.Errorf("/etc/hostname = %q, want %q", hostnameFile, name)
	}
	if mailnameFile != name {
		t.Errorf("/etc/mailname = %q, want %q", mailnameFile, name)
	}
	if !strings.Contains(hostsFile, name) {
		t.Errorf("/etc/hosts missing hostname %q: %s", name, hostsFile)
	}
}

func readTrim(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return strings.TrimSpace(string(b))
}
