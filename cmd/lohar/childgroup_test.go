//go:build linux

package main

import (
	"bufio"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// startShellGroup starts script as the leader of its own process group, under
// the reaper, as the exec paths do (but with lohar's own uid, so any user can
// run it). It returns the PID the script prints: the process it backgrounds.
func startShellGroup(t *testing.T, script string) (g *childGroup, background int) {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	cmd := exec.Command("sh", "-c", script)
	cmd.Stdout = w
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	g, err = startGroup(cmd)
	w.Close()
	if err != nil {
		t.Fatal(err)
	}
	r.SetReadDeadline(time.Now().Add(5 * time.Second))
	line, err := bufio.NewReader(r).ReadString('\n')
	if err != nil {
		g.signal(syscall.SIGKILL)
		g.wait()
		t.Fatalf("background PID: %v", err)
	}
	background, err = strconv.Atoi(strings.TrimSpace(line))
	if err != nil {
		t.Fatalf("background PID %q: %v", line, err)
	}
	return g, background
}

func TestChildGroupSignalReachesEveryMember(t *testing.T) {
	g, sleeper := startShellGroup(t, "sleep 3600 & echo $!; wait")
	g.signal(syscall.SIGKILL)
	exit := g.wait()
	if !exit.status.Signaled() || exit.status.Signal() != syscall.SIGKILL {
		t.Errorf("leader status %v, want killed by SIGKILL", exit.status)
	}
	waitProcessGone(t, sleeper)
}

// Once the leader's status is collected its PID is up for reuse. A member that
// outlives it stands in for whatever process holds that PID by then.
func TestChildGroupSignalStopsOnceWaited(t *testing.T) {
	g, bystander := startShellGroup(t, "sleep 3600 & echo $!")
	t.Cleanup(func() { syscall.Kill(bystander, syscall.SIGKILL) })
	if exit := g.wait(); exit.code() != 0 {
		t.Fatalf("leader exit %d, want 0", exit.code())
	}

	g.signal(syscall.SIGKILL)
	for deadline := time.Now().Add(200 * time.Millisecond); time.Now().Before(deadline); {
		if processGone(bystander) {
			t.Fatal("signal after wait still reached the group")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// signal runs on goroutines that never see wait's Release of the process.
func TestChildGroupSignalWhileWaiting(t *testing.T) {
	for range 20 {
		g, _ := startShellGroup(t, "sleep 3600 & echo $!; wait")
		signalled := make(chan struct{})
		go func() {
			defer close(signalled)
			g.signal(syscall.SIGKILL)
		}()
		g.wait()
		<-signalled
		g.signal(syscall.SIGKILL)
	}
}
