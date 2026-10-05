package krucible

import (
	"bufio"
	"bytes"
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

// TestFakeReseedVMMProcess is a protocol-speaking helper, not a hypervisor.
// The engine still has to complete the actual checkpoint/fork/restore paths.
func TestFakeReseedVMMProcess(t *testing.T) {
	if os.Getenv("BHATTI_FAKE_RESEED_VMM") != "1" {
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
	dir := filepath.Dir(os.Args[len(os.Args)-1])
	configJSON, err := os.ReadFile(filepath.Join(dir, "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	var cfg struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(configJSON, &cfg); err != nil {
		t.Fatal(err)
	}
	control, err := net.Listen("unix", spec.VsockControlUDS)
	if err != nil {
		t.Fatal(err)
	}
	defer control.Close()
	vmm, err := net.Listen("unix", spec.ControlSocketUDS)
	if err != nil {
		t.Fatal(err)
	}
	defer vmm.Close()
	go func() {
		for {
			conn, err := vmm.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				cmd, err := bufio.NewReader(conn).ReadString('\n')
				if err != nil {
					return
				}
				if strings.HasPrefix(cmd, "SAVE ") {
					save := strings.TrimSpace(strings.TrimPrefix(cmd, "SAVE "))
					if err := os.Mkdir(save, 0o700); err == nil {
						err = os.WriteFile(filepath.Join(save, checkpointFile), []byte("checkpoint"), 0o600)
					}
					if err == nil {
						err = os.WriteFile(filepath.Join(save, memoryFile), []byte("memory"), 0o600)
					}
					if err != nil {
						fmt.Fprintf(conn, "ERR %v\n", err)
						return
					}
				}
				fmt.Fprint(conn, "OK\n")
			}()
		}
	}()
	for {
		conn, err := control.Accept()
		if err != nil {
			return
		}
		go func() {
			defer conn.Close()
			conn.SetDeadline(time.Now().Add(5 * time.Second))
			typ, token, err := proto.ReadFrame(conn)
			if err != nil {
				return // WaitReady only opens and closes a connection
			}
			if typ != proto.AUTH || string(token) != cfg.Token {
				proto.WriteFrame(conn, proto.ERROR, []byte("auth required"))
				return
			}
			typ, payload, err := proto.ReadFrame(conn)
			if err != nil {
				return
			}
			switch typ {
			case proto.INFO_REQ:
				switch mode := os.Getenv("BHATTI_RESEED_MODE"); {
				case mode == "unknown" && spec.SnapshotDir != "":
					proto.WriteFrame(conn, proto.ERROR, []byte("capabilities unavailable"))
				case mode == "old":
					proto.SendJSON(conn, proto.INFO_RESP, proto.AgentInfo{Version: "v2.4.0", Features: []proto.AgentFeature{proto.FeatureNetConfig}})
				default:
					proto.SendJSON(conn, proto.INFO_RESP, proto.AgentInfo{Version: "v2.5.0", Features: []proto.AgentFeature{proto.FeatureNetConfig, proto.FeatureReseedCRNG}})
				}
			case proto.EXEC_REQ:
				exit := proto.ExitPayload(0)
				proto.WriteFrame(conn, proto.EXIT, exit[:])
			case proto.RESEED:
				if len(payload) != proto.ReseedBytes {
					proto.WriteFrame(conn, proto.ERROR, []byte("wrong entropy length"))
					return
				}
				if os.Getenv("BHATTI_RESEED_MODE") == "failed" {
					proto.WriteFrame(conn, proto.ERROR, []byte("RNDADDENTROPY: permission denied"))
					return
				}
				if err := os.WriteFile(filepath.Join(dir, "reseed.bin"), payload, 0o600); err != nil {
					proto.WriteFrame(conn, proto.ERROR, []byte(err.Error()))
					return
				}
				proto.WriteFrame(conn, proto.RESEED, nil)
			default:
				proto.WriteFrame(conn, proto.ERROR, []byte(fmt.Sprintf("unexpected frame type 0x%02x", typ)))
			}
		}()
	}
}

func fakeReseedEngine(t *testing.T, mode string) *Engine {
	t.Helper()
	dataDir := t.TempDir()
	vmm := filepath.Join(dataDir, "fake-vmm")
	script := fmt.Sprintf("#!/bin/sh\nBHATTI_FAKE_RESEED_VMM=1 BHATTI_RESEED_MODE=%s exec %q -test.run=^TestFakeReseedVMMProcess$ \"$1\"\n", mode, os.Args[0])
	if err := os.WriteFile(vmm, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	base := filepath.Join(dataDir, "base.ext4")
	if err := os.WriteFile(base, make([]byte, 1<<20), 0o600); err != nil {
		t.Fatal(err)
	}
	e := &Engine{
		cfg:  Config{DataDir: dataDir, SocketDir: shortSockDir(t), BaseImage: base, VMMBinary: vmm, BlockRoot: true, DefaultVcpus: 1, DefaultMemMiB: 64},
		caps: VMMCapabilities{Checkpoint: true},
		vms:  map[string]*VM{},
	}
	t.Cleanup(func() {
		for id := range e.vms {
			_ = e.Destroy(context.Background(), id)
		}
	})
	return e
}

func noNetworkSpec(name string) engine.SandboxSpec {
	return engine.SandboxSpec{Name: name, NetPolicy: &gateway.NetPolicyWire{Default: gateway.PostureNone}}
}

func reseedFromVM(t *testing.T, e *Engine, id string) []byte {
	t.Helper()
	vm, err := e.getVM(id)
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(vm.SandboxDir, "reseed.bin"))
	if err != nil || len(data) != proto.ReseedBytes {
		t.Fatalf("sandbox %s returned before receiving %d entropy bytes: %v, %d bytes", id, proto.ReseedBytes, err, len(data))
	}
	return data
}

func TestForkReseedsEachCloneBeforeReturning(t *testing.T) {
	e := fakeReseedEngine(t, "supported")
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	source, err := e.Create(ctx, noNetworkSpec("original"))
	if err != nil {
		t.Fatal(err)
	}
	first, err := e.Fork(ctx, source.ID, "first")
	if err != nil {
		t.Fatal(err)
	}
	second, err := e.Fork(ctx, source.ID, "second")
	if err != nil {
		t.Fatal(err)
	}
	for _, clone := range []engine.SandboxInfo{first, second} {
		if clone.Status != "running" || clone.GuestReseedUnsupported {
			t.Fatalf("fork returned without a reseeded running clone: %+v", clone)
		}
	}
	if a, b := reseedFromVM(t, e, first.ID), reseedFromVM(t, e, second.ID); bytes.Equal(a, b) || bytes.Equal(a, make([]byte, proto.ReseedBytes)) || bytes.Equal(b, make([]byte, proto.ReseedBytes)) {
		t.Fatal("two forks of one memory image received the same or empty entropy")
	}
	if _, err := os.Stat(filepath.Join(e.vms[source.ID].SandboxDir, "reseed.bin")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("original VM was unnecessarily reseeded: %v", err)
	}
}

func TestRestoredParentAndChildBothReseed(t *testing.T) {
	e := fakeReseedEngine(t, "supported")
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	source, err := e.Create(ctx, noNetworkSpec("original"))
	if err != nil {
		t.Fatal(err)
	}
	snapDir := t.TempDir()
	manifest, err := e.Checkpoint(ctx, source.ID, "", 0, "memory", snapDir)
	if err != nil {
		t.Fatal(err)
	}
	jsonManifest, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	parent, err := e.ResumeFromManifestJSON(ctx, filepath.Join(snapDir, "memory"), jsonManifest, "restored-parent", "")
	if err != nil {
		t.Fatal(err)
	}
	parentSeed := reseedFromVM(t, e, parent.ID)
	child, err := e.Fork(ctx, parent.ID, "restored-child")
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(parentSeed, reseedFromVM(t, e, child.ID)) {
		t.Fatal("parent restored from memory and its child received the same host entropy")
	}
}

func TestRestoreOldAgentWarnsButReseedFailureRefuses(t *testing.T) {
	for _, tc := range []struct {
		mode        string
		wantFailure bool
	}{
		{"old", false}, {"failed", true}, {"unknown", true},
	} {
		t.Run(tc.mode, func(t *testing.T) {
			e := fakeReseedEngine(t, tc.mode)
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			source, err := e.Create(ctx, noNetworkSpec("original"))
			if err != nil {
				t.Fatal(err)
			}
			child, err := e.Fork(ctx, source.ID, "copy")
			if tc.wantFailure {
				if !errors.Is(err, engine.ErrGuestReseedFailed) {
					t.Fatalf("%s: expected fail-closed guest reseed, got %v", tc.mode, err)
				}
				if len(e.vms) != 1 {
					t.Fatalf("failed restore registered a clone: %d VMs", len(e.vms))
				}
				return
			}
			if err != nil || !child.GuestReseedUnsupported {
				t.Fatalf("old guest restore: info=%+v err=%v; expected explicit warning status", child, err)
			}
			vm, err := e.getVM(child.ID)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(filepath.Join(vm.SandboxDir, "reseed.bin")); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("old agent was sent an unsupported reseed: %v", err)
			}
		})
	}
}
