package krucible

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/sahil-shubham/bhatti/pkg/engine"
	"github.com/sahil-shubham/bhatti/pkg/engine/enginetest"
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

// newSuiteEngine builds a krucible engine for the shared enginetest suite,
// self-skipping if libkrun / bhatti-vmm aren't available (so `go test ./...`
// stays green on hosts without libkrun). Build the helper with `make vmm`.
func newSuiteEngine(t *testing.T) engine.Engine {
	repo := repoRoot(t)
	if !hasLibkrun() {
		t.Skip("libkrun not installed (pkg-config libkrun); skipping")
	}
	if !hasHypervisor() {
		t.Skip("no hypervisor (/dev/kvm or HVF); skipping VM suite")
	}
	vmm := filepath.Join(repo, "bhatti-vmm")
	if _, err := os.Stat(vmm); err != nil {
		t.Skip("bhatti-vmm not built — run `make vmm`; skipping")
	}
	ensureVMMSigned(t, vmm)
	eng, err := New(Config{
		DataDir:    t.TempDir(),
		BaseRootfs: buildBaseRootfs(t, repo),
		VMMBinary:  vmm,
		LibDir:     libDir(),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return eng
}

// TestKrucibleAgentSuite runs the shared VMM-agnostic behavior suite against the
// krucible engine — the parity gate (the same suite is meant to pass on FC).
func TestKrucibleAgentSuite(t *testing.T) {
	enginetest.RunAgentSuite(t, newSuiteEngine)
}

// TestKrucibleThermalSuite asserts hot/warm transitions on the krucible engine.
// Skips until the engine implements the thermal surface (P2).
func TestKrucibleThermalSuite(t *testing.T) {
	enginetest.RunThermalSuite(t, newSuiteEngine)
}

// newBlockRootEngine builds a krucible engine that boots sandboxes from a CoW
// ext4 block image (cold-tier capable). Requires mke2fs on the host.
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
	ensureVMMSigned(t, vmm)
	eng, err := New(Config{
		DataDir:    t.TempDir(),
		BaseRootfs: buildBaseRootfs(t, repo),
		VMMBinary:  vmm,
		LibDir:     libDir(),
		BlockRoot:  true,
		// Opt-in: boot the external lean kernel instead of the libkrunfw bundle,
		// so the block-root suites (snapshot, concurrent-wake) double as the
		// lean-kernel parity gate when KRUCIBLE_LEAN_KERNEL is set.
		KernelImage: os.Getenv("KRUCIBLE_LEAN_KERNEL"),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return eng
}

// TestKrucibleSnapshotSuite is the cold-tier gate: Stop (snapshot + free RAM) /
// Start (restore) round-trip with RAM + rootfs intact and exec-after-restore.
func TestKrucibleSnapshotSuite(t *testing.T) {
	enginetest.RunSnapshotSuite(t, newBlockRootEngine)
}

// TestKrucibleReliabilitySuite is the cold-tier HARDENING gate (migration plan
// P1): N stop→start cycles stay stable (RAM + rootfs survive each), lifecycle
// transitions are idempotent, and a concurrent Stop/Start/Exec storm converges
// to a usable VM. The engine-internal failure injections (snapshot-write
// failure, agent-timeout cleanup) live in reliability_test.go.
func TestKrucibleReliabilitySuite(t *testing.T) {
	enginetest.RunReliabilitySuite(t, newBlockRootEngine)
}

// TestKrucibleBlockRootAgentSuite runs the agent suite on a block-root engine.
// With KRUCIBLE_LEAN_KERNEL set it doubles as the cross-arch lean-kernel
// boot+agent gate, independent of the cold tier — so it runs on linux/arm64
// (where the cold tier isn't wired yet) as well as macOS + linux/x86.
func TestKrucibleBlockRootAgentSuite(t *testing.T) {
	enginetest.RunAgentSuite(t, newBlockRootEngine)
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

// libDir returns a dyld search path covering libkrun + libkrunfw. Prefers the
// libkrucible build prefix (our fork's libkrun) and appends the dir holding
// libkrunfw (Homebrew). Colon-separated; passed straight to the helper's
// DYLD_FALLBACK_LIBRARY_PATH / LD_LIBRARY_PATH.
func libDir() string {
	var dirs []string
	// libkrucible install prefix: libkrun.so lands in lib64 on Linux, lib on macOS.
	for _, sub := range []string{"lib64", "lib"} {
		if p, err := filepath.Abs("../../../libkrucible/_install/" + sub); err == nil {
			if m, _ := filepath.Glob(filepath.Join(p, "libkrun.*")); len(m) > 0 {
				dirs = append(dirs, p)
			}
		}
	}
	// libkrunfw: Homebrew on macOS, /usr/local/lib64 (or lib) on Linux.
	for _, d := range []string{"/opt/homebrew/lib", "/usr/local/lib64", "/usr/local/lib", "/usr/lib64", "/usr/lib"} {
		if matches, _ := filepath.Glob(filepath.Join(d, "libkrunfw*")); len(matches) > 0 {
			dirs = append(dirs, d)
			break
		}
	}
	return strings.Join(dirs, ":")
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
	for _, n := range []string{"echo", "errcho", "false", "sleep", "printenv", "cat", "sync", "sh", "writeuid"} {
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
	}
}
`
