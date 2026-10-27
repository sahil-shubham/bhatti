package agent

import (
	"context"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sahil-shubham/bhatti/pkg/agent/proto"
)

// abortingGuest reads the FILE_WRITE_REQ, optionally answers ERROR(reply), and
// closes without reading the body, as lohar does when it refuses a write.
func abortingGuest(t *testing.T, reply string) *AgentClient {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "bh-fw-") // macOS sockaddr_un length limit
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	path := filepath.Join(dir, "control.sock")
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				conn.SetDeadline(time.Now().Add(5 * time.Second))
				if typ, _, err := proto.ReadFrame(conn); err != nil || typ != proto.FILE_WRITE_REQ {
					return
				}
				if reply != "" {
					proto.WriteFrame(conn, proto.ERROR, []byte(reply))
				}
			}()
		}
	}()
	return NewTestClient(path, path)
}

// zeros is an endless body: the host only stops when the connection breaks.
type zeros struct{}

func (zeros) Read(p []byte) (int, error) { clear(p); return len(p), nil }

func TestFileWriteSurfacesGuestRefusal(t *testing.T) {
	const noSpace = "no space: need 198180864 bytes (189.0M), 1288490188 bytes (1.2G) available"
	const tooLarge = "file too large: 198180864 bytes (max 104857600)"
	for _, tc := range []struct {
		name  string
		reply string
		body  func() io.Reader
		want  error
	}{
		// A big body keeps the host writing after the guest closed: broken pipe.
		{"no space mid-upload", noSpace, func() io.Reader { return io.LimitReader(zeros{}, 1<<30) }, ErrNoSpace},
		{"old agent cap mid-upload", tooLarge, func() io.Reader { return io.LimitReader(zeros{}, 1<<30) }, ErrFileTooLarge},
		// A body that fits the socket buffer is written fully; the reply is read normally.
		{"no space small body", noSpace, func() io.Reader { return strings.NewReader("tiny") }, ErrNoSpace},
		{"old agent cap small body", tooLarge, func() io.Reader { return strings.NewReader("tiny") }, ErrFileTooLarge},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			err := abortingGuest(t, tc.reply).FileWrite(ctx, "/workspace/big.bin", "0644", 198180864, tc.body())
			if !errors.Is(err, tc.want) {
				t.Fatalf("FileWrite = %v; want %v", err, tc.want)
			}
			if err.Error() != tc.reply {
				t.Fatalf("message = %q; want the guest's %q", err, tc.reply)
			}
		})
	}
}

func TestFileWriteBrokenPipeWithoutReplyKeepsWriteError(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	err := abortingGuest(t, "").FileWrite(ctx, "/workspace/big.bin", "0644", 1<<30, io.LimitReader(zeros{}, 1<<30))
	if err == nil || errors.Is(err, ErrNoSpace) || errors.Is(err, ErrFileTooLarge) || !strings.Contains(err.Error(), "write stdin frame") {
		t.Fatalf("FileWrite = %v; want the original write error", err)
	}
}

func TestFileWriteOtherGuestErrorStaysUntyped(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	err := abortingGuest(t, "permission denied").FileWrite(ctx, "/x", "0644", 1<<30, io.LimitReader(zeros{}, 1<<30))
	if err == nil || errors.Is(err, ErrNoSpace) || errors.Is(err, ErrFileTooLarge) || !strings.Contains(err.Error(), "permission denied") {
		t.Fatalf("FileWrite = %v; want untyped agent error carrying the message", err)
	}
}
