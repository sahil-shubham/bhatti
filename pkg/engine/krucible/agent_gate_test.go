package krucible

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sahil-shubham/bhatti/pkg/agent/proto"
	"github.com/sahil-shubham/bhatti/pkg/engine"
	"github.com/sahil-shubham/bhatti/pkg/gateway"
)

func TestForkRefusesOldLoharBeforeCheckpoint(t *testing.T) {
	dataDir := t.TempDir()
	vm := &VM{ID: "old", Status: "running", Thermal: "hot", netIP: "100.64.0.2", netdKey: "u:owner", AgentInfo: proto.AgentInfo{Legacy: true}}
	e := &Engine{cfg: Config{DataDir: dataDir}, caps: VMMCapabilities{Checkpoint: true}, vms: map[string]*VM{"old": vm}}
	_, err := e.Fork(context.Background(), "old", "copy")
	if !errors.Is(err, engine.ErrGuestAgentOutdated) || !strings.Contains(err.Error(), "net_config") {
		t.Fatalf("fork old lohar = %v; want network capability conflict", err)
	}
	entries, err := os.ReadDir(dataDir)
	if err != nil || len(entries) != 0 {
		t.Fatalf("fork touched snapshot directory before refusing: %v, %v", entries, err)
	}

	// A guest that understands the old IP-only protocol must not claim success
	// after silently ignoring the fork's new MAC.
	vm.AgentInfo = proto.AgentInfo{Version: "v2.4.0", Features: []proto.AgentFeature{"net_config"}}
	_, err = e.Fork(context.Background(), "old", "copy")
	if !errors.Is(err, engine.ErrGuestAgentOutdated) || !strings.Contains(err.Error(), "net_config_mac") {
		t.Fatalf("fork IP-only lohar = %v; want MAC capability conflict", err)
	}
	vm.AgentInfo = proto.AgentInfo{Version: "v2.5.0", Features: []proto.AgentFeature{proto.FeatureNetConfigMAC}}
	if err := e.RequireGuestAgentFeature(context.Background(), "old", proto.FeatureNetConfigMAC); err != nil {
		t.Fatalf("MAC-capable agent rejected: %v", err)
	}
	vm.AgentInfoErr = errors.New("info timed out")
	if err := e.RequireGuestAgentFeature(context.Background(), "old", proto.FeatureNetConfigMAC); err == nil || errors.Is(err, engine.ErrGuestAgentOutdated) {
		t.Fatalf("unknown capability mislabeled old: %v", err)
	}
}

// TestOldAgentVMMProcess is a fake helper, not a hypervisor. It accepts the
// auth/exec/info frames used by launch(), answering the unknown frame exactly
// as a pre-handshake lohar does. The caller owns its PID and cleanup.
func TestOldAgentVMMProcess(t *testing.T) {
	if os.Getenv("BHATTI_FAKE_OLD_LOHAR") != "1" {
		return
	}
	data, err := os.ReadFile(os.Args[len(os.Args)-1])
	if err != nil {
		t.Fatal(err)
	}
	var spec VMSpec
	if err := json.Unmarshal(data, &spec); err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("unix", spec.VsockControlUDS)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	for {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		go func() {
			defer conn.Close()
			conn.SetDeadline(time.Now().Add(3 * time.Second))
			typ, _, err := proto.ReadFrame(conn)
			if err != nil {
				return
			}
			if typ == proto.AUTH {
				typ, _, err = proto.ReadFrame(conn)
				if err != nil {
					return
				}
			}
			switch typ {
			case proto.EXEC_REQ:
				exit := proto.ExitPayload(0)
				proto.WriteFrame(conn, proto.EXIT, exit[:])
			case proto.INFO_REQ:
				proto.WriteFrame(conn, proto.ERROR, []byte("unexpected frame type 0x15"))
			default:
				proto.WriteFrame(conn, proto.ERROR, []byte(fmt.Sprintf("unexpected frame type 0x%02x", typ)))
			}
		}()
	}
}

func TestCreateRejectsOldLoharGrantAndRootGrowthWithoutVM(t *testing.T) {
	for _, tc := range []struct {
		name    string
		spec    engine.SandboxSpec
		feature string
	}{
		{"grant", engine.SandboxSpec{Name: "grant", RequireGuestCA: true, CACert: "fake CA"}, "sandbox_ca"},
		{"root growth", engine.SandboxSpec{Name: "larger", DiskSizeMB: 2}, "root_growth"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dataDir := t.TempDir()
			sockDir, err := os.MkdirTemp("/tmp", "bh-ag-")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { os.RemoveAll(sockDir) })
			base := filepath.Join(dataDir, "images", "bases", "rootfs-minimal-test.ext4")
			if err := os.MkdirAll(filepath.Dir(base), 0755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(base, make([]byte, 1<<20), 0644); err != nil {
				t.Fatal(err)
			}
			vmm := filepath.Join(dataDir, "fake-vmm")
			script := fmt.Sprintf("#!/bin/sh\nBHATTI_FAKE_OLD_LOHAR=1 exec %q -test.run=^TestOldAgentVMMProcess$ \"$1\"\n", os.Args[0])
			if err := os.WriteFile(vmm, []byte(script), 0755); err != nil {
				t.Fatal(err)
			}
			e := &Engine{cfg: Config{DataDir: dataDir, SocketDir: sockDir, BaseImage: base, VMMBinary: vmm, BlockRoot: true}, vms: map[string]*VM{}}
			t.Cleanup(func() {
				for id := range e.vms {
					_ = e.Destroy(context.Background(), id)
				}
			})
			tc.spec.NetPolicy = &gateway.NetPolicyWire{Default: gateway.PostureNone}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_, err = e.Create(ctx, tc.spec)
			if !errors.Is(err, engine.ErrGuestAgentOutdated) || !strings.Contains(err.Error(), tc.feature) {
				t.Fatalf("old agent create = %v; want outdated %s", err, tc.feature)
			}
			if got, err := os.ReadDir(filepath.Join(dataDir, "sandboxes")); err != nil || len(got) != 0 {
				t.Fatalf("failed create left sandbox dirs: %v, %v", got, err)
			}
			if len(e.vms) != 0 {
				t.Fatalf("failed create registered %d VMs", len(e.vms))
			}
		})
	}
}
