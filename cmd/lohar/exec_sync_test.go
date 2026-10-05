//go:build linux

package main

import (
	"net"
	"os"
	"testing"

	"github.com/sahil-shubham/bhatti/pkg/agent/proto"
)

func TestPipedExecOnlySyncsBeforeOptInExit(t *testing.T) {
	// handlePipedExec intentionally runs commands as uid 1000, so an
	// unprivileged host cannot exercise the real guest command path.
	if os.Geteuid() != 0 {
		t.Skip("requires root to spawn the guest uid 1000 command")
	}
	original := execSync
	defer func() { execSync = original }()
	for _, requested := range []bool{false, true} {
		host, guest := net.Pipe()
		synced := make(chan struct{}, 1)
		execSync = func() { synced <- struct{}{} }
		req := proto.ExecRequest{Argv: []string{"/bin/true"}}
		if requested {
			req.Sync = &requested
		}
		go func() {
			defer guest.Close()
			handlePipedExec(guest, req)
		}()
		for {
			msgType, payload, err := proto.ReadFrame(host)
			if err != nil {
				t.Fatalf("read exec exit: %v", err)
			}
			if msgType == proto.ERROR {
				t.Fatalf("guest exec rejected: %s", payload)
			}
			if msgType == proto.EXIT {
				if code, ok := proto.ParseExitCode(payload); !ok || code != 0 {
					t.Fatalf("guest exec exit: %d, valid=%v", code, ok)
				}
				if (len(synced) == 1) != requested {
					t.Fatalf("sync before EXIT = %v; requested = %v", len(synced) == 1, requested)
				}
				break
			}
		}
		host.Close()
	}
}
