// No build tag: adoption logic with stand-in processes and a fake control
// socket, so it runs wherever `go test ./...` does (macOS, linux/arm64,
// linux/amd64), no hypervisor needed.

package krucible

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// running reports whether pid still runs argv. pidAlive can't tell a killed
// child of ours from a live one: it stays a zombie until reaped.
func running(pid int, argv ...string) bool {
	args, err := procArgs(pid)
	return err == nil && slices.Equal(args, argv)
}

// execd waits until pid shows argv. Start returns once exec is past its
// point of no return, a moment before the kernel publishes the new argv.
func execd(t *testing.T, pid int, argv ...string) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); !running(pid, argv...); time.Sleep(time.Millisecond) {
		if time.Now().After(deadline) {
			args, err := procArgs(pid)
			t.Fatalf("pid %d never showed argv %q: %q (%v)", pid, argv, args, err)
		}
	}
}

// fakeHelper starts a process that passes for the helper running spec: argv
// ["bhatti-vmm", spec]. The script reports when /bin/sh has finished startup
// and reached its blocking read; seeing argv before that point isn't enough on
// Darwin, where shell initialization can still change the process arguments.
func fakeHelper(t *testing.T, spec string) int {
	t.Helper()
	ready := spec + ".ready"
	script := fmt.Sprintf("printf ready > %q\nread line\n", ready)
	if err := os.WriteFile(spec, []byte(script), 0o644); err != nil {
		t.Fatal(err)
	}
	pr, pw, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("/bin/sh")
	cmd.Args = []string{"bhatti-vmm", spec}
	cmd.Stdin = pr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	pr.Close()
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
		pw.Close()
	})
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(time.Millisecond) {
		if _, err := os.Stat(ready); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("fake helper did not reach its blocking read: pid=%d spec=%s", cmd.Process.Pid, spec)
		}
	}
	execd(t, cmd.Process.Pid, "bhatti-vmm", spec)
	return cmd.Process.Pid
}

// fakeControl answers STATUS on sock as a helper's control socket does.
func fakeControl(t *testing.T, sock, state string) {
	t.Helper()
	ln, err := net.Listen("unix", sock)
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
				if line, _ := bufio.NewReader(c).ReadString('\n'); strings.TrimSpace(line) == "STATUS" {
					fmt.Fprintf(c, "OK %s\n", state)
				} else {
					fmt.Fprintln(c, "ERR unknown command")
				}
			}()
		}
	}()
}

// sandboxPaths makes sandbox id's directories under e and returns the spec
// its helper runs and its control socket.
func sandboxPaths(t *testing.T, e *Engine, id string) (spec, ctl string) {
	t.Helper()
	dir := filepath.Join(e.cfg.DataDir, "sandboxes", id)
	socks := filepath.Join(e.cfg.SocketDir, id)
	for _, d := range []string{dir, socks} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	return filepath.Join(dir, "vmspec.json"), filepath.Join(socks, "k.sock")
}

// recordSandbox writes sandbox id's state.json as a daemon would have left it.
func recordSandbox(e *Engine, id, status string, pid int) {
	dir := filepath.Join(e.cfg.DataDir, "sandboxes", id)
	socks := filepath.Join(e.cfg.SocketDir, id)
	ctl := filepath.Join(socks, "k.sock")
	writeRecord(vmRecord{
		ID: id, Name: id, SandboxDir: dir, SockDir: socks,
		ControlUDS: filepath.Join(socks, "c.sock"), ForwardUDS: filepath.Join(socks, "f.sock"), CtlSockUDS: ctl,
		Status: status, Thermal: "hot", HelperPID: pid,
		BaseSpec: VMSpec{ControlSocketUDS: ctl},
	})
}

// readRecord reads back the state.json in sandboxDir.
func readRecord(t *testing.T, sandboxDir string) vmRecord {
	t.Helper()
	data, err := os.ReadFile(stateFilePath(sandboxDir))
	if err != nil {
		t.Fatal(err)
	}
	var rec vmRecord
	if err := json.Unmarshal(data, &rec); err != nil {
		t.Fatal(err)
	}
	return rec
}

// TestProcArgs: a process's argv as started (spawnStandin waits until procArgs
// shows it), and what isHelper makes of it.
func TestProcArgs(t *testing.T) {
	_, sleeper := spawnStandin(t)
	spec := filepath.Join(t.TempDir(), "vmspec.json")
	if pid := fakeHelper(t, spec); !isHelper(pid, spec) {
		args, err := procArgs(pid)
		t.Fatalf("a process started on %s isn't taken for its helper: argv %q (%v)", spec, args, err)
	}
	if isHelper(sleeper, spec) {
		t.Fatal("sleep taken for a helper")
	}
}

// TestRecoverAdoptsLiveHelpers is the restart path, with stand-ins for helpers:
// a helper still running its sandbox's spec that answers on its control
// socket is adopted as it is — running, or paused (warm, its agent asked only
// once resumed) — where recovery used to require the guest agent to answer,
// which a paused guest can't: it was marked stopped with its helper left
// running. A helper that is the sandbox's but doesn't answer, or whose launch
// never finished, is killed before the sandbox is marked stopped, so its next
// boot doesn't share disks with it. A recorded pid that is now another process
// is left alone.
func TestRecoverAdoptsLiveHelpers(t *testing.T) {
	e := &Engine{cfg: Config{DataDir: t.TempDir(), SocketDir: shortSockDir(t)}, vms: map[string]*VM{}, netds: map[string]*netdInstance{}}

	spec, ctl := sandboxPaths(t, e, "running")
	runningPID := fakeHelper(t, spec)
	fakeControl(t, ctl, "running")
	recordSandbox(e, "running", "running", runningPID)

	spec, ctl = sandboxPaths(t, e, "paused")
	pausedPID := fakeHelper(t, spec)
	fakeControl(t, ctl, "paused")
	recordSandbox(e, "paused", "running", pausedPID)

	spec, _ = sandboxPaths(t, e, "wedged") // nothing on its control socket
	wedgedPID := fakeHelper(t, spec)
	recordSandbox(e, "wedged", "running", wedgedPID)

	spec, ctl = sandboxPaths(t, e, "launching") // recorded before the guest came up
	launchingPID := fakeHelper(t, spec)
	fakeControl(t, ctl, "running")
	recordSandbox(e, "launching", "stopped", launchingPID)

	sandboxPaths(t, e, "reused")
	_, otherPID := spawnStandin(t)
	recordSandbox(e, "reused", "running", otherPID)

	sandboxPaths(t, e, "dead")
	gone := exec.Command("true")
	if err := gone.Run(); err != nil {
		t.Fatal(err)
	}
	recordSandbox(e, "dead", "running", gone.Process.Pid)

	e.recover()

	want := map[string]struct {
		status, thermal string
		pid             int
	}{
		"running":   {"running", "hot", runningPID},
		"paused":    {"running", "warm", pausedPID},
		"wedged":    {"stopped", "cold", 0},
		"launching": {"stopped", "cold", 0},
		"reused":    {"stopped", "cold", 0},
		"dead":      {"stopped", "cold", 0},
	}
	for id, w := range want {
		vm := e.vms[id]
		if vm == nil {
			t.Errorf("%s: not recovered", id)
			continue
		}
		if vm.Status != w.status || vm.Thermal != w.thermal || vm.HelperPID != w.pid {
			t.Errorf("%s: recovered %s/%s pid %d, want %s/%s pid %d", id, vm.Status, vm.Thermal, vm.HelperPID, w.status, w.thermal, w.pid)
		}
		if (vm.Agent != nil) != (w.status == "running") {
			t.Errorf("%s: agent client %v with status %s", id, vm.Agent, vm.Status)
		}
		if rec := readRecord(t, vm.SandboxDir); rec.Status != w.status || rec.Thermal != w.thermal || rec.HelperPID != w.pid {
			t.Errorf("%s: state.json says %s/%s pid %d, want %s/%s pid %d", id, rec.Status, rec.Thermal, rec.HelperPID, w.status, w.thermal, w.pid)
		}
	}
	if err := e.vms["paused"].AgentInfoErr; !errors.Is(err, errAgentInfoPaused) {
		t.Errorf("paused: agent info error %v, want it left for Resume", err)
	}
	for id, pid := range map[string]int{"running": runningPID, "paused": pausedPID} {
		if !running(pid, "bhatti-vmm", e.vms[id].specPath()) {
			t.Errorf("%s: helper (pid %d) killed by recovery", id, pid)
		}
	}
	if !running(otherPID, "sleep", "60") {
		t.Errorf("reused: pid %d, no helper of ours, killed by recovery", otherPID)
	}
	for id, pid := range map[string]int{"wedged": wedgedPID, "launching": launchingPID} {
		if pidAlive(pid) {
			t.Errorf("%s: its helper (pid %d) left running beside a stopped sandbox", id, pid)
		}
	}
}

// TestKillChecksHelperIdentity: killing an adopted helper (no Cmd handle, only
// the recorded pid) signals the pid only while it still runs the sandbox's
// spec — after the helper exits its number can be anyone's — and returns once
// the helper is gone, so a relaunch can't open its disks while it holds them.
func TestKillChecksHelperIdentity(t *testing.T) {
	dir := t.TempDir()
	_, otherPID := spawnStandin(t)
	vm := &VM{ID: "x", SandboxDir: dir, HelperPID: otherPID}
	vm.kill()
	if !running(otherPID, "sleep", "60") {
		t.Fatal("kill signalled a pid that isn't the sandbox's helper")
	}
	if vm.HelperPID != 0 {
		t.Fatalf("HelperPID %d after kill, want 0", vm.HelperPID)
	}

	pid := fakeHelper(t, vm.specPath())
	vm.HelperPID = pid
	vm.kill()
	if pidAlive(pid) {
		t.Fatal("adopted helper still there when kill returned")
	}
}

// TestLaunchRecordsHelperBeforeReady: state.json names a helper from the moment
// it starts, not only once its guest is up, so a daemon that goes away
// mid-launch leaves a helper the next one finds (and kills, see
// TestRecoverAdoptsLiveHelpers). A failed launch takes the name back.
func TestLaunchRecordsHelperBeforeReady(t *testing.T) {
	fake := filepath.Join(t.TempDir(), "slow-vmm")
	if err := os.WriteFile(fake, []byte("#!/bin/sh\nexec sleep 30\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	dir, socks := t.TempDir(), shortSockDir(t)
	vm := &VM{
		ID: "launching", Status: "stopped", Thermal: "cold",
		SandboxDir: dir, SockDir: socks, logPath: filepath.Join(dir, "vmm.log"),
		ControlUDS: filepath.Join(socks, "c.sock"), ForwardUDS: filepath.Join(socks, "f.sock"),
	}
	e := &Engine{cfg: Config{VMMBinary: fake}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	errc := make(chan error, 1)
	go func() { errc <- e.launch(ctx, vm, "") }()

	var rec vmRecord
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		data, err := os.ReadFile(stateFilePath(dir))
		if err == nil && json.Unmarshal(data, &rec) == nil && rec.HelperPID != 0 {
			break
		}
		if time.Now().After(deadline) {
			cancel()
			t.Fatalf("state.json never named the helper while its guest was coming up (%v)", <-errc)
		}
	}
	if !pidAlive(rec.HelperPID) || rec.Status != "stopped" {
		t.Errorf("mid-launch record: pid %d (alive %v), status %q, want a live helper and stopped", rec.HelperPID, pidAlive(rec.HelperPID), rec.Status)
	}

	cancel()
	if err := <-errc; err == nil {
		t.Fatal("launch succeeded without an agent")
	}
	if rec = readRecord(t, dir); rec.HelperPID != 0 {
		t.Fatalf("failed launch left state.json naming helper %d", rec.HelperPID)
	}
}
