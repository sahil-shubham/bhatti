//go:build krucible

package krucible

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sahil-shubham/bhatti/pkg/engine"
)

// TestKrucibleVMMConfined: the helper the engine spawns is confined from its
// first instruction. Every one of its threads has no_new_privs and no
// capabilities and, the daemon being root, runs as the sandbox's own
// unprivileged uid. And it can't write outside its sandbox: told over its
// control socket to SAVE into a directory elsewhere that anyone may write to,
// it is refused and nothing appears there, while the same SAVE into its own
// save dir works.
func TestKrucibleVMMConfined(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("bhatti-vmm is confined on Linux only")
	}
	e := newBlockRootEngine(t).(*Engine)
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Second)
	defer cancel()
	info, err := e.Create(ctx, engine.SandboxSpec{Name: "vmmconf", CPUs: 1, MemoryMB: 512})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	t.Cleanup(func() { e.Destroy(context.Background(), info.ID) })
	vm, err := e.getVM(info.ID)
	if err != nil {
		t.Fatal(err)
	}
	vm.mu.Lock()
	pid, uid, ctl, dir := vm.HelperPID, vm.vmmUID, vm.CtlSockUDS, vm.SandboxDir
	vm.mu.Unlock()

	root := os.Geteuid() == 0
	if root && uid == 0 {
		t.Fatal("a root daemon gave the helper no uid of its own")
	}
	tasks, err := os.ReadDir(fmt.Sprintf("/proc/%d/task", pid))
	if err != nil {
		t.Fatal(err)
	}
	// The Go runtime's threads, started before main, as much as libkrun's.
	if len(tasks) < 2 {
		t.Fatalf("helper has %d thread(s); checking them would prove little", len(tasks))
	}
	for _, task := range tasks {
		status, err := os.ReadFile(fmt.Sprintf("/proc/%d/task/%s/status", pid, task.Name()))
		if errors.Is(err, os.ErrNotExist) {
			continue // exited meanwhile
		}
		if err != nil {
			t.Fatal(err)
		}
		f := procStatus(status)
		if got := f["NoNewPrivs"]; len(got) != 1 || got[0] != "1" {
			t.Errorf("thread %s: NoNewPrivs %v", task.Name(), got)
		}
		for _, k := range []string{"CapEff", "CapPrm"} {
			if got := f[k]; len(got) != 1 || strings.Trim(got[0], "0") != "" {
				t.Errorf("thread %s: %s %v", task.Name(), k, got)
			}
		}
		if root {
			want := strconv.FormatUint(uint64(uid), 10)
			for _, k := range []string{"Uid", "Gid"} {
				if got := f[k]; len(got) != 4 || slices.ContainsFunc(got, func(id string) bool { return id != want }) {
					t.Errorf("thread %s: %s %v, want %s throughout", task.Name(), k, got, want)
				}
			}
		}
	}

	if !e.caps.Checkpoint {
		t.Log("this bhatti-vmm build can't SAVE; the write check needs it")
		return
	}
	outside, err := os.MkdirTemp("", "vmmconf-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(outside)
	if err := os.Chmod(outside, 0o777); err != nil { // only confinement can refuse the write
		t.Fatal(err)
	}
	escaped := filepath.Join(outside, "escaped")
	if _, err := controlCmd(ctx, ctl, "SAVE "+escaped); err == nil {
		t.Fatalf("SAVE into %s, outside the sandbox, succeeded", escaped)
	} else if !strings.Contains(strings.ToLower(err.Error()), "permission denied") {
		t.Fatalf("SAVE outside the sandbox failed, but not for want of permission: %v", err)
	}
	if _, err := os.Lstat(escaped); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the helper created %s (%v)", escaped, err)
	}
	inside := filepath.Join(dir, "save", "probe")
	if _, err := controlCmd(ctx, ctl, "SAVE "+inside); err != nil {
		t.Fatalf("SAVE into its own save dir: %v", err)
	}
	if _, err := controlCmd(ctx, ctl, "RESUME"); err != nil {
		t.Fatalf("RESUME: %v", err)
	}
	if r, err := e.Exec(ctx, info.ID, []string{"true"}); err != nil || r.ExitCode != 0 {
		t.Fatalf("guest after the SAVEs: err=%v exit=%d", err, r.ExitCode)
	}
}

// procStatus splits /proc/<pid>/status into its fields' values.
func procStatus(status []byte) map[string][]string {
	f := map[string][]string{}
	for _, l := range strings.Split(string(status), "\n") {
		if k, v, ok := strings.Cut(l, ":"); ok {
			f[k] = strings.Fields(v)
		}
	}
	return f
}
