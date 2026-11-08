package krucible

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/sahil-shubham/bhatti/pkg/launchgate"
)

func TestRecoveryPreservesRelativeSpecFromAnotherWorkingDirectory(t *testing.T) {
	working := t.TempDir()
	e := &Engine{cfg: Config{DataDir: filepath.Join(working, "actual-data"), SocketDir: shortSockDir(t), VMMBinary: os.Args[0]},
		vms: map[string]*VM{}, netds: map[string]*netdInstance{}}
	spec, _ := sandboxPaths(t, e, "relative-helper")
	writeFile(t, spec, []byte("{}"))
	disk := filepath.Join(filepath.Dir(spec), "root.qcow2")
	writeFile(t, disk, []byte("live disk"))
	relative, err := filepath.Rel(working, spec)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(relative); err == nil {
		t.Fatal("fixture's relative argument unexpectedly resolves from the daemon working directory")
	}
	gate, err := launchgate.New()
	if err != nil {
		t.Fatal(err)
	}
	defer gate.Close()
	cmd := exec.Command(os.Args[0], relative)
	cmd.Dir = working
	cmd.Args[0] = "historical-helper"
	cmd.Env = append(os.Environ(), standinEnv+"=idle")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := gate.Start(cmd); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { reap(t, cmd) })
	if err := gate.Open(); err != nil {
		t.Fatal(err)
	}
	execd(t, cmd.Process.Pid, cmd.Args...)
	if err := e.recover(); err == nil || !strings.Contains(err.Error(), "still has helper") {
		t.Fatalf("live relative spec did not prevent reclamation: %v", err)
	}
	if data, err := os.ReadFile(disk); err != nil || string(data) != "live disk" {
		t.Fatalf("recovery removed a relative helper's disk: %q, %v", data, err)
	}
	if !running(cmd.Process.Pid, cmd.Args...) {
		t.Fatal("ambiguous relative helper reference was killed")
	}
}

func TestNewMakesDataDirAbsolute(t *testing.T) {
	f := newNodeFixture(t)
	cfg := storageStartupConfig(t, f.data, f.base)
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	cfg.DataDir, err = filepath.Rel(cwd, f.data)
	if err != nil {
		t.Fatal(err)
	}
	e, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if !filepath.IsAbs(e.cfg.DataDir) || e.cfg.DataDir != f.data {
		t.Fatalf("New retained a relative launch directory: %q, want %q", e.cfg.DataDir, f.data)
	}
}
