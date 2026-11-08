package krucible

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

func TestRecoveryReclaimsPendingCreates(t *testing.T) {
	for _, kind := range []string{"dead", "strict-helper-through-alias", "foreign-process"} {
		t.Run(kind, func(t *testing.T) {
			e := &Engine{cfg: Config{DataDir: t.TempDir(), SocketDir: shortSockDir(t), VMMBinary: os.Args[0]},
				vms: map[string]*VM{}, netds: map[string]*netdInstance{}}
			spec, _ := sandboxPaths(t, e, "pending")
			writeFile(t, spec, []byte("{}"))
			disk := filepath.Join(filepath.Dir(spec), "root.qcow2")
			writeFile(t, disk, []byte("private disk"))
			pid, argv0, arg := 0, filepath.Base(os.Args[0]), spec
			if kind == "strict-helper-through-alias" {
				alias := filepath.Join(t.TempDir(), "data-alias")
				if err := os.Symlink(e.cfg.DataDir, alias); err != nil {
					t.Fatal(err)
				}
				arg = filepath.Join(alias, "sandboxes", "pending", "vmspec.json")
			}
			if kind == "foreign-process" {
				argv0 = "administrator-editor"
			}
			if kind != "dead" {
				pid = gatedStandinAs(t, arg, argv0)
			}
			recordSandbox(e, "pending", "stopped", pid)
			raw, err := os.ReadFile(stateFilePath(filepath.Dir(spec)))
			if err != nil {
				t.Fatal(err)
			}
			var record map[string]any
			if err := json.Unmarshal(raw, &record); err != nil {
				t.Fatal(err)
			}
			record["pending"] = true
			if err := writeDaemonMetadata(stateFilePath(filepath.Dir(spec)), record); err != nil {
				t.Fatal(err)
			}
			err = e.recover()
			if kind == "foreign-process" {
				if err == nil || !strings.Contains(err.Error(), "still has helper") {
					t.Fatalf("pending record concealed another spec user: %v", err)
				}
				if !running(pid, argv0, arg) {
					t.Fatal("recovery killed a process without strict helper identity")
				}
				if _, err := os.Stat(disk); err != nil {
					t.Fatalf("recovery removed another process's pending disk: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(filepath.Dir(spec)); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("unreturned create survived recovery: %v", err)
			}
			if pid != 0 && running(pid, argv0, arg) {
				t.Fatal("pending helper survived disk reclamation")
			}
			if len(e.vms) != 0 {
				t.Fatalf("pending create became a committed sandbox: %+v", e.vms)
			}
		})
	}
}

func TestCreateCrashLeavesPendingSafetyRecord(t *testing.T) {
	const childEnv = "BHATTI_TEST_PENDING_CREATE_DATA"
	if data := os.Getenv(childEnv); data != "" {
		e := &Engine{cfg: Config{DataDir: data, SocketDir: os.Getenv("BHATTI_TEST_PENDING_CREATE_SOCKETS"),
			BaseImage: filepath.Join(data, "images", "bases", "base.ext4"),
			VMMBinary: filepath.Join(data, "crash-vmm"), BlockRoot: true, DefaultVcpus: 1, DefaultMemMiB: 64},
			vms: map[string]*VM{}, netds: map[string]*netdInstance{}}
		_, err := e.Create(t.Context(), noNetworkSpec("interrupted"))
		t.Fatalf("create did not stop after its safety record: %v", err)
	}
	data := t.TempDir()
	base := filepath.Join(data, "images", "bases", "base.ext4")
	writeFile(t, base, make([]byte, 1<<20))
	binary := filepath.Join(data, "crash-vmm")
	writeFile(t, binary, []byte("#!/bin/sh\nkill -KILL \"$PPID\"\nexit 0\n"))
	if err := os.Chmod(binary, 0o755); err != nil {
		t.Fatal(err)
	}
	sockets := shortSockDir(t)
	cmd := exec.Command(os.Args[0], "-test.run=^TestCreateCrashLeavesPendingSafetyRecord$")
	cmd.Env = append(os.Environ(), childEnv+"="+data, "BHATTI_TEST_PENDING_CREATE_SOCKETS="+sockets)
	out, err := cmd.CombinedOutput()
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.Sys().(syscall.WaitStatus).Signal() != syscall.SIGKILL {
		t.Fatalf("create subprocess did not reach the helper's crash: %v: %s", err, out)
	}
	records, err := filepath.Glob(filepath.Join(data, "sandboxes", "*", stateFile))
	if err != nil || len(records) != 1 {
		t.Fatalf("crash did not leave exactly one safety record: %v, %v", records, err)
	}
	raw, err := os.ReadFile(records[0])
	if err != nil {
		t.Fatal(err)
	}
	var record map[string]any
	if err := json.Unmarshal(raw, &record); err != nil {
		t.Fatal(err)
	}
	if record["pending"] != true {
		t.Fatalf("pre-return safety record was committed: %s", raw)
	}
	e := &Engine{cfg: Config{DataDir: data, SocketDir: sockets, VMMBinary: binary},
		vms: map[string]*VM{}, netds: map[string]*netdInstance{}}
	if err := e.recover(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Dir(records[0])); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("crashed create's safety record pinned its disk: %v", err)
	}
}

func TestCompletedCreateClearsPendingSafetyRecord(t *testing.T) {
	e := fakeReseedEngine(t, "supported")
	info, err := e.Create(context.Background(), noNetworkSpec("complete"))
	if err != nil {
		t.Fatal(err)
	}
	vm, err := e.getVM(info.ID)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(stateFilePath(vm.SandboxDir))
	if err != nil {
		t.Fatal(err)
	}
	var record map[string]any
	if err := json.Unmarshal(raw, &record); err != nil {
		t.Fatal(err)
	}
	if record["pending"] == true {
		t.Fatalf("successful create retained its uncommitted record: %s", raw)
	}
}
