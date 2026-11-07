package krucible

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A guest that never gets ready keeps the host probing for 30 s, and libkrun
// logs a line per probe: the lines that say how far the guest got (lohar's boot
// trace) were written first, and a plain tail of the log lost them all.
func TestVMMLogExcerptKeepsTheGuestsBootTraceUnderProbeNoise(t *testing.T) {
	var log strings.Builder
	log.WriteString("vmm: run vcpus=2 mem=512MiB root=/x/root.qcow2\n")
	log.WriteString("[2026-11-07T11:18:24.641752Z ERROR init_or_kernel] lohar: boot +0ms start\r\n")
	log.WriteString("[2026-11-07T11:18:24.654269Z ERROR init_or_kernel] lohar: config vsock dial: vsock connect (cid=2 port=1026): interrupted system call\r\n")
	log.WriteString("[2026-11-07T11:18:24.673027Z ERROR init_or_kernel] lohar: ready\r\n")
	for i := range 3000 {
		fmt.Fprintf(&log, "[2026-11-07T11:18:25.%06dZ WARN  krun_devices::virtio::vsock::muxer_thread] deferring proxy removal: %d\n", i, 4398046511104+int64(i)*7919)
	}
	path := filepath.Join(t.TempDir(), "vmm.log")
	if err := os.WriteFile(path, []byte(log.String()), 0o600); err != nil {
		t.Fatal(err)
	}

	got := vmmLogExcerpt(path, 4096)
	for _, want := range []string{"vmm: run", "boot +0ms start", "interrupted system call", "lohar: ready"} {
		if !strings.Contains(got, want) {
			t.Errorf("excerpt lost %q:\n%s", want, got)
		}
	}
	if n := strings.Count(got, "deferring proxy removal"); n != 1 {
		t.Errorf("the repeated probe line appears %d times, want once", n)
	}
	if len(got) > 4096 {
		t.Errorf("excerpt is %d bytes, over its limit", len(got))
	}
}

func TestVMMLogExcerptKeepsTheEndWhenTheLimitBites(t *testing.T) {
	var log strings.Builder
	for i := range 200 {
		fmt.Fprintf(&log, "line %c%c%c\n", 'a'+i%26, 'a'+(i/26)%26, 'a'+(i/676)%26)
	}
	log.WriteString("the last word\n")
	path := filepath.Join(t.TempDir(), "vmm.log")
	if err := os.WriteFile(path, []byte(log.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	got := vmmLogExcerpt(path, 100)
	if len(got) > 100 || !strings.HasSuffix(got, "the last word\n") {
		t.Fatalf("excerpt = %q, want at most 100 bytes ending in the log's last line", got)
	}
	if vmmLogExcerpt(filepath.Join(t.TempDir(), "absent"), 100) != "" {
		t.Fatal("a missing log produced an excerpt")
	}
}
