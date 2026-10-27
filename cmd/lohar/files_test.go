//go:build linux

package main

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/sahil-shubham/bhatti/pkg/agent/proto"
)

// fakeFreeSpace makes handleFileWrite see `avail` free bytes on any filesystem.
func fakeFreeSpace(t *testing.T, avail uint64) {
	t.Helper()
	old := statfsFn
	statfsFn = func(_ string, st *syscall.Statfs_t) error {
		st.Bsize = 4096
		st.Bavail = avail / 4096
		return nil
	}
	t.Cleanup(func() { statfsFn = old })
}

// startFileWrite runs handleFileWrite against one end of a pipe.
func startFileWrite(t *testing.T, path string, size int64) (net.Conn, <-chan struct{}) {
	t.Helper()
	host, guest := net.Pipe()
	host.SetDeadline(time.Now().Add(30 * time.Second)) // a refusing handler must fail the test, not hang it
	t.Cleanup(func() { host.Close() })
	req, _ := json.Marshal(map[string]any{"path": path, "mode": "0644", "size": size})
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer guest.Close()
		handleFileWrite(guest, req)
	}()
	return host, done
}

func readReply(t *testing.T, c net.Conn) (byte, string) {
	t.Helper()
	c.SetReadDeadline(time.Now().Add(10 * time.Second))
	typ, payload, err := proto.ReadFrame(c)
	if err != nil {
		t.Fatalf("read reply: %v", err)
	}
	return typ, string(payload)
}

// The old fixed 100 MB cap rejected CI-sized uploads even with plenty of disk.
func TestFileWriteAboveFormerCap(t *testing.T) {
	fakeFreeSpace(t, 1<<40)
	const size = 120 << 20
	target := filepath.Join(t.TempDir(), "big.bin")
	host, done := startFileWrite(t, target, size)

	chunk := make([]byte, 512<<10)
	for sent := 0; sent < size; sent += len(chunk) {
		if err := proto.WriteFrame(host, proto.STDIN, chunk); err != nil {
			t.Fatalf("write at %d: %v", sent, err)
		}
	}
	if typ, msg := readReply(t, host); typ != proto.FILE_WRITE_RESP {
		t.Fatalf("reply = 0x%02x %q; want FILE_WRITE_RESP", typ, msg)
	}
	<-done
	if st, err := os.Stat(target); err != nil || st.Size() != size {
		t.Fatalf("stat %s = %v, %v; want %d bytes", target, st, err, size)
	}
}

// A declared size that cannot fit is refused before any body is sent, so the
// host gets a reason instead of a broken pipe mid-upload.
func TestFileWriteNoSpaceRefusedBeforeBody(t *testing.T) {
	fakeFreeSpace(t, 1200<<20)
	target := filepath.Join(t.TempDir(), "big.bin")
	host, done := startFileWrite(t, target, 2<<30)

	typ, msg := readReply(t, host) // nothing was sent after the request
	if typ != proto.ERROR || !strings.HasPrefix(msg, proto.ErrPrefixNoSpace) {
		t.Fatalf("reply = 0x%02x %q; want ERROR with %q prefix", typ, msg, proto.ErrPrefixNoSpace)
	}
	for _, want := range []string{fmt.Sprint(uint64(2<<30 + writeSpaceReserve)), fmt.Sprint(uint64(1200 << 20))} {
		if !strings.Contains(msg, want) {
			t.Errorf("message %q lacks byte count %s", msg, want)
		}
	}
	<-done
	if _, err := os.Stat(target + ".bhatti-tmp"); !os.IsNotExist(err) {
		t.Errorf("temp file left behind: %v", err)
	}
}

// The reserve exists for bulk writes; a small config file must still land on
// a nearly-full disk as it did before the check existed.
func TestFileWriteSmallFitsInsideReserve(t *testing.T) {
	fakeFreeSpace(t, 1<<20)
	target := filepath.Join(t.TempDir(), "small.txt")
	host, done := startFileWrite(t, target, 5)
	if err := proto.WriteFrame(host, proto.STDIN, []byte("hello")); err != nil {
		t.Fatal(err)
	}
	if typ, msg := readReply(t, host); typ != proto.FILE_WRITE_RESP {
		t.Fatalf("reply = 0x%02x %q; want FILE_WRITE_RESP", typ, msg)
	}
	<-done
}

func TestDescribeWriteErrTagsENOSPC(t *testing.T) {
	err := describeWriteErr(&os.PathError{Op: "write", Path: "/x", Err: syscall.ENOSPC}, 10, 100)
	if !strings.HasPrefix(err.Error(), proto.ErrPrefixNoSpace) {
		t.Fatalf("ENOSPC error = %q; want %q prefix", err, proto.ErrPrefixNoSpace)
	}
	other := &os.PathError{Op: "write", Path: "/x", Err: syscall.EIO}
	if got := describeWriteErr(other, 10, 100); got != error(other) {
		t.Fatalf("EIO was rewritten: %v", got)
	}
}
