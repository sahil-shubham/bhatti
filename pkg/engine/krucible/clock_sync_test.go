package krucible

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/sahil-shubham/bhatti/pkg/agent"
	"github.com/sahil-shubham/bhatti/pkg/agent/proto"
)

// clockPeer is a guest agent and VMM control socket that record, in order,
// the control commands and clock pushes they receive. features is what the
// guest's INFO_RESP advertises; clockReply decides how SET_TIME is answered.
type clockPeer struct {
	agentSock, controlSock string
	ops                    chan string
	stamps                 chan int64
}

func newClockPeer(t *testing.T, features []proto.AgentFeature, clockReply func(net.Conn)) *clockPeer {
	t.Helper()
	dir := shortSockDir(t)
	p := &clockPeer{
		agentSock:   filepath.Join(dir, "a.sock"),
		controlSock: filepath.Join(dir, "k.sock"),
		ops:         make(chan string, 16),
		stamps:      make(chan int64, 16),
	}
	serve := func(path string, handle func(net.Conn)) {
		ln, err := net.Listen("unix", path)
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
					handle(c)
				}()
			}
		}()
	}
	serve(p.controlSock, func(c net.Conn) {
		var line string
		_, _ = fmt.Fscanln(c, &line)
		p.ops <- line
		fmt.Fprintln(c, "OK")
	})
	serve(p.agentSock, func(c net.Conn) {
		typ, payload, err := proto.ReadFrame(c)
		if err != nil {
			return
		}
		if typ == proto.AUTH {
			if typ, payload, err = proto.ReadFrame(c); err != nil {
				return
			}
		}
		switch typ {
		case proto.INFO_REQ:
			_ = proto.SendJSON(c, proto.INFO_RESP, proto.AgentInfo{Version: "v2.5.5", Features: features})
		case proto.SET_TIME:
			p.ops <- "clock"
			if len(payload) == 8 {
				p.stamps <- int64(binary.BigEndian.Uint64(payload))
			}
			clockReply(c)
		case proto.EXEC_REQ:
			p.ops <- "exec"
			exit := proto.ExitPayload(0)
			_ = proto.WriteFrame(c, proto.EXIT, exit[:])
		}
	})
	return p
}

func ackClock(c net.Conn) { _ = proto.WriteFrame(c, proto.SET_TIME, nil) }

func (p *clockPeer) vm(t *testing.T, thermal string, info proto.AgentInfo) (*Engine, *VM) {
	t.Helper()
	vm := &VM{
		ID: "clocked", SandboxDir: t.TempDir(), CtlSockUDS: p.controlSock,
		Status: "running", Thermal: thermal, AgentInfo: info,
		Agent: agent.NewKrucibleClient(p.agentSock, "", ""),
	}
	return &Engine{caps: VMMCapabilities{Pause: true}, vms: map[string]*VM{vm.ID: vm}}, vm
}

var clockSyncInfo = proto.AgentInfo{Version: "v2.5.5", Features: []proto.AgentFeature{proto.FeatureClockSync}}

// A warm wake restores the frozen kvmclock, which leaves CLOCK_REALTIME
// behind by the pause. The host must push its time once RESUME has run, and
// the stamp must be the host's time at that moment.
func TestResumePushesHostTimeAfterResume(t *testing.T) {
	p := newClockPeer(t, clockSyncInfo.Features, ackClock)
	e, vm := p.vm(t, "warm", clockSyncInfo)
	before := time.Now()
	if err := e.EnsureHot(context.Background(), vm.ID); err != nil {
		t.Fatal(err)
	}
	after := time.Now()
	if got := readOperations(p.ops); !slices.Equal(got, []string{"RESUME", "clock"}) {
		t.Fatalf("wake operations = %v, want RESUME then clock", got)
	}
	stamp := time.Unix(0, <-p.stamps)
	if stamp.Before(before) || stamp.After(after) {
		t.Fatalf("pushed time %s outside the wake window [%s, %s]", stamp, before, after)
	}
	if vm.Thermal != "hot" {
		t.Fatalf("thermal = %q after wake", vm.Thermal)
	}
}

// A lohar built before clock_sync would answer SET_TIME with an error; the
// host must not send it at all.
func TestResumeSkipsClockPushForAgentWithoutFeature(t *testing.T) {
	for name, info := range map[string]proto.AgentInfo{
		"older lohar":    {Version: "v2.5.4", Features: []proto.AgentFeature{proto.FeatureReseedCRNG}},
		"legacy lohar":   {Legacy: true},
		"capabilities ?": {},
	} {
		t.Run(name, func(t *testing.T) {
			p := newClockPeer(t, info.Features, ackClock)
			e, vm := p.vm(t, "warm", info)
			if err := e.EnsureHot(context.Background(), vm.ID); err != nil {
				t.Fatal(err)
			}
			if got := readOperations(p.ops); !slices.Equal(got, []string{"RESUME"}) {
				t.Fatalf("operations = %v, want RESUME only", got)
			}
		})
	}
}

func TestResumeSkipsClockPushWhenCapabilityProbeFailed(t *testing.T) {
	p := newClockPeer(t, clockSyncInfo.Features, ackClock)
	e, vm := p.vm(t, "warm", proto.AgentInfo{})
	vm.AgentInfoErr = fmt.Errorf("probe timed out")
	if err := e.EnsureHot(context.Background(), vm.ID); err != nil {
		t.Fatal(err)
	}
	if got := readOperations(p.ops); !slices.Equal(got, []string{"RESUME"}) {
		t.Fatalf("operations = %v, want RESUME only", got)
	}
}

// The sync is best effort: neither a guest refusal nor a hung agent may fail
// the wake, and a hung one may not hold it past the sync timeout.
func TestResumeSurvivesClockPushFailure(t *testing.T) {
	hang := make(chan struct{})
	t.Cleanup(func() { close(hang) })
	for name, reply := range map[string]func(net.Conn){
		"guest error": func(c net.Conn) { _ = proto.WriteFrame(c, proto.ERROR, []byte("set time: operation not permitted")) },
		"wrong ack":   func(c net.Conn) { _ = proto.WriteFrame(c, proto.RESEED, nil) },
		"hung agent":  func(net.Conn) { <-hang },
	} {
		t.Run(name, func(t *testing.T) {
			p := newClockPeer(t, clockSyncInfo.Features, reply)
			e, vm := p.vm(t, "warm", clockSyncInfo)
			start := time.Now()
			if err := e.EnsureHot(context.Background(), vm.ID); err != nil {
				t.Fatalf("wake failed because of the clock push: %v", err)
			}
			if elapsed := time.Since(start); elapsed > guestClockSyncTimeout+time.Second {
				t.Fatalf("wake took %s; the clock push must be bounded by %s", elapsed, guestClockSyncTimeout)
			}
			if vm.Thermal != "hot" {
				t.Fatalf("thermal = %q after wake", vm.Thermal)
			}
			if got := readOperations(p.ops); !slices.Equal(got, []string{"RESUME", "clock"}) {
				t.Fatalf("operations = %v", got)
			}
		})
	}
}

// freezeDisk's brief PAUSE→RESUME also parks the vCPUs, so it leaves the
// guest behind by the copy's duration; the clock is pushed after its RESUME.
func TestFreezeDiskPushesHostTimeAfterResume(t *testing.T) {
	p := newClockPeer(t, clockSyncInfo.Features, ackClock)
	e, vm := p.vm(t, "hot", clockSyncInfo)
	src := filepath.Join(t.TempDir(), "root.qcow2")
	if err := os.WriteFile(src, []byte("overlay"), 0o600); err != nil {
		t.Fatal(err)
	}
	vm.baseSpec.RootDisk = src
	if err := e.freezeDisk(context.Background(), vm.ID, filepath.Join(t.TempDir(), "copy.qcow2")); err != nil {
		t.Fatal(err)
	}
	if got := readOperations(p.ops); !slices.Equal(got, []string{"exec", "PAUSE", "RESUME", "clock"}) {
		t.Fatalf("freeze operations = %v, want sync, PAUSE, RESUME, clock", got)
	}
}

// A memory image keeps the wall-clock offset it was saved with, lag included,
// so a restored clone gets the host's time next to its CRNG reseed. The
// source's checkpoint pause is followed by a push as well.
func TestForkPushesHostTimeBeforeReturning(t *testing.T) {
	e := fakeReseedEngine(t, "supported")
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	source, err := e.Create(ctx, noNetworkSpec("original"))
	if err != nil {
		t.Fatal(err)
	}
	before := time.Now()
	clone, err := e.Fork(ctx, source.ID, "clone")
	if err != nil {
		t.Fatal(err)
	}
	after := time.Now()
	vm, err := e.getVM(clone.ID)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(vm.SandboxDir, "clock.bin"))
	if err != nil || len(raw) != 8 {
		t.Fatalf("clone returned without a clock push: %v, %d bytes", err, len(raw))
	}
	if sent := time.Unix(0, int64(binary.BigEndian.Uint64(raw))); sent.Before(before) || sent.After(after) {
		t.Fatalf("clone's clock pushed as %s, outside the fork window [%s, %s]", sent, before, after)
	}
	// The fork's checkpoint parks the source's vCPUs briefly too, and the
	// push after that RESUME keeps the source from lagging by the save.
	srcRaw, err := os.ReadFile(filepath.Join(e.vms[source.ID].SandboxDir, "clock.bin"))
	if err != nil || len(srcRaw) != 8 {
		t.Fatalf("source got no clock push after its checkpoint resumed: %v, %d bytes", err, len(srcRaw))
	}
	if sent := time.Unix(0, int64(binary.BigEndian.Uint64(srcRaw))); sent.Before(before) || sent.After(after) {
		t.Fatalf("source's clock pushed as %s, outside the fork window [%s, %s]", sent, before, after)
	}
}

// A restore must not fail over the clock: an agent that predates clock_sync
// is not sent SET_TIME, and one that refuses it only costs a warning.
func TestRestoreClockPushIsFeatureGatedAndBestEffort(t *testing.T) {
	for _, mode := range []string{"old", "clockfail"} {
		t.Run(mode, func(t *testing.T) {
			e := fakeReseedEngine(t, mode)
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			source, err := e.Create(ctx, noNetworkSpec("original"))
			if err != nil {
				t.Fatal(err)
			}
			clone, err := e.Fork(ctx, source.ID, "clone")
			if err != nil {
				t.Fatalf("fork failed over the clock push: %v", err)
			}
			vm, err := e.getVM(clone.ID)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(filepath.Join(vm.SandboxDir, "clock.bin")); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("%s: clock push recorded: %v", mode, err)
			}
		})
	}
}
