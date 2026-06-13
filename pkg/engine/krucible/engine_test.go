package krucible

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/sahil-shubham/bhatti/pkg/engine"
)

// TestEngineCreateExecDestroy is the P1 gate: boot a real microVM via the
// bhatti-vmm helper and drive lohar's agent through the bridged vsock.
//
// Self-skips unless the prereqs are present (libkrun via pkg-config + a built
// bhatti-vmm), so `go test ./...` stays green on hosts without libkrun. Build
// the helper first with `make vmm`.
func TestEngineCreateExecDestroy(t *testing.T) {
	repo := repoRoot(t)
	if !hasLibkrun() {
		t.Skip("libkrun not installed (pkg-config libkrun); skipping krucible engine test")
	}
	vmm := filepath.Join(repo, "bhatti-vmm")
	if _, err := os.Stat(vmm); err != nil {
		t.Skip("bhatti-vmm not built — run `make vmm`; skipping")
	}

	base := buildBaseRootfs(t, repo)
	eng, err := New(Config{
		DataDir:    t.TempDir(),
		BaseRootfs: base,
		VMMBinary:  vmm,
		LibDir:     libDir(),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	info, err := eng.Create(ctx, engine.SandboxSpec{Name: "p1test", CPUs: 1, MemoryMB: 512})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	defer eng.Destroy(context.Background(), info.ID)

	if info.Status != "running" {
		t.Fatalf("status = %q, want running", info.Status)
	}

	// List + Status reflect the new sandbox.
	if list, _ := eng.List(ctx); len(list) != 1 {
		t.Fatalf("List len = %d, want 1", len(list))
	}

	// Exec: exit code round-trips.
	res, err := eng.Exec(ctx, info.ID, []string{"true"})
	if err != nil {
		t.Fatalf("Exec(true): %v", err)
	}
	if res.ExitCode != 0 {
		t.Fatalf("Exec(true) exit = %d, want 0", res.ExitCode)
	}

	// Exec: stdout round-trips.
	res, err = eng.Exec(ctx, info.ID, []string{"echo", "hello-krucible"})
	if err != nil {
		t.Fatalf("Exec(echo): %v", err)
	}
	if !strings.Contains(res.Stdout, "hello-krucible") {
		t.Fatalf("Exec(echo) stdout = %q, want it to contain hello-krucible", res.Stdout)
	}

	// Destroy removes it.
	if err := eng.Destroy(ctx, info.ID); err != nil {
		t.Fatalf("Destroy: %v", err)
	}
	if list, _ := eng.List(ctx); len(list) != 0 {
		t.Fatalf("List after destroy = %d, want 0", len(list))
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

// libDir finds the dir holding libkrunfw (libkrun dlopen()s it by name).
func libDir() string {
	for _, d := range []string{"/opt/homebrew/lib", "/usr/local/lib", "/usr/lib"} {
		if matches, _ := filepath.Glob(filepath.Join(d, "libkrunfw*")); len(matches) > 0 {
			return d
		}
	}
	return ""
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
	for _, n := range []string{"echo", "errcho", "false", "sleep"} {
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
