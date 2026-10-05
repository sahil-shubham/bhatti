package krucible

import (
	"context"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/sahil-shubham/bhatti/pkg/engine"
	"github.com/sahil-shubham/bhatti/pkg/gateway"
)

// A shell waiting on stdin has the same argv identity check as a detached netd,
// but needs neither a hypervisor nor an installed netd binary.
func standinNetd(t *testing.T, sock string) int {
	t.Helper()
	script := filepath.Join(t.TempDir(), "netd.sh")
	if err := os.WriteFile(script, []byte("read line\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	pr, pw, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("/bin/sh")
	cmd.Args = []string{"bhatti-netd", script, "--net-uds", sock}
	cmd.Stdin = pr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	pr.Close()
	t.Cleanup(func() { _ = cmd.Process.Kill(); _, _ = cmd.Process.Wait(); pw.Close() })
	execd(t, cmd.Process.Pid, "bhatti-netd", script, "--net-uds", sock)
	return cmd.Process.Pid
}

type recordingNetd struct {
	mu  sync.Mutex
	ops []gateway.ControlOp
}

func (h *recordingNetd) SetSandbox(_, _ string, _ *gateway.EgressPolicy, _, _ string) error {
	h.mu.Lock()
	h.ops = append(h.ops, gateway.ControlSet)
	h.mu.Unlock()
	return nil
}
func (h *recordingNetd) DelSandbox(string) {
	h.mu.Lock()
	h.ops = append(h.ops, gateway.ControlDel)
	h.mu.Unlock()
}
func (h *recordingNetd) operations() []gateway.ControlOp {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]gateway.ControlOp(nil), h.ops...)
}

func netdFixture(t *testing.T, oldControl bool) (*Engine, *netdInstance, *recordingNetd) {
	t.Helper()
	dir := shortSockDir(t)
	key := "u:owner"
	path := filepath.Join(dir, "netd")
	if err := os.MkdirAll(path, 0o700); err != nil {
		t.Fatal(err)
	}
	inst := &netdInstance{owner: key, dir: path, sock: filepath.Join(path, "n.sock"), ctlSock: filepath.Join(path, "ctl.sock"), refs: 1}
	inst.pid = standinNetd(t, inst.sock)
	netLn, err := net.Listen("unix", inst.sock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { netLn.Close() })
	ctlLn, err := net.Listen("unix", inst.ctlSock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ctlLn.Close() })
	h := &recordingNetd{}
	if oldControl {
		go func() {
			for {
				c, err := ctlLn.Accept()
				if err != nil {
					return
				}
				// Legacy netd consumes messages but never replies to HELLO.
				go func() {
					defer c.Close()
					var buf [256]byte
					for {
						if _, err := c.Read(buf[:]); err != nil {
							return
						}
					}
				}()
			}
		}()
	} else {
		go gateway.ServeControl(ctlLn, h)
	}
	e := &Engine{cfg: Config{DataDir: t.TempDir(), SocketDir: dir, NetdBinary: "/nonexistent-bhatti-netd"},
		vms: map[string]*VM{}, netds: map[string]*netdInstance{key: inst}}
	return e, inst, h
}

func TestIncompatibleNetdIsKilledBeforeReuseAndEventsAreQueued(t *testing.T) {
	e, inst, _ := netdFixture(t, true)
	pid := inst.pid
	e.vms["sb"] = &VM{ID: "sb", sandboxRef: "sb-server", UserID: "owner", netdKey: inst.owner, Status: "running"}
	if err := e.ensureNetd(inst.owner); err == nil {
		t.Fatal("missing replacement binary should fail closed")
	}
	if isNetd(pid, inst.sock) {
		t.Fatal("outdated netd still running after failed replacement")
	}
	if inst.pid != 0 {
		t.Fatalf("incompatible pid %d retained", inst.pid)
	}
	var events []netdEvent
	e.SetNetdEventRecorder(func(user, sandbox, reason string) { events = append(events, netdEvent{user, sandbox, reason}) })
	if len(events) != 1 || events[0].userID != "owner" || events[0].sandboxID != "sb-server" || !strings.Contains(events[0].reason, "hello") {
		t.Fatalf("missing per-sandbox network-loss event: %+v", events)
	}
}

func TestCreateFailureRevokesPushedSharedNetdPolicy(t *testing.T) {
	t.Setenv("KRUCIBLE_ROOT_RAW", "1")
	e, inst, h := netdFixture(t, false)
	base := filepath.Join(e.cfg.DataDir, "base.img")
	if err := os.WriteFile(base, []byte("base image"), 0o600); err != nil {
		t.Fatal(err)
	}
	e.cfg.BaseImage = base
	e.cfg.BlockRoot = true
	e.cfg.KernelImage = "unused-kernel"
	e.cfg.VMMBinary = "/nonexistent-vmm"
	// A sibling keeps the gateway alive, so a leaked policy would remain live.
	inst.refs++
	_, err := e.Create(context.Background(), engine.SandboxSpec{UserID: "owner", SubnetIndex: 0,
		NetPolicy: &gateway.NetPolicyWire{Default: "public"}})
	if err == nil {
		t.Fatal("unexpected successful helper launch")
	}
	ops := h.operations()
	if len(ops) != 2 || ops[0] != gateway.ControlSet || ops[1] != gateway.ControlDel {
		t.Fatalf("create failure left pushed policy behind: %v (error: %v)", ops, err)
	}
}
