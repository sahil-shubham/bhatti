package krucible

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
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
