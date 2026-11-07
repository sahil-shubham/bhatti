package krucible

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/sahil-shubham/bhatti/pkg/launchgate"
)

// standinEnv makes this test binary stand in for a gated bhatti-vmm when the
// daemon starts it like one: `<binary> <spec>` with its gate in the
// environment. The process then has exactly the argv recovery recognises a
// helper by (isHelper), which no shell script or `go test` flag-parsed child
// can have. Its value says what it does once its gate opens: "vmm" plays the
// guest's side (fakeReseedVMM), "idle" waits, "hog:<MiB>" touches that much
// memory first, as a guest does as it boots, then waits.
const standinEnv = "BHATTI_KRUCIBLE_STANDIN"

// standinStarted is what a stand-in leaves in its sandbox dir the moment its
// gate opens: where its work begins. Absent means it hasn't started.
const standinStarted = "standin.started"

func TestMain(m *testing.M) {
	if mode := os.Getenv(standinEnv); mode != "" {
		if fd, err := launchgate.FromEnv(); err == nil && fd >= 0 && len(os.Args) == 2 {
			standinMain(mode, fd, os.Args[1])
		}
	}
	os.Exit(m.Run())
}

func standinMain(mode string, fd int, spec string) {
	if err := launchgate.Wait(fd, 30*time.Second); err != nil {
		fmt.Fprintln(os.Stderr, "standin:", err)
		os.Exit(3)
	}
	dir := filepath.Dir(spec)
	cgroup, _ := os.ReadFile("/proc/self/cgroup") // where it was when it began, on Linux
	_ = os.WriteFile(filepath.Join(dir, standinStarted), append([]byte(time.Now().Format(time.RFC3339Nano)+"\n"), cgroup...), 0o600)
	switch {
	case mode == "vmm":
		fakeReseedVMM(spec, func(err error) {
			fmt.Fprintln(os.Stderr, "standin:", err)
			os.Exit(1)
		})
	case strings.HasPrefix(mode, "hog:"):
		mib, _ := strconv.Atoi(strings.TrimPrefix(mode, "hog:"))
		mem := make([]byte, mib<<20)
		for i := 0; i < len(mem); i += 4096 {
			mem[i] = 1 // every page, so it is resident and charged
		}
		_ = os.WriteFile(filepath.Join(dir, "standin.allocated"), []byte(strconv.Itoa(mib)), 0o600)
		time.Sleep(time.Hour)
		runtime.KeepAlive(mem)
	}
	time.Sleep(time.Hour)
	os.Exit(0)
}

// gatedStandin starts this test binary as an idle stand-in for the configured
// helper, with argv [basename(binary) spec] and its gate opened. It returns
// its pid once it has begun and is killed when the test ends.
func gatedStandin(t *testing.T, spec string) int {
	t.Helper()
	return gatedStandinAs(t, spec, filepath.Base(os.Args[0]))
}

// gatedStandinAs also permits an unrelated process's argv without changing
// the executable, so helper identity tests can distinguish argv from exe.
func gatedStandinAs(t *testing.T, spec, argv0 string) int {
	t.Helper()
	return gatedStandinFrom(t, os.Args[0], spec, argv0)
}

// gatedStandinFrom runs the stand-in from exe, a copy of this test binary, so
// a test can move or delete the executable under a running helper, as an
// update does to the runtime.
func gatedStandinFrom(t *testing.T, exe, spec, argv0 string) int {
	t.Helper()
	g, err := launchgate.New()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(g.Close)
	cmd := exec.Command(exe, spec)
	cmd.Args[0] = argv0
	cmd.Env = append(os.Environ(), standinEnv+"=idle")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := g.Start(cmd); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { reap(t, cmd) })
	if err := g.Open(); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the stand-in to start", func() bool { return exists(filepath.Join(filepath.Dir(spec), standinStarted)) })
	execd(t, cmd.Process.Pid, argv0, spec)
	return cmd.Process.Pid
}
