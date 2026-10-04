package krucible

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sahil-shubham/bhatti/pkg/agent/proto"
	"github.com/sahil-shubham/bhatti/pkg/engine"
	"github.com/sahil-shubham/bhatti/pkg/engine/enginetest"
	"github.com/sahil-shubham/bhatti/pkg/gateway"
)

// ensureVMMSigned keeps the dev loop robust on darwin: HVF requires bhatti-vmm
// to carry the hypervisor entitlement, but a plain `go build -o bhatti-vmm`
// (instead of `make vmm`) strips the ad-hoc signature, so hv_vm_create then
// fails for EVERY VM (VmSetup(VmCreate)) — an easy, silent way to lose an hour.
// Re-apply it here (idempotent, ~50ms) so tests pass regardless of how the
// binary was built. No-op off darwin (Linux/KVM needs no signing).
func ensureVMMSigned(t *testing.T, vmm string) {
	if runtime.GOOS != "darwin" {
		return
	}
	ent := filepath.Join(repoRoot(t), "cmd", "vmm", "hvf-entitlements.plist")
	if out, err := exec.Command("codesign", "--force", "--entitlements", ent, "-s", "-", vmm).CombinedOutput(); err != nil {
		t.Logf("ensureVMMSigned: codesign %s failed (%v): %s", vmm, err, out)
	}
}

// leanKernel returns the lean guest kernel the VM tests boot:
// KRUCIBLE_LEAN_KERNEL, else the build output under dist/kernel.
func leanKernel(repo string) string {
	if k := os.Getenv("KRUCIBLE_LEAN_KERNEL"); k != "" {
		return k
	}
	karch := map[string]string{"arm64": "aarch64", "amd64": "x86_64"}[runtime.GOARCH]
	for _, pat := range []string{"Image-lean-*-" + karch, "vmlinux-lean-*-" + karch} {
		if m, _ := filepath.Glob(filepath.Join(repo, "dist", "kernel", pat)); len(m) > 0 {
			return m[0]
		}
	}
	return ""
}

// shortSockDir returns a SHORT temp dir for vsock/UDS paths. t.TempDir() on macOS
// (/var/folders/...) exceeds the ~104-byte sockaddr_un limit, so sockets need a
// short base like /tmp. (DataDir can stay t.TempDir() — regular files, no limit.)
func shortSockDir(t *testing.T) string {
	d, err := os.MkdirTemp("/tmp", "kr")
	if err != nil {
		t.Fatalf("short sock dir: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(d) })
	return d
}

// requireLeanKernel returns leanKernel(repo), skipping the test if there is none.
func requireLeanKernel(t *testing.T, repo string) string {
	t.Helper()
	k := leanKernel(repo)
	if k == "" {
		t.Skip("no lean kernel: set KRUCIBLE_LEAN_KERNEL or build dist/kernel; skipping")
	}
	return k
}

// requireNetd returns the built bhatti-netd, skipping the test if there is none.
func requireNetd(t *testing.T, repo string) string {
	t.Helper()
	netd := filepath.Join(repo, "bhatti-netd")
	if _, err := os.Stat(netd); err != nil {
		t.Skip("bhatti-netd not built (go build -o bhatti-netd ./cmd/bhatti-netd); skipping")
	}
	return netd
}

// newBlockRootEngine builds a krucible engine for the VM suites: a qcow2 block
// root over a base built from a test rootfs, booting the lean kernel. It
// self-skips when libkrun, a hypervisor, mke2fs, bhatti-vmm or the kernel is
// missing, so `go test ./...` stays green on hosts that can't run VMs.
func newBlockRootEngine(t *testing.T) engine.Engine {
	repo := repoRoot(t)
	if !hasLibkrun() {
		t.Skip("libkrun not installed (pkg-config libkrun); skipping")
	}
	if !hasHypervisor() {
		t.Skip("no hypervisor (/dev/kvm or HVF); skipping VM suite")
	}
	if _, err := exec.LookPath("mke2fs"); err != nil {
		t.Skip("mke2fs not found (e2fsprogs); skipping block-root suite")
	}
	vmm := filepath.Join(repo, "bhatti-vmm")
	if _, err := os.Stat(vmm); err != nil {
		t.Skip("bhatti-vmm not built — run `make vmm`; skipping")
	}
	kernel := requireLeanKernel(t, repo)
	ensureVMMSigned(t, vmm)
	eng, err := New(Config{
		DataDir:     t.TempDir(),
		BaseRootfs:  buildBaseRootfs(t, repo),
		VMMBinary:   vmm,
		LibDir:      libDir(),
		BlockRoot:   true,
		KernelImage: kernel,
		NetdBinary:  requireNetd(t, repo),
		SocketDir:   shortSockDir(t),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return eng
}

// newPauseEngine is newBlockRootEngine for warm-tier tests; it skips when the
// bhatti-vmm build can't pause a VM.
func newPauseEngine(t *testing.T) engine.Engine {
	eng := newBlockRootEngine(t)
	if !eng.(*Engine).caps.Pause {
		t.Skip("bhatti-vmm build can't pause; skipping")
	}
	return eng
}

// newCheckpointEngine gates tests that save or restore guest memory.
func newCheckpointEngine(t *testing.T) engine.Engine {
	eng := newBlockRootEngine(t)
	if !eng.(*Engine).caps.Checkpoint {
		t.Skip("bhatti-vmm build has no checkpoint support; skipping")
	}
	return eng
}

// TestKrucibleAgentSuite runs the shared VMM-agnostic behavior suite against the
// krucible engine — the parity gate (the same suite is meant to pass on FC).
func TestKrucibleAgentSuite(t *testing.T) {
	enginetest.RunAgentSuite(t, newBlockRootEngine)
}

// TestKrucibleThermalSuite asserts hot/warm transitions.
func TestKrucibleThermalSuite(t *testing.T) {
	enginetest.RunThermalSuite(t, newPauseEngine)
}

// TestKrucibleSnapshotSuite exercises a named checkpoint/restore into a new
// sandbox, keeping the original running.
func TestKrucibleSnapshotSuite(t *testing.T) {
	enginetest.RunSnapshotSuite(t, newCheckpointEngine)
}

// TestKrucibleReliabilitySuite exercises repeated power-off/reboot and
// concurrent cold wakeups, independent of checkpoint availability.
func TestKrucibleReliabilitySuite(t *testing.T) {
	enginetest.RunReliabilitySuite(t, newBlockRootEngine)
}

// TestKrucibleStopPowersOff pins that Stop always discards RAM and Start boots
// fresh from the persisted root disk, even when checkpointing is available.
func TestKrucibleStopPowersOff(t *testing.T) {
	eng := newBlockRootEngine(t)
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	info, err := eng.Create(ctx, engine.SandboxSpec{Name: "poweroff", CPUs: 1, MemoryMB: 512,
		NetPolicy: &gateway.NetPolicyWire{Default: gateway.PostureNone}})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	id := info.ID
	t.Cleanup(func() { eng.Destroy(context.Background(), id) })

	run := func(argv ...string) string {
		t.Helper()
		r, err := eng.Exec(ctx, id, argv)
		if err != nil || r.ExitCode != 0 {
			t.Fatalf("exec %v: err=%v exit=%d", argv, err, r.ExitCode)
		}
		return strings.TrimSpace(r.Stdout)
	}
	bootBefore := run("cat", "/proc/sys/kernel/random/boot_id")
	run("writeuid", "/workspace/on-disk")
	run("writeuid", "/tmp/in-ram")

	if err := eng.Stop(ctx, id); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if err := eng.Start(ctx, id); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if got := run("cat", "/proc/sys/kernel/random/boot_id"); got == bootBefore {
		t.Fatal("same boot id after Stop/Start: the VM wasn't powered off")
	}
	if got := run("cat", "/workspace/on-disk"); got != "1000" {
		t.Fatalf("file on the root disk after power-off = %q, want 1000", got)
	}
	if got := run("cat", "/tmp/in-ram"); got != "" {
		t.Fatalf("tmpfs file survived a power-off: %q", got)
	}
}

// TestKrucibleStopIgnoresCheckpointCapability exercises the cold transition
// without a hypervisor: a checkpoint-capable engine must still terminate its
// helper rather than try to PAUSE/save to a control socket.
func TestKrucibleStopIgnoresCheckpointCapability(t *testing.T) {
	cmd := exec.Command("sleep", "30")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	t.Cleanup(func() { _ = cmd.Process.Kill() })

	id := "stop-capability"
	vm := &VM{
		ID: id, Status: "running", Thermal: "hot", SandboxDir: t.TempDir(),
		cmd: cmd, waitDone: done, HelperPID: cmd.Process.Pid,
	}
	e := &Engine{
		caps: VMMCapabilities{Checkpoint: true},
		vms:  map[string]*VM{id: vm},
	}
	if err := e.Stop(context.Background(), id); err != nil {
		t.Fatalf("Stop with checkpoint support: %v", err)
	}
	if vm.Status != "stopped" || vm.Thermal != "cold" || vm.HelperPID != 0 || vm.cmd != nil {
		t.Fatalf("power-off did not clear helper: status=%q thermal=%q pid=%d cmd=%v",
			vm.Status, vm.Thermal, vm.HelperPID, vm.cmd)
	}
	if pidAlive(cmd.Process.Pid) {
		t.Fatalf("helper pid %d survived Stop", cmd.Process.Pid)
	}
}

// --- test helpers ---

func repoRoot(t *testing.T) string {
	t.Helper()
	abs, err := filepath.Abs("../../..")
	if err != nil {
		t.Fatalf("repo root: %v", err)
	}
	return abs
}

func hasLibkrun() bool {
	return exec.Command("pkg-config", "--exists", "libkrun").Run() == nil
}

// hasHypervisor reports whether a usable hypervisor is present, so the VM suites
// skip (rather than fail) on hosts that have libkrun + bhatti-vmm built but no
// accelerator — e.g. a GitHub-hosted CI runner with no /dev/kvm. On linux we
// require an openable /dev/kvm (KVM); on darwin HVF is always available on the
// supported hardware (the entitlement/codesign is the real gate, enforced when
// the helper launches). The VM integration suites run on the self-hosted KVM
// cluster / a dev Mac; the build job (no accelerator) compiles everything and
// runs the pure-unit tests, with the VM suites skipping here.
func hasHypervisor() bool {
	switch runtime.GOOS {
	case "linux":
		f, err := os.OpenFile("/dev/kvm", os.O_RDWR, 0)
		if err != nil {
			return false
		}
		_ = f.Close()
		return true
	case "darwin":
		return true
	default:
		return false
	}
}

// libDir returns the libkrucible build prefix's library dir (lib64 on Linux,
// lib on macOS), passed to the helper's DYLD_FALLBACK_LIBRARY_PATH /
// LD_LIBRARY_PATH.
func libDir() string {
	for _, sub := range []string{"lib64", "lib"} {
		if p, err := filepath.Abs("../../../libkrucible/_install/" + sub); err == nil {
			if m, _ := filepath.Glob(filepath.Join(p, "libkrun.*")); len(m) > 0 {
				return p
			}
		}
	}
	return ""
}

// buildBaseRootfs cross-compiles lohar to <root>/init.krun and a tiny multi-call
// util (true/echo) to <root>/bin, plus the mountpoints lohar mounts over.
func buildBaseRootfs(t *testing.T, repo string) string {
	t.Helper()
	root := t.TempDir()
	// "workspace" is init's working dir (runInitSession sets cmd.Dir=/workspace);
	// without it `sh -c` chdir fails and --init never runs.
	for _, d := range []string{"bin", "proc", "sys", "dev/pts", "tmp", "run", "etc", "root", "workspace", "usr/local/bin"} {
		if err := os.MkdirAll(filepath.Join(root, d), 0755); err != nil {
			t.Fatal(err)
		}
	}
	// The guest user (uid 1000) writes to /workspace; the image's files are root's.
	if err := os.Chmod(filepath.Join(root, "workspace"), 0o777); err != nil {
		t.Fatal(err)
	}
	guestArch := runtime.GOARCH // HVF/KVM: guest arch == host arch

	// lohar -> /init.krun
	loharBuild := exec.Command("go", "build", "-o", filepath.Join(root, "init.krun"), "./cmd/lohar")
	loharBuild.Dir = repo
	loharBuild.Env = append(os.Environ(), "GOOS=linux", "GOARCH="+guestArch, "CGO_ENABLED=0")
	if out, err := loharBuild.CombinedOutput(); err != nil {
		t.Fatalf("build lohar: %v\n%s", err, out)
	}

	// tiny multi-call util (true/echo) -> /bin/{true,echo}
	utilSrc := t.TempDir()
	if err := os.WriteFile(filepath.Join(utilSrc, "go.mod"), []byte("module mu\ngo 1.21\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(utilSrc, "main.go"), []byte(miniutilSrc), 0644); err != nil {
		t.Fatal(err)
	}
	util := filepath.Join(root, "bin", "true")
	utilBuild := exec.Command("go", "build", "-o", util, ".")
	utilBuild.Dir = utilSrc
	utilBuild.Env = append(os.Environ(), "GOOS=linux", "GOARCH="+guestArch, "CGO_ENABLED=0")
	if out, err := utilBuild.CombinedOutput(); err != nil {
		t.Fatalf("build miniutil: %v\n%s", err, out)
	}
	// sh: init runs `sh -c <script>`; our multi-call util handles the -c form by
	// splitting on whitespace and dispatching in-process (no real shell needed).
	// writeuid: writes the caller's uid to a file — lets a test observe that
	// --init (and exec) ran as uid 1000 without a full userland.
	for _, n := range []string{"echo", "errcho", "false", "sleep", "printenv", "cat", "sync", "sh", "writeuid", "fsbytes", "date", "oncpu"} {
		if err := os.Symlink("true", filepath.Join(root, "bin", n)); err != nil {
			t.Fatal(err)
		}
	}

	// netcheck -> /bin/netcheck (TSI egress prober + tiny HTTP server)
	ncSrc := t.TempDir()
	if err := os.WriteFile(filepath.Join(ncSrc, "go.mod"), []byte("module nc\ngo 1.21\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ncSrc, "main.go"), []byte(netcheckSrc), 0644); err != nil {
		t.Fatal(err)
	}
	ncBuild := exec.Command("go", "build", "-o", filepath.Join(root, "bin", "netcheck"), ".")
	ncBuild.Dir = ncSrc
	ncBuild.Env = append(os.Environ(), "GOOS=linux", "GOARCH="+guestArch, "CGO_ENABLED=0")
	if out, err := ncBuild.CombinedOutput(); err != nil {
		t.Fatalf("build netcheck: %v\n%s", err, out)
	}
	return root
}

const miniutilSrc = `package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unsafe"
)

func main() {
	name := filepath.Base(os.Args[0])
	args := os.Args[1:]
	// sh -c "<cmd>": init runs 'sh -c <script>'. No real shell in this rootfs, so
	// split the script on whitespace and dispatch back into the multi-call switch.
	if name == "sh" && len(args) >= 2 && args[0] == "-c" {
		fields := strings.Fields(args[1])
		if len(fields) == 0 {
			return
		}
		name, args = fields[0], fields[1:]
	}
	dispatch(name, args)
}

func dispatch(name string, args []string) {
	switch name {
	case "sync": // flush the guest page cache to the block device
		syscall.Sync()
	case "echo":
		fmt.Println(strings.Join(args, " "))
	case "errcho":
		fmt.Fprintln(os.Stderr, strings.Join(args, " "))
	case "false":
		os.Exit(1)
	case "sleep":
		if len(args) > 0 {
			n, _ := strconv.Atoi(args[0])
			time.Sleep(time.Duration(n) * time.Second)
		}
	case "printenv": // printenv KEY -> value of os.Getenv(KEY)
		if len(args) > 0 {
			fmt.Println(os.Getenv(args[0]))
		}
	case "cat": // cat FILE -> contents (os.ReadFile handles 0-size procfs files)
		if len(args) > 0 {
			b, _ := os.ReadFile(args[0])
			os.Stdout.Write(b)
		}
	case "writeuid": // writeuid PATH -> write the caller's uid to PATH (observe --init/exec uid)
		if len(args) > 0 {
			os.WriteFile(args[0], []byte(strconv.Itoa(os.Getuid())), 0644)
		}
	case "date": // high-resolution wall clock for restore-gap tests
		fmt.Println(time.Now().UnixNano())
	case "oncpu": // exercise a specific online vCPU after restoring its state
		cpu, err := strconv.Atoi(args[0])
		if err != nil || cpu < 0 || cpu >= 1024 {
			os.Exit(2)
		}
		var mask [128]byte
		mask[cpu/8] = 1 << uint(cpu%8)
		_, _, errno := syscall.RawSyscall(syscall.SYS_SCHED_SETAFFINITY, 0, uintptr(len(mask)), uintptr(unsafe.Pointer(&mask[0])))
		if errno != 0 {
			fmt.Fprintln(os.Stderr, errno)
			os.Exit(1)
		}
		until := time.Now().Add(100 * time.Millisecond)
		for time.Now().Before(until) {}
		fmt.Println(cpu)
	case "fsbytes": // fsbytes PATH -> total bytes of the filesystem holding PATH
		var st syscall.Statfs_t
		if len(args) > 0 && syscall.Statfs(args[0], &st) == nil {
			fmt.Println(uint64(st.Blocks) * uint64(st.Bsize))
		}
	default: // true
	}
}
`

const netcheckSrc = `package main

import (
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"time"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Println("usage: netcheck MODE")
		os.Exit(2)
	}
	switch os.Args[1] {
	case "tcp":
		c, err := net.DialTimeout("tcp", "1.1.1.1:443", 5*time.Second)
		if err != nil {
			fmt.Println("ERR", err)
			os.Exit(1)
		}
		c.Close()
		fmt.Println("OK tcp 1.1.1.1:443")
	case "dns":
		ips, err := net.LookupHost("example.com")
		if err != nil {
			fmt.Println("ERR", err)
			os.Exit(1)
		}
		fmt.Println("OK dns", ips)
	case "http":
		cl := &http.Client{Timeout: 8 * time.Second}
		r, err := cl.Get("http://example.com")
		if err != nil {
			fmt.Println("ERR", err)
			os.Exit(1)
		}
		r.Body.Close()
		fmt.Println("OK http", r.Status)
	case "dial": // dial ADDR -> exit 0 if a TCP connect succeeds within 3s, else 1.
		// Used to assert isolation: the guest must NOT reach a host-only service.
		if len(os.Args) < 3 {
			fmt.Println("usage: netcheck dial ADDR")
			os.Exit(2)
		}
		c, err := net.DialTimeout("tcp", os.Args[2], 3*time.Second)
		if err != nil {
			fmt.Println("ERR", err)
			os.Exit(1)
		}
		c.Close()
		fmt.Println("OK dial", os.Args[2])
	case "serve":
		http.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
			io.WriteString(w, "hello-from-guest\n")
		})
		http.ListenAndServe("0.0.0.0:"+os.Args[2], nil)
	case "localip": // print the first non-loopback IPv4 on the box (the guest's eth0 address).
		addrs, _ := net.InterfaceAddrs()
		for _, a := range addrs {
			if ipnet, ok := a.(*net.IPNet); ok && ipnet.IP.To4() != nil && !ipnet.IP.IsLoopback() {
				fmt.Println(ipnet.IP.To4().String())
				os.Exit(0)
			}
		}
		os.Exit(1)
	}
}
`

// TestKruciblePipedStderrStream: a piped session asked to keep stderr apart
// delivers it as STDERR frames; without the flag it arrives merged into
// STDOUT, as before. EXIT comes only after stderr has drained.
func TestKruciblePipedStderrStream(t *testing.T) {
	eng := newBlockRootEngine(t)
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	info, err := eng.Create(ctx, engine.SandboxSpec{Name: "piped", CPUs: 1, MemoryMB: 512,
		NetPolicy: &gateway.NetPolicyWire{Default: gateway.PostureNone}})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	t.Cleanup(func() { eng.Destroy(context.Background(), info.ID) })
	pe := eng.(engine.PipedSessionEngine)

	collect := func(separate bool) (stdout, stderr string) {
		t.Helper()
		_, pc, err := pe.PipedSession(ctx, info.ID, engine.PipedSpec{Cmd: []string{"errcho", "to-stderr"}, Stderr: separate})
		if err != nil {
			t.Fatalf("PipedSession: %v", err)
		}
		defer pc.Close()
		for {
			typ, payload, err := pc.ReadFrame()
			if err != nil {
				t.Fatalf("ReadFrame: %v (stdout=%q stderr=%q)", err, stdout, stderr)
			}
			switch typ {
			case proto.STDOUT:
				stdout += string(payload)
			case proto.STDERR:
				stderr += string(payload)
			case proto.EXIT:
				return stdout, stderr
			}
		}
	}
	if out, errs := collect(true); strings.TrimSpace(errs) != "to-stderr" || out != "" {
		t.Fatalf("separate: stdout=%q stderr=%q, want stderr only", out, errs)
	}
	if out, errs := collect(false); strings.TrimSpace(out) != "to-stderr" || errs != "" {
		t.Fatalf("merged: stdout=%q stderr=%q, want stdout only", out, errs)
	}
}

// TestKrucibleDiskSize: --disk-size larger than the image gives the sandbox a
// root filesystem of about that size (the overlay is created at that size and
// lohar grows ext4 online at boot).
func TestKrucibleDiskSize(t *testing.T) {
	eng := newBlockRootEngine(t)
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	const diskMB = 2048
	info, err := eng.Create(ctx, engine.SandboxSpec{Name: "disk", CPUs: 1, MemoryMB: 512, DiskSizeMB: diskMB,
		NetPolicy: &gateway.NetPolicyWire{Default: gateway.PostureNone}})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	t.Cleanup(func() { eng.Destroy(context.Background(), info.ID) })
	r, err := eng.Exec(ctx, info.ID, []string{"fsbytes", "/"})
	if err != nil || r.ExitCode != 0 {
		t.Fatalf("fsbytes: err=%v exit=%d", err, r.ExitCode)
	}
	got, _ := strconv.ParseUint(strings.TrimSpace(r.Stdout), 10, 64)
	// ext4 metadata takes a few percent; a filesystem that wasn't grown is the
	// test image's size, far below this.
	if want := uint64(diskMB) << 20 * 9 / 10; got < want {
		t.Fatalf("root filesystem is %d MiB, want about %d MiB", got>>20, diskMB)
	}
}
