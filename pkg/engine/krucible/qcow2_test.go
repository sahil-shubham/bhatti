package krucible

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestQcow2OverlayCopyOnWrite checks the overlay against qemu's own tools (an
// implementation independent of ours): it must pass `qemu-img check`, report
// the raw backing file and the base's virtual size, read through to the base,
// and absorb writes without touching the base.
func TestQcow2OverlayCopyOnWrite(t *testing.T) {
	for _, tool := range []string{"qemu-img", "qemu-io"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s not installed", tool)
		}
	}
	dir := t.TempDir()
	base := filepath.Join(dir, "base.raw")
	ovl := filepath.Join(dir, "root.qcow2")
	// 1 GiB + 1 byte: spans three L2 ranges, so the L1 has more than one entry,
	// and isn't sector-aligned, so it must be rounded up rather than truncated.
	const size = 1<<30 + 1
	const wantSize = 1<<30 + 512

	f, err := os.Create(base)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(size); err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteAt(bytes.Repeat([]byte{0x5a}, 4096), 1<<29); err != nil {
		t.Fatal(err)
	}
	f.Close()

	if err := createQcow2Overlay(ovl, base, size); err != nil {
		t.Fatal(err)
	}
	run := func(name string, args ...string) string {
		t.Helper()
		out, err := exec.Command(name, args...).CombinedOutput()
		if err != nil {
			t.Fatalf("%s %v: %v\n%s", name, args, err, out)
		}
		return string(out)
	}

	run("qemu-img", "check", "-f", "qcow2", ovl)
	var info struct {
		VirtualSize   uint64 `json:"virtual-size"`
		BackingFile   string `json:"backing-filename"`
		BackingFormat string `json:"backing-filename-format"`
	}
	if err := json.Unmarshal([]byte(run("qemu-img", "info", "-f", "qcow2", "--output=json", ovl)), &info); err != nil {
		t.Fatal(err)
	}
	if info.VirtualSize != wantSize || info.BackingFile != base || info.BackingFormat != "raw" {
		t.Fatalf("qemu-img info = %+v, want size %d backing %s (raw)", info, uint64(wantSize), base)
	}

	// Reads fall through to the base; writes land in the overlay only.
	run("qemu-io", "-f", "qcow2", "-c", "read -P 0x5a 512M 4k", ovl)
	run("qemu-io", "-f", "qcow2", "-c", "write -P 0xc3 512M 4k", ovl)
	run("qemu-io", "-f", "qcow2", "-c", "read -P 0xc3 512M 4k", ovl)
	run("qemu-io", "-f", "raw", "-c", "read -P 0x5a 512M 4k", base)
	run("qemu-img", "check", "-f", "qcow2", ovl) // refcounts still consistent after allocation
}

// TestQcow2BackingRewrite moves an overlay onto a copy of its base elsewhere,
// as an imported snapshot's root disk is, and checks it is the same disk: the
// header names the new base (longer, then shorter, with nothing of the old
// name left), a name the header can't hold is refused without touching the
// image, and qemu (when installed) reads the overlay's writes and the base's
// data through it.
func TestQcow2BackingRewrite(t *testing.T) {
	dir := t.TempDir()
	base := filepath.Join(dir, "base.raw")
	data := bytes.Repeat([]byte{0x5a}, 1<<20)
	if err := os.WriteFile(base, data, 0o644); err != nil {
		t.Fatal(err)
	}
	ovl := filepath.Join(dir, "root.qcow2")
	if err := createQcow2Overlay(ovl, base, uint64(len(data))); err != nil {
		t.Fatal(err)
	}
	qemu := true
	for _, tool := range []string{"qemu-img", "qemu-io"} {
		if _, err := exec.LookPath(tool); err != nil {
			qemu = false
		}
	}
	run := func(name string, args ...string) {
		t.Helper()
		if out, err := exec.Command(name, args...).CombinedOutput(); err != nil {
			t.Fatalf("%s %v: %v\n%s", name, args, err, out)
		}
	}
	if qemu {
		run("qemu-io", "-f", "qcow2", "-c", "write -P 0xc3 0 4k", ovl)
	}

	moved := filepath.Join(dir, "a much longer directory", "where the base image moved.ext4")
	short := filepath.Join(dir, "b")
	for _, to := range []string{moved, short} {
		if err := os.MkdirAll(filepath.Dir(to), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(to, data, 0o644); err != nil {
			t.Fatal(err)
		}
		if err := setQcow2Backing(ovl, to); err != nil {
			t.Fatal(err)
		}
		if got, err := qcow2Backing(ovl); err != nil || got != to {
			t.Fatalf("backing = %q, %v; want %q", got, err, to)
		}
		if qemu {
			run("qemu-img", "check", "-f", "qcow2", ovl)
			run("qemu-io", "-f", "qcow2", "-c", "read -P 0xc3 0 4k", "-c", "read -P 0x5a 4k 4k", ovl)
		}
	}
	// The old, longer name's tail is cleared.
	hdr := make([]byte, qcow2ClusterSize)
	f, err := os.Open(ovl)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.ReadAt(hdr, 0); err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(hdr, []byte("where the base image moved")) {
		t.Fatal("the header still holds part of the previous backing name")
	}

	before, _ := os.ReadFile(ovl)
	if err := setQcow2Backing(ovl, "/"+strings.Repeat("x", qcow2MaxBackingName)); err == nil {
		t.Fatal("a backing name longer than qcow2 allows was written")
	}
	if after, _ := os.ReadFile(ovl); !bytes.Equal(before, after) {
		t.Fatal("a refused rewrite changed the image")
	}
	if _, err := qcow2Backing(base); err == nil {
		t.Fatal("a raw image read as qcow2")
	}
}

// TestQcow2BackingRewriteQemu re-points overlays and has qemu-img (an
// implementation independent of ours) report the backing file: one qemu-img
// created (its own header layout and extensions), then many times over with
// long names, so the name runs out of room after the old one and goes back
// to the start of the header cluster's free space. Every step leaves an image
// qemu-img checks clean and that names exactly the new base.
func TestQcow2BackingRewriteQemu(t *testing.T) {
	if _, err := exec.LookPath("qemu-img"); err != nil {
		t.Skip("qemu-img not installed")
	}
	dir := t.TempDir()
	data := bytes.Repeat([]byte{0x5a}, 1<<20)
	base := filepath.Join(dir, "base.raw")
	if err := os.WriteFile(base, data, 0o644); err != nil {
		t.Fatal(err)
	}
	run := func(args ...string) string {
		t.Helper()
		out, err := exec.Command("qemu-img", args...).CombinedOutput()
		if err != nil {
			t.Fatalf("qemu-img %v: %v\n%s", args, err, out)
		}
		return string(out)
	}
	backingSeen := func(ovl string) string {
		t.Helper()
		var info struct {
			BackingFile   string `json:"backing-filename"`
			BackingFormat string `json:"backing-filename-format"`
		}
		if err := json.Unmarshal([]byte(run("info", "-f", "qcow2", "--output=json", ovl)), &info); err != nil {
			t.Fatal(err)
		}
		if info.BackingFormat != "raw" {
			t.Fatalf("backing format %q after a rewrite, want raw", info.BackingFormat)
		}
		return info.BackingFile
	}
	byQemu := filepath.Join(dir, "qemu.qcow2")
	run("create", "-q", "-f", "qcow2", "-F", "raw", "-b", base, byQemu)
	byUs := filepath.Join(dir, "ours.qcow2")
	if err := createQcow2Overlay(byUs, base, uint64(len(data))); err != nil {
		t.Fatal(err)
	}

	// Names of ~800 bytes (path elements stay under NAME_MAX), each a hard
	// link to the base: the same bytes under another name.
	deep := filepath.Join(dir, strings.Repeat("a", 200), strings.Repeat("b", 200), strings.Repeat("c", 200))
	if err := os.MkdirAll(deep, 0o755); err != nil {
		t.Fatal(err)
	}
	long := func(ovl string, i int) string {
		p := filepath.Join(deep, fmt.Sprintf("%s-%03d.raw", filepath.Base(ovl), i))
		if err := os.Link(base, p); err != nil {
			t.Fatal(err)
		}
		return p
	}
	for _, ovl := range []string{byQemu, byUs} {
		// 100 rewrites of ~800 bytes go past the end of a 64 KiB header
		// cluster, so the name wraps back to the start of the free space.
		for i := range 100 {
			to := long(ovl, i)
			if err := setQcow2Backing(ovl, to); err != nil {
				t.Fatalf("%s rewrite %d: %v", filepath.Base(ovl), i, err)
			}
			if got := backingSeen(ovl); got != to {
				t.Fatalf("%s rewrite %d: qemu-img sees backing %q, want %q", filepath.Base(ovl), i, got, to)
			}
			run("check", "-q", "-f", "qcow2", ovl)
		}
		short := filepath.Join(dir, "b.raw")
		os.Remove(short)
		if err := os.Link(base, short); err != nil {
			t.Fatal(err)
		}
		if err := setQcow2Backing(ovl, short); err != nil {
			t.Fatal(err)
		}
		if got := backingSeen(ovl); got != short {
			t.Fatalf("qemu-img sees backing %q, want %q", got, short)
		}
	}
}
