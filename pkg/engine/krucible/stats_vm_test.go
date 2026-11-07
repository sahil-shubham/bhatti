//go:build krucible

package krucible

import (
	"archive/tar"
	"bytes"
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"testing"
	"time"

	"github.com/sahil-shubham/bhatti/pkg/collector"
	"github.com/sahil-shubham/bhatti/pkg/engine"
	"github.com/sahil-shubham/bhatti/pkg/gateway"
)

// The usual tiny rootfs has no shell or yes. Use the real BusyBox utilities
// from an already-cached image, without pulling or giving the container a
// network. The tiny rootfs's synthetic "spin" helper would not exercise the
// actual yes/sh command used by this acceptance test.
func collectorBusybox(t *testing.T, root string) {
	t.Helper()
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker is needed for the real yes/sh fixture")
	}
	const image = "alpine:3.22"
	if out, err := exec.Command("docker", "image", "inspect", image).CombinedOutput(); err != nil {
		t.Skipf("cache %s for the real yes/sh fixture: %v\n%s", image, err, out)
	}
	arch := map[string]string{"arm64": "aarch64", "amd64": "x86_64"}[runtime.GOARCH]
	if arch == "" {
		t.Skipf("no BusyBox loader fixture for %s", runtime.GOARCH)
	}
	loader := "lib/ld-musl-" + arch + ".so.1"
	cmd := exec.Command("docker", "run", "--rm", "--pull=never", "--network=none", image,
		"tar", "-cf", "-", "bin/busybox", loader)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	data, err := cmd.Output()
	if err != nil {
		t.Fatalf("extract real yes/sh fixture: %v\n%s", err, stderr.String())
	}
	tr := tar.NewReader(bytes.NewReader(data))
	copied := 0
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if h.Name != "bin/busybox" && h.Name != loader || h.Typeflag != tar.TypeReg {
			t.Fatalf("unexpected BusyBox fixture entry: %+v", h)
		}
		path := filepath.Join(root, h.Name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		data, err := io.ReadAll(tr)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, data, 0o755); err != nil {
			t.Fatal(err)
		}
		copied++
	}
	if copied != 2 {
		t.Fatalf("extracted %d BusyBox fixture files, want binary and loader", copied)
	}
}

// A guest burning one vCPU shows as about one CPU on its VMM, through the
// engine's own listing and the collector: per process on macOS, through the
// VMM's cgroup on Linux.
func TestCollectorSeesAGuestBurnOneCPUOnItsVMM(t *testing.T) {
	eng := newNetEngine(t)
	ke := eng.(*Engine)
	collectorBusybox(t, ke.cfg.BaseRootfs)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	info, err := eng.Create(ctx, engine.SandboxSpec{Name: "collector-burn", CPUs: 1, MemoryMB: 512, UserID: "collector-test",
		NetPolicy: &gateway.NetPolicyWire{Default: "public"}})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	id := info.ID
	t.Cleanup(func() { eng.Destroy(context.Background(), id) })

	col := collector.New(collector.Config{Source: ke, DataDir: t.TempDir(), Interval: time.Second})
	sample := func(what string) collector.Sandbox {
		t.Helper()
		snap := col.Sample(ctx)
		sb, ok := snap.Sandbox(id)
		if !ok {
			t.Fatalf("%s: the sandbox isn't in the sample: %+v", what, snap.Sandboxes)
		}
		line := what + ": pid " + strconv.Itoa(sb.PID) + " accounting " + sb.Accounting
		if sb.Rates != nil {
			line += " cpu " + f1(sb.Rates.CPUPercent) + "% of a CPU"
		}
		if sb.Memory != nil {
			line += " memory " + f1(float64(sb.Memory.Current)/(1<<20)) + " MiB"
		}
		if sb.IO != nil {
			line += " disk read " + f1(float64(sb.IO.ReadBytes)/(1<<20)) + " MiB written " + f1(float64(sb.IO.WriteBytes)/(1<<20)) + " MiB"
		}
		if sb.Net != nil {
			line += " net rx " + f1(float64(sb.Net.RxBytes)/1024) + " KiB tx " + f1(float64(sb.Net.TxBytes)/1024) + " KiB"
		}
		t.Log(line)
		return sb
	}

	// A booted guest with nothing running, once it has finished starting up: its
	// VMM's CPU use settles from boot's burst to a trickle.
	var idle collector.Sandbox
	sample("idle, first")
	for range 20 {
		time.Sleep(3 * time.Second)
		idle = sample("idle")
		if idle.Rates != nil && idle.Rates.CPUPercent < 15 {
			break
		}
	}
	if idle.Rates == nil || idle.Rates.CPUPercent > 25 {
		t.Fatalf("a guest with nothing running never settled: its VMM used %+v", idle.Rates)
	}
	if idle.Accounting != engine.AccountingProcess && idle.Accounting != engine.AccountingCgroup {
		t.Fatalf("accounting = %q: this engine measures neither per process nor per cgroup", idle.Accounting)
	}

	// One guest process spinning for as long as it is let: one vCPU's worth.
	if _, _, err := ke.ExecDetached(ctx, id, []string{"/bin/busybox", "sh", "-c", "exec /bin/busybox yes > /dev/null"}, ""); err != nil {
		t.Fatalf("start the burner: %v", err)
	}
	sample("burning, first")
	// Command startup also exercises the VMM's control transport. Wait for two
	// complete steady-state intervals rather than folding that transient into
	// the measurement of an otherwise single-threaded guest workload.
	stable := 0
	for range 30 {
		time.Sleep(time.Second)
		s := sample("burning, settling")
		if s.Rates != nil && s.Rates.CPUPercent >= 70 && s.Rates.CPUPercent <= 130 {
			stable++
		} else {
			stable = 0
		}
		if stable == 2 {
			break
		}
	}
	if stable != 2 {
		t.Fatal("the single-vCPU guest workload never reached steady state")
	}
	const interval = 5 * time.Second
	time.Sleep(interval)
	burn := sample("burning")
	if burn.Rates == nil {
		t.Fatal("no rates for the burning sandbox")
	}
	t.Logf("yes > /dev/null: the one-vCPU guest's VMM used %.1f%% of a CPU over %s", burn.Rates.CPUPercent, interval)
	if burn.Rates.CPUPercent < 70 || burn.Rates.CPUPercent > 130 {
		t.Fatalf("a guest burning one vCPU used %.1f%% on its VMM, want about 100%%", burn.Rates.CPUPercent)
	}
	if burn.Memory == nil || burn.Memory.Current < 16<<20 {
		t.Errorf("the VMM's memory = %+v", burn.Memory)
	}
	if series := col.Series(id); len(series) < 3 || series[len(series)-1].CPUPercent < 70 {
		t.Errorf("history = %+v", series)
	}

	// The gateway is measured too: its own use, and what it carried for the
	// guest. A guest that has only booted has sent nothing the gateway carries
	// (its own addresses are configured, and what else it says on the link isn't
	// IPv4 or ARP), so give it a connection to make: whether or not the network
	// beyond is reachable, its first packet crosses the gateway and something
	// comes back.
	nctx, ncancel := context.WithTimeout(ctx, 20*time.Second)
	_, _ = eng.Exec(nctx, id, []string{"netcheck", "tcp"})
	ncancel()
	after := sample("after a connection")
	snap := col.Snapshot()
	if len(snap.Netds) != 1 || snap.Netds[0].CPU == nil {
		t.Fatalf("gateways = %+v", snap.Netds)
	}
	n := snap.Netds[0]
	t.Logf("gateway pid %d: accounting %s, cpu %d µs, stats error %q", n.PID, n.Accounting, n.CPU.UsageUsec, n.StatsErr)
	if n.StatsErr != "" {
		t.Errorf("gateway stats: %s", n.StatsErr)
	} else if after.Net == nil || after.Net.TxPackets == 0 || after.Net.RxPackets == 0 || after.Net.TxBytes == 0 || after.Net.RxBytes == 0 {
		t.Errorf("a guest that opened a connection through its gateway has net figures %+v", after.Net)
	}
}

func f1(v float64) string { return strconv.FormatFloat(v, 'f', 1, 64) }
