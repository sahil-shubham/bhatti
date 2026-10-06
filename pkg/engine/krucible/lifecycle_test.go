package krucible

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/sahil-shubham/bhatti/pkg/agent"
	"github.com/sahil-shubham/bhatti/pkg/engine"
	"github.com/sahil-shubham/bhatti/pkg/gateway"
)

func awaitLifecycle(t *testing.T, events <-chan engine.LifecycleEvent) engine.LifecycleEvent {
	t.Helper()
	select {
	case event := <-events:
		return event
	case <-time.After(4 * time.Second):
		t.Fatal("process death was not reported within four seconds")
		return engine.LifecycleEvent{}
	}
}

func ownedVMFixture(t *testing.T) (*Engine, *VM, *exec.Cmd, chan engine.LifecycleEvent) {
	t.Helper()
	dir, socks := t.TempDir(), shortSockDir(t)
	cmd := exec.Command("sleep", "30")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	waitDone := make(chan error, 1)
	go func() { waitDone <- cmd.Wait() }()
	t.Cleanup(func() { _ = cmd.Process.Kill() })
	cfgSock := filepath.Join(socks, "cfg.sock")
	cfgSrv, err := newConfigServer(cfgSock, []byte("{}"))
	if err != nil {
		t.Fatal(err)
	}
	vm := &VM{ID: "vm", Name: "vm", Status: "running", Thermal: "hot", UserID: "owner", sandboxRef: "server-vm",
		SandboxDir: dir, SockDir: socks, cmd: cmd, waitDone: waitDone, exitDone: make(chan struct{}), HelperPID: cmd.Process.Pid,
		CtlSockUDS: filepath.Join(socks, "k.sock"), ControlUDS: filepath.Join(socks, "c.sock"),
		Agent: agent.NewKrucibleClient(filepath.Join(socks, "c.sock"), "", ""), configSrv: cfgSrv,
		baseSpec: VMSpec{VsockConfigUDS: cfgSock}}
	if err := os.WriteFile(vm.CtlSockUDS, []byte("old socket"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "rootfs.ext4"), []byte("disk survives"), 0o600); err != nil {
		t.Fatal(err)
	}
	vm.persist()
	e := &Engine{vms: map[string]*VM{vm.ID: vm}, netds: make(map[string]*netdInstance)}
	events := make(chan engine.LifecycleEvent, 4)
	e.SetLifecycleHandler(func(event engine.LifecycleEvent) { events <- event })
	e.watchVM(vm, cmd, waitDone, vm.exitDone)
	t.Cleanup(e.Shutdown)
	return e, vm, cmd, events
}

func TestOwnedVMMDeathStopsAndPersistsBeforeEvent(t *testing.T) {
	e, vm, cmd, events := ownedVMFixture(t)
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	event := awaitLifecycle(t, events)
	if event.Kind != engine.VMExited || event.Signal != "killed" || event.ExitCode != -1 ||
		event.EngineID != vm.ID || event.SandboxID != "server-vm" || event.UserID != "owner" {
		t.Fatalf("wrong VM exit event: %+v", event)
	}
	status, err := e.Status(context.Background(), vm.ID)
	if err != nil || status.Status != "stopped" || e.ThermalState(vm.ID) != "cold" {
		t.Fatalf("dead VM still running: %+v, %v", status, err)
	}
	if vm.Agent != nil || vm.HelperPID != 0 || vm.cmd != nil {
		t.Fatalf("dead helper retained: pid=%d cmd=%v agent=%v", vm.HelperPID, vm.cmd, vm.Agent)
	}
	rec := readRecord(t, vm.SandboxDir)
	if rec.Status != "stopped" || rec.Thermal != "cold" || rec.HelperPID != 0 {
		t.Fatalf("event preceded durable stopped state: %+v", rec)
	}
	if _, err := os.Stat(vm.CtlSockUDS); !os.IsNotExist(err) {
		t.Fatalf("stale control socket retained: %v", err)
	}
	if _, err := os.Stat(vm.baseSpec.VsockConfigUDS); !os.IsNotExist(err) || vm.configSrv != nil {
		t.Fatalf("boot config server survived helper death: socket=%v srv=%v", err, vm.configSrv)
	}
	if data, err := os.ReadFile(filepath.Join(vm.SandboxDir, "rootfs.ext4")); err != nil || string(data) != "disk survives" {
		t.Fatalf("guest disk lost on crash: %q (%v)", data, err)
	}
	select {
	case duplicate := <-events:
		t.Fatalf("duplicate event: %+v", duplicate)
	default:
	}
}

func TestOwnedVMMNormalExitReportsCode(t *testing.T) {
	pr, pw, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("/bin/sh", "-c", "read line; exit 7")
	cmd.Stdin = pr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	pr.Close()
	t.Cleanup(func() { _ = cmd.Process.Kill(); pw.Close() })
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	vm := &VM{ID: "normal-exit", SandboxDir: t.TempDir(), Status: "running", Thermal: "hot",
		cmd: cmd, waitDone: done, exitDone: make(chan struct{}), HelperPID: cmd.Process.Pid}
	e := &Engine{vms: map[string]*VM{vm.ID: vm}}
	t.Cleanup(e.Shutdown)
	events := make(chan engine.LifecycleEvent, 1)
	e.SetLifecycleHandler(func(event engine.LifecycleEvent) { events <- event })
	e.watchVM(vm, cmd, done, vm.exitDone)
	if _, err := pw.Write([]byte("go\n")); err != nil {
		t.Fatal(err)
	}
	event := awaitLifecycle(t, events)
	if event.Kind != engine.VMExited || event.ExitCode != 7 || event.Signal != "" {
		t.Fatalf("normal process exit lost status code: %+v", event)
	}
}

func TestOwnedVMMDeathRevokesNetdPolicy(t *testing.T) {
	e, vm, cmd, events := ownedVMFixture(t)
	_, inst, netd := netdFixture(t, false)
	e.netdMu.Lock()
	e.netds[inst.owner] = inst
	e.netdMu.Unlock()
	vm.mu.Lock()
	vm.netdKey, vm.netIP = inst.owner, "100.64.1.2"
	vm.mu.Unlock()
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	if event := awaitLifecycle(t, events); event.Kind != engine.VMExited {
		t.Fatalf("no VM exit after process death: %+v", event)
	}
	if ops := netd.operations(); len(ops) != 1 || ops[0] != gateway.ControlDel {
		t.Fatalf("stopped VM retained its gateway registration: %v", ops)
	}
}

func TestRequestedVMMKillDoesNotEmitExit(t *testing.T) {
	e, vm, _, events := ownedVMFixture(t)
	if err := e.Destroy(context.Background(), vm.ID); err != nil {
		t.Fatal(err)
	}
	select {
	case event := <-events:
		t.Fatalf("explicit Destroy reported spontaneous exit: %+v", event)
	case <-time.After(100 * time.Millisecond):
	}
}

func TestAdoptedVMMDeathIsObservedWithoutRestart(t *testing.T) {
	e := &Engine{cfg: Config{DataDir: t.TempDir(), SocketDir: shortSockDir(t)}, vms: map[string]*VM{}, netds: map[string]*netdInstance{}}
	spec, control := sandboxPaths(t, e, "adopted")
	pid := fakeHelper(t, spec)
	fakeControl(t, control, "running")
	recordSandbox(e, "adopted", "running", pid)
	e.recover()
	t.Cleanup(e.Shutdown)
	events := make(chan engine.LifecycleEvent, 2)
	e.SetLifecycleHandler(func(event engine.LifecycleEvent) { events <- event })
	if err := syscall.Kill(pid, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	event := awaitLifecycle(t, events)
	if event.Kind != engine.VMExited || event.EngineID != "adopted" || event.ExitCode != -1 || event.Signal != "" ||
		!strings.Contains(event.Reason, "exit status unavailable") {
		t.Fatalf("adopted exit reported as child: %+v", event)
	}
	if info, err := e.Status(context.Background(), "adopted"); err != nil || info.Status != "stopped" {
		t.Fatalf("adopted VM still running: %+v (%v)", info, err)
	}
	if rec := readRecord(t, e.vms["adopted"].SandboxDir); rec.HelperPID != 0 || rec.Thermal != "cold" {
		t.Fatalf("adopted exit not persisted: %+v", rec)
	}
}

// This subprocess provides real gateway sockets and the HELLO handshake without
// a hypervisor or the netd binary; the surrounding tests kill its actual PID.
func TestLifecycleGatewayChild(t *testing.T) {
	if os.Getenv("BHATTI_LIFECYCLE_TEST_GATEWAY") != "1" {
		return
	}
	args := os.Args
	for i, arg := range args {
		if arg == "--" {
			args = args[i+1:]
			break
		}
	}
	var netSock, ctlSock string
	for i := 0; i+1 < len(args); i++ {
		switch args[i] {
		case "--net-uds":
			netSock = args[i+1]
		case "--ctl-uds":
			ctlSock = args[i+1]
		}
	}
	ln, err := net.Listen("unix", netSock)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	ctl, err := net.Listen("unix", ctlSock)
	if err != nil {
		t.Fatal(err)
	}
	defer ctl.Close()
	_ = gateway.ServeControl(ctl, &recordingNetd{})
}

func gatewayBinary(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "gateway-test.sh")
	script := fmt.Sprintf("#!/bin/sh\nBHATTI_LIFECYCLE_TEST_GATEWAY=1 exec %q -test.run '^TestLifecycleGatewayChild$' -- \"$@\"\n", os.Args[0])
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestGatewayDeathFromSpawnAndAdoption(t *testing.T) {
	for _, mode := range []string{"spawned", "adopted"} {
		t.Run(mode, func(t *testing.T) {
			socks := shortSockDir(t)
			owner := "u:owner"
			inst := &netdInstance{owner: owner, dir: filepath.Join(socks, "owner"),
				sock: filepath.Join(socks, "owner", "n.sock"), ctlSock: filepath.Join(socks, "owner", "ctl.sock"), refs: 3}
			if err := os.MkdirAll(inst.dir, 0o700); err != nil {
				t.Fatal(err)
			}
			e := &Engine{cfg: Config{SocketDir: socks, NetdBinary: gatewayBinary(t)}, netds: map[string]*netdInstance{owner: inst},
				vms: map[string]*VM{
					"first":   {ID: "first", sandboxRef: "sb-1", UserID: "owner", netdKey: owner, Status: "running"},
					"second":  {ID: "second", UserID: "owner", netdKey: owner, Status: "running"},
					"stopped": {ID: "stopped", sandboxRef: "sb-stopped", UserID: "owner", netdKey: owner, Status: "stopped"},
				}}
			t.Cleanup(func() {
				for range 3 {
					e.releaseNetd(owner)
				}
				e.Shutdown()
			})
			var oldPID int
			if mode == "adopted" {
				oldPID = standinNetd(t, inst.sock)
				inst.pid = oldPID
				network, err := net.Listen("unix", inst.sock)
				if err != nil {
					t.Fatal(err)
				}
				defer network.Close()
				control, err := net.Listen("unix", inst.ctlSock)
				if err != nil {
					t.Fatal(err)
				}
				defer control.Close()
				go gateway.ServeControl(control, &recordingNetd{})
			}
			if err := e.ensureNetd(owner); err != nil {
				t.Fatalf("start/adopt netd: %v", err)
			}
			if mode == "spawned" {
				inst.mu.Lock()
				oldPID = inst.pid
				inst.mu.Unlock()
			}
			events := make(chan engine.LifecycleEvent, 4)
			e.SetLifecycleHandler(func(event engine.LifecycleEvent) { events <- event })
			if err := syscall.Kill(oldPID, syscall.SIGKILL); err != nil {
				t.Fatal(err)
			}
			seen := make(map[string]engine.LifecycleEvent)
			for range 3 {
				event := awaitLifecycle(t, events)
				if event.Kind != engine.NetworkLost || !strings.Contains(event.Reason, "cannot reattach") {
					t.Fatalf("gateway death had wrong event: %+v", event)
				}
				seen[event.EngineID] = event
			}
			if len(seen) != 3 || seen["first"].SandboxID != "sb-1" ||
				seen["second"].SandboxID != "" || seen["stopped"].SandboxID != "sb-stopped" {
				t.Fatalf("gateway death omitted an owner's sandbox: %+v", seen)
			}
			if e.netdRunning(owner) {
				t.Fatal("netdRunning true after gateway was reaped")
			}
			if err := e.ensureNetd(owner); err != nil {
				t.Fatalf("new sandbox could not respawn gateway: %v", err)
			}
			inst.mu.Lock()
			newPID := inst.pid
			inst.mu.Unlock()
			if newPID <= 0 || newPID == oldPID || !e.netdRunning(owner) || gateway.ProbeVersion(inst.ctlSock) != nil {
				t.Fatalf("gateway did not respawn with working control: old=%d new=%d", oldPID, newPID)
			}
			select {
			case duplicate := <-events:
				t.Fatalf("duplicate network-loss event: %+v", duplicate)
			default:
			}
		})
	}
}

func TestListDoesNotWaitForHelperReap(t *testing.T) {
	cmd := exec.Command("sleep", "30")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	gate, reaped, done := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	go func() {
		err := cmd.Wait() // sole reaper, but deliberately delay its notification
		close(reaped)
		<-gate
		done <- err
	}()
	defer func() {
		_ = cmd.Process.Kill()
		select {
		case <-gate:
		default:
			close(gate)
		}
	}()
	vm := &VM{ID: "slow-kill", Status: "running", Thermal: "hot", SandboxDir: t.TempDir(),
		cmd: cmd, waitDone: done, HelperPID: cmd.Process.Pid}
	e := &Engine{vms: map[string]*VM{"slow-kill": vm, "other": {ID: "other", Status: "running"}}}
	destroyed := make(chan error, 1)
	go func() { destroyed <- e.Destroy(context.Background(), vm.ID) }()
	select {
	case <-reaped:
	case <-time.After(time.Second):
		t.Fatal("Destroy did not reach helper wait")
	}
	query := make(chan error, 1)
	go func() {
		list, err := e.List(context.Background())
		if err == nil && len(list) != 2 {
			err = fmt.Errorf("List had %d guests while Destroy waited for reap", len(list))
		}
		if err == nil {
			_, err = e.Status(context.Background(), vm.ID)
		}
		query <- err
	}()
	select {
	case err := <-query:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(250 * time.Millisecond):
		t.Fatal("List/Status held behind another helper's process wait")
	}
	close(gate)
	if err := <-destroyed; err != nil {
		t.Fatal(err)
	}
}

func TestListAndStatusDoNotWaitForSlowControlSocket(t *testing.T) {
	socks := shortSockDir(t)
	ctl := filepath.Join(socks, "ctl.sock")
	ln, err := net.Listen("unix", ctl)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	entered := make(chan struct{}, 1)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		_, _ = bufio.NewReader(conn).ReadString('\n')
		entered <- struct{}{}
		_, _ = bufio.NewReader(conn).ReadString('\n') // never acknowledge PAUSE
	}()
	e := &Engine{caps: VMMCapabilities{Pause: true}, vms: map[string]*VM{
		"stalled": {ID: "stalled", CtlSockUDS: ctl, Status: "running", Thermal: "hot"},
		"other":   {ID: "other", Status: "running", Thermal: "hot"},
	}}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	finished := make(chan error, 1)
	go func() { finished <- e.ForcePause(ctx, "stalled") }()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("PAUSE never reached stalled control socket")
	}
	queryDone := make(chan error, 1)
	go func() {
		list, err := e.List(context.Background())
		if err == nil && len(list) != 2 {
			err = fmt.Errorf("List had %d guests, want 2", len(list))
		}
		if err == nil {
			_, err = e.Status(context.Background(), "stalled")
		}
		queryDone <- err
	}()
	select {
	case err := <-queryDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(250 * time.Millisecond):
		t.Fatal("List/Status blocked behind unrelated control socket")
	}
	if err := <-finished; err == nil {
		t.Fatal("stalled PAUSE unexpectedly completed")
	}
}
