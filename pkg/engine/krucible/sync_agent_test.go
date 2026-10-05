package krucible

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sahil-shubham/bhatti/pkg/agent"
	"github.com/sahil-shubham/bhatti/pkg/agent/proto"
)

func TestLifecycleSyncUsesPlainCommandOnOldGuest(t *testing.T) {
	for _, tc := range []struct {
		name     string
		features []proto.AgentFeature
		cmd      string
		sync     bool
		exitCode int32
	}{
		{"old", []proto.AgentFeature{}, "sync", false, 0},
		{"new", []proto.AgentFeature{proto.FeatureExecSync}, "true", true, 0},
		{"failed", []proto.AgentFeature{}, "sync", false, 7},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir, err := os.MkdirTemp("/tmp", "bh-sync-")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { os.RemoveAll(dir) })
			path := filepath.Join(dir, "agent.sock")
			ln, err := net.Listen("unix", path)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { ln.Close() })
			frames := make(chan proto.ExecRequest, 1)
			go func() {
				for {
					conn, err := ln.Accept()
					if err != nil {
						return
					}
					go func() {
						defer conn.Close()
						conn.SetDeadline(time.Now().Add(2 * time.Second))
						typ, payload, err := proto.ReadFrame(conn)
						if err != nil {
							return
						}
						switch typ {
						case proto.INFO_REQ:
							data, _ := json.Marshal(proto.AgentInfo{Version: "v2.5.1", Features: tc.features})
							proto.WriteFrame(conn, proto.INFO_RESP, data)
						case proto.EXEC_REQ:
							var req proto.ExecRequest
							if json.Unmarshal(payload, &req) == nil {
								frames <- req
							}
							exit := proto.ExitPayload(tc.exitCode)
							proto.WriteFrame(conn, proto.EXIT, exit[:])
						}
					}()
				}
			}()
			vm := &VM{Agent: agent.NewTestClient(path, path), AgentInfo: proto.AgentInfo{Version: "v2.5.1", Features: tc.features}}
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			err = vm.syncGuest(ctx)
			if (err != nil) != (tc.exitCode != 0) {
				t.Fatalf("syncGuest error = %v; guest exit=%d", err, tc.exitCode)
			}
			select {
			case req := <-frames:
				if len(req.Argv) != 1 || req.Argv[0] != tc.cmd || (req.Sync != nil && *req.Sync) != tc.sync {
					t.Fatalf("guest sync request = %+v, want command %q, sync=%v", req, tc.cmd, tc.sync)
				}
			case <-ctx.Done():
				t.Fatal("guest sync never sent an exec request")
			}
		})
	}
}

func TestLifecycleSyncNeverTreatsUnknownCapabilitiesAsOld(t *testing.T) {
	for _, tc := range []struct {
		name string
		vm   *VM
	}{
		{"missing agent", &VM{}},
		{"failed INFO", &VM{Agent: agent.NewTestClient("/missing-agent.sock", ""), AgentInfoErr: errors.New("INFO timeout")}},
		{"missing INFO", &VM{Agent: agent.NewTestClient("/missing-agent.sock", "")}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.vm.syncGuest(context.Background()); err == nil || !strings.Contains(err.Error(), "unavailable") {
				t.Fatalf("unknown capability = %v; want explicit failure", err)
			}
		})
	}
}
