package agent

import (
	"bytes"
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/sahil-shubham/bhatti/pkg/agent/proto"
)

// erroringGuest answers any request with ERROR(reply), as lohar does when the
// os call behind a file read, stat or list fails.
func erroringGuest(t *testing.T, reply string) *AgentClient {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "bh-fr-") // macOS sockaddr_un length limit
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
				if _, _, err := proto.ReadFrame(conn); err != nil {
					return
				}
				proto.WriteFrame(conn, proto.ERROR, []byte(reply))
			}()
		}
	}()
	return NewTestClient(path, path)
}

func TestFileReadStatListTypeMissingPath(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	ops := map[string]func(*AgentClient) error{
		"read": func(c *AgentClient) error { _, _, err := c.FileRead(ctx, "/nope", &bytes.Buffer{}); return err },
		"stat": func(c *AgentClient) error { _, err := c.FileStat(ctx, "/nope"); return err },
		"list": func(c *AgentClient) error { _, err := c.FileList(ctx, "/nope"); return err },
	}
	for name, op := range ops {
		t.Run(name+"/missing", func(t *testing.T) {
			err := op(erroringGuest(t, "stat /nope: no such file or directory"))
			if !errors.Is(err, ErrFileNotFound) {
				t.Fatalf("err = %v; want ErrFileNotFound", err)
			}
			if got := err.Error(); got != "agent: stat /nope: no such file or directory" {
				t.Fatalf("message = %q; want the guest's text kept", got)
			}
		})
		t.Run(name+"/other", func(t *testing.T) {
			err := op(erroringGuest(t, "open /nope: permission denied"))
			if err == nil || errors.Is(err, ErrFileNotFound) {
				t.Fatalf("err = %v; want an untyped error", err)
			}
		})
	}
}
