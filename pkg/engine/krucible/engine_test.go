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

// newSuiteEngine builds a krucible engine for the shared enginetest suite,
// self-skipping if libkrun / bhatti-vmm aren't available (so `go test ./...`
// stays green on hosts without libkrun). Build the helper with `make vmm`.
func newSuiteEngine(t *testing.T) engine.Engine {
	repo := repoRoot(t)
	if !hasLibkrun() {
		t.Skip("libkrun not installed (pkg-config libkrun); skipping")
	}
	vmm := filepath.Join(repo, "bhatti-vmm")
	if _, err := os.Stat(vmm); err != nil {
		t.Skip("bhatti-vmm not built — run `make vmm`; skipping")
	}
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
	if _, err := exec.LookPath("mke2fs"); err != nil {
		t.Skip("mke2fs not found (e2fsprogs); skipping block-root suite")
	}
	vmm := filepath.Join(repo, "bhatti-vmm")
	if _, err := os.Stat(vmm); err != nil {
		t.Skip("bhatti-vmm not built — run `make vmm`; skipping")
	}
	eng, err := New(Config{
		DataDir:    t.TempDir(),
		BaseRootfs: buildBaseRootfs(t, repo),
		VMMBinary:  vmm,
		LibDir:     libDir(),
		BlockRoot:  true,
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
	for _, d := range []string{"bin", "proc", "sys", "dev/pts", "tmp", "run", "etc", "root", "usr/local/bin"} {
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
	for _, n := range []string{"echo", "errcho", "false", "sleep", "printenv", "cat"} {
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
	"time"
)

func main() {
	switch filepath.Base(os.Args[0]) {
	case "echo":
		fmt.Println(strings.Join(os.Args[1:], " "))
	case "errcho":
		fmt.Fprintln(os.Stderr, strings.Join(os.Args[1:], " "))
	case "false":
		os.Exit(1)
	case "sleep":
		if len(os.Args) > 1 {
			n, _ := strconv.Atoi(os.Args[1])
			time.Sleep(time.Duration(n) * time.Second)
		}
	case "printenv": // printenv KEY -> value of os.Getenv(KEY)
		if len(os.Args) > 1 {
			fmt.Println(os.Getenv(os.Args[1]))
		}
	case "cat": // cat FILE -> contents (os.ReadFile handles 0-size procfs files)
		if len(os.Args) > 1 {
			b, _ := os.ReadFile(os.Args[1])
			os.Stdout.Write(b)
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
	case "serve":
		http.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
			io.WriteString(w, "hello-from-guest\n")
		})
		http.ListenAndServe("0.0.0.0:"+os.Args[2], nil)
	}
}
`
