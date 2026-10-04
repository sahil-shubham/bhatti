//go:build linux && cgo

package confine

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"unsafe"

	"golang.org/x/sys/unix"

	"github.com/sahil-shubham/bhatti/pkg/engine/krucible"
)

// The constructor confines the test binary itself as it starts, so each test
// runs the binary again with a policy and the child (TestMain, before any test
// runs) checks what it can and can't do: Landlock can't be undone in-process.

const childEnv = "CONFINE_TEST_CHILD"

// expect is what the confined child checks.
type expect struct {
	UID        uint32 // dropped to; 0 = kept the parent's (non-root parent)
	CapEff     uint64 // effective (and permitted) capabilities, every thread
	Writable   string // dir it can create a file in
	Unwritable string // dir it can't, though the mode would let it
	Readable   string // file it can read
	Unreadable string // file it can't, though the mode would let it
	TCP        string // a listener it can't connect to (Landlock ABI >= 4)
	// SwitchUID: switching the thread's euid away and back (as libkrun's
	// virtio-fs server does per request) leaves its capabilities in place.
	SwitchUID bool
}

func TestMain(m *testing.M) {
	if e := os.Getenv(childEnv); e != "" {
		var want expect
		if err := json.Unmarshal([]byte(e), &want); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(2)
		}
		if errs := check(want); len(errs) > 0 {
			fmt.Fprintln(os.Stderr, strings.Join(errs, "\n"))
			os.Exit(1)
		}
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func check(want expect) (errs []string) {
	fail := func(format string, a ...any) { errs = append(errs, fmt.Sprintf(format, a...)) }

	// Every thread, including those the Go runtime started before main: one
	// left unconfined would share the address space with the confined ones.
	tasks, err := os.ReadDir("/proc/self/task")
	if err != nil {
		fail("list threads: %v", err)
	}
	for _, task := range tasks {
		status, err := os.ReadFile(filepath.Join("/proc/self/task", task.Name(), "status"))
		if err != nil {
			fail("thread %s: %v", task.Name(), err)
			continue
		}
		f := statusFields(string(status))
		if f["NoNewPrivs"] != "1" {
			fail("thread %s: NoNewPrivs %q", task.Name(), f["NoNewPrivs"])
		}
		for _, k := range []string{"CapEff", "CapPrm"} {
			if got, _ := strconv.ParseUint(f[k], 16, 64); got != want.CapEff {
				fail("thread %s: %s %x, want %x", task.Name(), k, got, want.CapEff)
			}
		}
		if want.UID != 0 {
			id := strconv.FormatUint(uint64(want.UID), 10)
			for _, k := range []string{"Uid", "Gid"} {
				if got := strings.Join(strings.Fields(f[k+"s"]), " "); got != strings.Repeat(id+" ", 3)+id {
					fail("thread %s: %s %q, want %s everywhere", task.Name(), k, got, id)
				}
			}
		}
	}
	if len(tasks) < 2 {
		fail("only %d thread(s): the check proves nothing about the runtime's own", len(tasks))
	}

	if err := os.WriteFile(filepath.Join(want.Writable, "ok"), nil, 0o600); err != nil {
		fail("write where allowed: %v", err)
	}
	if err := os.WriteFile(filepath.Join(want.Unwritable, "escaped"), nil, 0o600); !errors.Is(err, os.ErrPermission) {
		fail("write outside the policy: err=%v, want permission denied", err)
	}
	if _, err := os.ReadFile(want.Readable); err != nil {
		fail("read where allowed: %v", err)
	}
	if _, err := os.ReadFile(want.Unreadable); !errors.Is(err, os.ErrPermission) {
		fail("read outside the policy: err=%v, want permission denied", err)
	}
	if abi, _, _ := unix.Syscall(unix.SYS_LANDLOCK_CREATE_RULESET, 0, 0, unix.LANDLOCK_CREATE_RULESET_VERSION); abi >= 4 {
		if c, err := net.Dial("tcp", want.TCP); err == nil {
			c.Close()
			fail("TCP connect to %s allowed", want.TCP)
		}
	}
	const capSetuid = 7
	if want.UID != 0 && want.CapEff&(1<<capSetuid) == 0 {
		if _, _, errno := unix.RawSyscall(unix.SYS_SETRESUID, 0, 0, 0); errno == 0 {
			fail("setresuid(0) succeeded")
		}
	}
	if want.SwitchUID {
		// To euid 0 and back is the switch the kernel would otherwise answer
		// by clearing the effective set.
		runtime.LockOSThread()
		for _, uid := range []uint32{0, want.UID} {
			if _, _, errno := unix.RawSyscall(unix.SYS_SETRESUID, ^uintptr(0), uintptr(uid), ^uintptr(0)); errno != 0 {
				fail("switch euid to %d: %v", uid, errno)
			}
		}
		if eff := threadCapEff(); eff != want.CapEff {
			fail("after an euid round trip CapEff %x, want %x", eff, want.CapEff)
		}
	}
	return errs
}

func statusFields(status string) map[string]string {
	f := map[string]string{}
	for _, l := range strings.Split(status, "\n") {
		if k, v, ok := strings.Cut(l, ":"); ok {
			v = strings.TrimSpace(v)
			if k == "Uid" || k == "Gid" {
				f[k+"s"] = v
				continue
			}
			if fields := strings.Fields(v); len(fields) > 0 {
				f[k] = fields[0]
			}
		}
	}
	return f
}

func threadCapEff() uint64 {
	hdr := unix.CapUserHeader{Version: unix.LINUX_CAPABILITY_VERSION_3}
	var data [2]unix.CapUserData
	if _, _, errno := unix.RawSyscall(unix.SYS_CAPGET, uintptr(unsafe.Pointer(&hdr)), uintptr(unsafe.Pointer(&data[0])), 0); errno != 0 {
		return ^uint64(0)
	}
	return uint64(data[1].Effective)<<32 | uint64(data[0].Effective)
}

// fixture lays out what the child is allowed and denied, reachable by an
// unprivileged uid when the test runs as root.
type fixture struct {
	allowedDir, deniedDir   string
	allowedFile, deniedFile string
	tcp                     string
}

func newFixture(t *testing.T, fileMode os.FileMode) fixture {
	t.Helper()
	root := t.TempDir()
	if err := os.Chmod(filepath.Dir(root), 0o711); err != nil { // t.TempDir's private parent
		t.Fatal(err)
	}
	f := fixture{
		allowedDir: filepath.Join(root, "allowed"), deniedDir: filepath.Join(root, "denied"),
	}
	for _, d := range []string{root, f.allowedDir, f.deniedDir} {
		if err := os.MkdirAll(d, 0o777); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(d, 0o777); err != nil { // the mode allows; only Landlock can deny
			t.Fatal(err)
		}
	}
	f.allowedFile = filepath.Join(f.allowedDir, "readable")
	f.deniedFile = filepath.Join(f.deniedDir, "unreadable")
	for _, p := range []string{f.allowedFile, f.deniedFile} {
		if err := os.WriteFile(p, []byte("x"), fileMode); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(p, fileMode); err != nil {
			t.Fatal(err)
		}
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	f.tcp = ln.Addr().String()
	return f
}

func (f fixture) rules() []krucible.VMMRule {
	return []krucible.VMMRule{
		{Path: f.allowedDir, Access: krucible.LandlockReadFile | krucible.LandlockReadDir |
			krucible.LandlockWriteFile | krucible.LandlockMakeReg | krucible.LandlockTruncate},
		{Path: "/proc", Access: krucible.LandlockReadFile | krucible.LandlockReadDir},
	}
}

// runConfined runs the test binary under policy and returns its output and
// exit error.
func runConfined(t *testing.T, policy krucible.VMMPolicy, want expect) ([]byte, error) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "policy")
	if err := os.WriteFile(path, policy.Encode(), 0o600); err != nil {
		t.Fatal(err)
	}
	w, _ := json.Marshal(want)
	cmd := exec.Command(os.Args[0])
	cmd.Env = append(os.Environ(), krucible.VMMPolicyEnv+"="+path, childEnv+"="+string(w))
	return cmd.CombinedOutput()
}

// testUID is the unprivileged identity the root-run cases drop to: in the
// range the engine gives sandboxes, so nothing else on the host owns it.
const testUID = 0x70000000 + 0xfffff

// TestConfinedProcess: started under a policy, the process — every thread of
// it — runs with no_new_privs, as the policy's uid with no capabilities when
// started as root, and can write and read only where the policy says, however
// permissive the file modes are; it can't open TCP connections.
func TestConfinedProcess(t *testing.T) {
	f := newFixture(t, 0o644)
	policy := krucible.VMMPolicy{Rules: f.rules()}
	want := expect{Writable: f.allowedDir, Unwritable: f.deniedDir, Readable: f.allowedFile, Unreadable: f.deniedFile, TCP: f.tcp}
	if os.Geteuid() == 0 {
		policy.UID, policy.GID = testUID, testUID
		want.UID = testUID
	} else if eff := threadCapEff(); eff != 0 {
		t.Skipf("non-root test process with capabilities %x", eff)
	}
	out, err := runConfined(t, policy, want)
	if err != nil {
		if strings.Contains(string(out), "Landlock unavailable") {
			t.Skip("kernel without Landlock")
		}
		t.Fatalf("confined child: %v\n%s", err, out)
	}
}

// TestConfinedProcessKeepsCaps: capabilities the policy keeps (a sandbox with a
// virtio-fs mount keeps a few for its file server) survive the drop to the
// unprivileged uid and the server's per-request euid switches, and still
// stop at the policy's paths.
func TestConfinedProcessKeepsCaps(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("dropping identity needs root")
	}
	f := newFixture(t, 0o600) // root-owned, unreadable without CAP_DAC_OVERRIDE
	const dacOverride, setuid = 1, 7
	policy := krucible.VMMPolicy{UID: testUID, GID: testUID, Caps: []uint{dacOverride, setuid}, Rules: f.rules()}
	want := expect{UID: testUID, CapEff: 1<<dacOverride | 1<<setuid, Writable: f.allowedDir, Unwritable: f.deniedDir,
		Readable: f.allowedFile, Unreadable: f.deniedFile, TCP: f.tcp, SwitchUID: true}
	if out, err := runConfined(t, policy, want); err != nil {
		t.Fatalf("confined child: %v\n%s", err, out)
	}
}

// TestConfineRefusesToStayRoot: a helper started as root whose policy names no
// identity to drop to exits instead of running as root.
func TestConfineRefusesToStayRoot(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("needs root")
	}
	f := newFixture(t, 0o644)
	out, err := runConfined(t, krucible.VMMPolicy{Rules: f.rules()}, expect{})
	var ee *exec.ExitError
	if !errors.As(err, &ee) || !strings.Contains(string(out), "started as root without an identity") {
		t.Fatalf("child ran as root: err=%v\n%s", err, out)
	}
}
