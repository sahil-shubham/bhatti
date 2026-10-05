package krucible

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sahil-shubham/bhatti/pkg/agent"
	"github.com/sahil-shubham/bhatti/pkg/agent/proto"
)

// durabilityPeer is a guest and VMM control socket with observable wire
// operations. It checks both guest syncs surrounding a warm pause and stop.
func durabilityPeer(t *testing.T, exit int32) (agentSock, controlSock string, operations chan string) {
	t.Helper()
	dir := shortSockDir(t)
	agentSock = filepath.Join(dir, "a.sock")
	controlSock = filepath.Join(dir, "k.sock")
	operations = make(chan string, 16)
	for _, target := range []string{agentSock, controlSock} {
		ln, err := net.Listen("unix", target)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { ln.Close() })
		go func() {
			for {
				c, err := ln.Accept()
				if err != nil {
					return
				}
				go func() {
					defer c.Close()
					if target == controlSock {
						var line string
						_, _ = fmt.Fscanln(c, &line)
						operations <- line
						fmt.Fprintln(c, "OK")
						return
					}
					typ, payload, err := proto.ReadFrame(c)
					if err != nil {
						return
					}
					if typ == proto.AUTH {
						typ, payload, err = proto.ReadFrame(c)
						if err != nil {
							return
						}
					}
					switch typ {
					case proto.INFO_REQ:
						_ = proto.SendJSON(c, proto.INFO_RESP, proto.AgentInfo{Version: "v2.5.1", Features: []proto.AgentFeature{proto.FeatureExecSync}})
					case proto.EXEC_REQ:
						var req proto.ExecRequest
						if json.Unmarshal(payload, &req) != nil {
							return
						}
						if req.Sync != nil && *req.Sync && slices.Equal(req.Argv, []string{"true"}) {
							operations <- "sync"
						} else if req.Sync == nil && slices.Equal(req.Argv, []string{"sync"}) {
							operations <- "legacy-sync"
						} else {
							operations <- "unexpected-exec"
						}
						result := proto.ExitPayload(exit)
						_ = proto.WriteFrame(c, proto.EXIT, result[:])
					}
				}()
			}
		}()
	}
	return agentSock, controlSock, operations
}

func durabilityEngine(t *testing.T, thermal, agSock, ctlSock string) (*Engine, *VM) {
	t.Helper()
	cmd := exec.Command("sleep", "30")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	t.Cleanup(func() { _ = cmd.Process.Kill() })
	vm := &VM{
		ID: "durable", SandboxDir: t.TempDir(), CtlSockUDS: ctlSock,
		Status: "running", Thermal: thermal,
		Agent: agent.NewKrucibleClient(agSock, "", ""),
		cmd:   cmd, waitDone: done, HelperPID: cmd.Process.Pid,
	}
	e := &Engine{caps: VMMCapabilities{Pause: true}, vms: map[string]*VM{vm.ID: vm}}
	return e, vm
}

func readOperations(ch <-chan string) []string {
	var ops []string
	for {
		select {
		case op := <-ch:
			ops = append(ops, op)
		default:
			return ops
		}
	}
}

func TestPauseSyncBeforeParkAndWarmStopResyncs(t *testing.T) {
	agentSock, ctlSock, ops := durabilityPeer(t, 0)
	e, vm := durabilityEngine(t, "hot", agentSock, ctlSock)
	info, err := vm.Agent.Info(context.Background()) // caches feature for ExecWithSync
	if err != nil {
		t.Fatal(err)
	}
	vm.AgentInfo = info
	if err := e.Pause(context.Background(), vm.ID); err != nil {
		t.Fatal(err)
	}
	if rec := readRecord(t, vm.SandboxDir); rec.Thermal != "warm" {
		t.Fatalf("PAUSE did not persist warm VM state: %+v", rec)
	}
	if got := readOperations(ops); !slices.Equal(got, []string{"sync", "PAUSE"}) {
		t.Fatalf("guest sync must precede PAUSE, got %v", got)
	}
	// A writer can dirty the guest between sync completion and PAUSE.
	// A warm stop must therefore RESUME and issue a fresh sync.
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := e.Stop(ctx, vm.ID); err != nil {
		t.Fatal(err)
	}
	if got := readOperations(ops); !slices.Equal(got, []string{"RESUME", "sync"}) {
		t.Fatalf("warm power-off skipped its final sync: %v", got)
	}
	if rec := readRecord(t, vm.SandboxDir); rec.Status != "stopped" {
		t.Fatalf("power-off state not persisted correctly: %+v", rec)
	}
}

func TestLegacyPauseSyncDoesNotSendNewExecField(t *testing.T) {
	agentSock, ctlSock, ops := durabilityPeer(t, 0)
	e, vm := durabilityEngine(t, "hot", agentSock, ctlSock)
	vm.AgentInfo = proto.AgentInfo{Legacy: true}
	if err := e.Pause(context.Background(), vm.ID); err != nil {
		t.Fatal(err)
	}
	if got := readOperations(ops); !slices.Equal(got, []string{"legacy-sync", "PAUSE"}) {
		t.Fatalf("legacy agent received new sync field or no guest-wide sync: %v", got)
	}
	if err := e.Stop(context.Background(), vm.ID); err != nil {
		t.Fatal(err)
	}
	if got := readOperations(ops); !slices.Equal(got, []string{"RESUME", "legacy-sync"}) {
		t.Fatalf("legacy warm stop missed its final sync or sent a new field: %v", got)
	}
}

func TestPauseRejectionKeepsHot(t *testing.T) {
	agentSock, _, ops := durabilityPeer(t, 0)
	controlSock := filepath.Join(shortSockDir(t), "reject.sock")
	ln, err := net.Listen("unix", controlSock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		_, _ = fmt.Fscanln(c, new(string))
		_, _ = fmt.Fprintln(c, "ERR pause refused")
	}()
	e, vm := durabilityEngine(t, "hot", agentSock, controlSock)
	info, err := vm.Agent.Info(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	vm.AgentInfo = info
	vm.persist()
	if err := e.Pause(context.Background(), vm.ID); err == nil {
		t.Fatal("rejected PAUSE returned success")
	}
	if got := readOperations(ops); !slices.Equal(got, []string{"sync"}) {
		t.Fatalf("guest sync did not complete before PAUSE rejection: %v", got)
	}
	rec := readRecord(t, vm.SandboxDir)
	if vm.Thermal != "hot" || rec.Thermal != "hot" {
		t.Fatalf("failed PAUSE recorded a warm VM: %+v", rec)
	}
	if err := e.Stop(context.Background(), vm.ID); err != nil {
		t.Fatal(err)
	}
}

func TestPauseFailedSyncMustBeRetriedAtColdStop(t *testing.T) {
	agentSock, ctlSock, ops := durabilityPeer(t, 1)
	e, vm := durabilityEngine(t, "hot", agentSock, ctlSock)
	info, err := vm.Agent.Info(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	vm.AgentInfo = info
	if err := e.Pause(context.Background(), vm.ID); err != nil {
		t.Fatalf("failed guest sync need not prevent cooling: %v", err)
	}
	if vm.Thermal != "warm" || readRecord(t, vm.SandboxDir).Thermal != "warm" {
		t.Fatal("failed pre-pause guest sync left the VM in an unexpected state")
	}
	if got := readOperations(ops); !slices.Equal(got, []string{"sync", "PAUSE"}) {
		t.Fatalf("failed sync must still precede VMM pause: %v", got)
	}
	var unclean *UncleanStopError
	if err := e.Stop(context.Background(), vm.ID); !errors.As(err, &unclean) {
		t.Fatalf("failed final warm sync did not report unclean power-off: %v", err)
	}
	if got := readOperations(ops); !slices.Equal(got, []string{"RESUME", "sync"}) {
		t.Fatalf("cold stop failed to retry prior sync: %v", got)
	}
}

func TestForcePauseSkipsAgentButWarmStopSyncs(t *testing.T) {
	agentSock, ctlSock, ops := durabilityPeer(t, 0)
	e, vm := durabilityEngine(t, "hot", agentSock, ctlSock)
	if err := e.ForcePause(context.Background(), vm.ID); err != nil {
		t.Fatal(err)
	}
	if got := readOperations(ops); !slices.Equal(got, []string{"PAUSE"}) {
		t.Fatalf("force pause must use control socket only: %v", got)
	}
	// Simulate a daemon that adopted this paused VM: its feature report was
	// unavailable while vCPUs were parked and must be refreshed on RESUME.
	vm.AgentInfoErr = errAgentInfoPaused
	if err := e.Stop(context.Background(), vm.ID); err != nil {
		t.Fatal(err)
	}
	if got := readOperations(ops); !slices.Equal(got, []string{"RESUME", "sync"}) {
		t.Fatalf("forced warm stop must resume and sync before killing: %v", got)
	}
}

func TestFailedSyncPowersOffAndReportsUnclean(t *testing.T) {
	for _, thermal := range []string{"hot", "warm"} {
		t.Run(thermal, func(t *testing.T) {
			agentSock, ctlSock, ops := durabilityPeer(t, 1)
			e, vm := durabilityEngine(t, thermal, agentSock, ctlSock)
			info, err := vm.Agent.Info(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			vm.AgentInfo = info
			err = e.Stop(context.Background(), vm.ID)
			var unclean *UncleanStopError
			if !errors.As(err, &unclean) || !strings.Contains(unclean.UncleanStopReason(), "exit") {
				t.Fatalf("failed guest sync returned %v, want typed unclean result", err)
			}
			if vm.Status != "stopped" || vm.HelperPID != 0 || vm.cmd != nil {
				t.Fatalf("sync failure left helper alive: status=%q pid=%d cmd=%v", vm.Status, vm.HelperPID, vm.cmd)
			}
			if rec := readRecord(t, vm.SandboxDir); rec.Status != "stopped" {
				t.Fatalf("unclean power-off state not persisted: %+v", rec)
			}
			want := []string{"sync"}
			if thermal == "warm" {
				want = []string{"RESUME", "sync"}
			}
			if got := readOperations(ops); !slices.Equal(got, want) {
				t.Fatalf("power-off path operations = %v, want %v", got, want)
			}
		})
	}
}

func TestWarmStopFailedResumeStillPowersOff(t *testing.T) {
	agentSock, _, ops := durabilityPeer(t, 0)
	e, vm := durabilityEngine(t, "warm", agentSock, filepath.Join(shortSockDir(t), "missing.sock"))
	var unclean *UncleanStopError
	if err := e.Stop(context.Background(), vm.ID); !errors.As(err, &unclean) ||
		!strings.Contains(unclean.UncleanStopReason(), "resume") {
		t.Fatalf("failed RESUME result = %v, want unclean power-off", err)
	}
	if vm.Status != "stopped" || vm.HelperPID != 0 || readRecord(t, vm.SandboxDir).Status != "stopped" {
		t.Fatal("failed RESUME left a paused VM running")
	}
	if got := readOperations(ops); len(got) != 0 {
		t.Fatalf("guest agent used while vCPUs remained paused: %v", got)
	}
}

func TestUnresponsiveGuestStopHasShortBoundAndStillPowersOff(t *testing.T) {
	dir := shortSockDir(t)
	agentSock := filepath.Join(dir, "hung.sock")
	ln, err := net.Listen("unix", agentSock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	started := make(chan struct{})
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		if typ, _, err := proto.ReadFrame(c); err == nil && typ == proto.EXEC_REQ {
			close(started)
			_, _, _ = proto.ReadFrame(c) // wait for the host's deadline and close
		}
	}()
	e, vm := durabilityEngine(t, "hot", agentSock, "")
	vm.AgentInfo = proto.AgentInfo{Legacy: true}
	begin := time.Now()
	var unclean *UncleanStopError
	if err := e.Stop(context.Background(), vm.ID); !errors.As(err, &unclean) {
		t.Fatalf("hung agent stop result = %v, want powered-off unclean", err)
	}
	<-started // an actual guest sync request, not an immediate missing-agent error
	if elapsed := time.Since(begin); elapsed > 4*time.Second {
		t.Fatalf("hung guest serialized power-off for %s, want bounded sync", elapsed)
	}
	if rec := readRecord(t, vm.SandboxDir); rec.Status != "stopped" || vm.HelperPID != 0 {
		t.Fatalf("timed-out sync failed to power off VM: %+v, pid=%d", rec, vm.HelperPID)
	}
}
