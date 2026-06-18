package krucible

import (
	"context"
	"io"
	"net"
	"os"
	"os/exec"
	"testing"
	"time"

	"github.com/sahil-shubham/bhatti/pkg/agent"
)

func ctlCmd(t *testing.T, uds, cmd string) string {
	c, err := net.DialTimeout("unix", uds, 5*time.Second)
	if err != nil { t.Fatalf("dial %s: %v", uds, err) }
	defer c.Close()
	c.SetDeadline(time.Now().Add(30 * time.Second))
	c.Write([]byte(cmd + "\n"))
	buf := make([]byte, 256); n, _ := c.Read(buf)
	return string(buf[:n])
}

func launch(t *testing.T, spec string) (*exec.Cmd, context.CancelFunc) {
	ctx, cancel := context.WithCancel(context.Background())
	cmd := exec.CommandContext(ctx, repoRoot(t)+"/bhatti-vmm", spec)
	cmd.Stdout = io.Discard; cmd.Stderr = io.Discard
	cmd.Env = append(os.Environ(), "DYLD_FALLBACK_LIBRARY_PATH="+libDir(), "LD_LIBRARY_PATH="+libDir())
	if err := cmd.Start(); err != nil { t.Fatalf("start: %v", err) }
	return cmd, cancel
}

// TestColdLoopbackRestore is the cold-wake integration gate: boot -> agent
// ready -> PAUSE+SNAPSHOT -> kill the helper (free RAM) -> restore into a fresh
// helper -> the guest must resume and lohar must answer. Proves the VMM
// cold-wake machinery (memory + vCPU + GIC + vsock/console/rng) round-trips and
// survives the helper process exiting. (exec-after-restore needs a block root.)
func TestColdLoopbackRestore(t *testing.T) {
	if !hasLibkrun() { t.Skip("no libkrun") }
	// Manual/dev helper test: cold restore over a virtio-fs root from a
	// pre-built /tmp/kr-rootfs. Superseded by the cross-platform, block-root
	// TestKrucibleSnapshotSuite (which builds its own rootfs). Skip unless the
	// fixture exists.
	if _, err := os.Stat("/tmp/kr-rootfs"); err != nil {
		t.Skip("no /tmp/kr-rootfs fixture; use TestKrucibleSnapshotSuite for cold-tier coverage")
	}
	dir := "/tmp/bhatti-kr-cold"; os.RemoveAll(dir); os.MkdirAll(dir, 0700)
	snap := "/tmp/bhatti-kr-cold/bundle"
	c := dir + "/c.sock"; f := dir + "/f.sock"; k := dir + "/k.sock"
	base := `"rootfs_dir":"/tmp/kr-rootfs","vcpus":1,"mem_mib":512,"pid1":true,"exec_path":"/init.krun","vsock_control_uds":"` + c + `","vsock_forward_uds":"` + f + `","control_socket_uds":"` + k + `","log_level":2`
	os.WriteFile(dir+"/boot.json", []byte("{"+base+"}"), 0600)
	os.WriteFile(dir+"/restore.json", []byte("{"+base+`,"snapshot_dir":"`+snap+`"}`), 0600)

	ctx := context.Background()
	// 1. boot + wait ready (fs works pre-snapshot)
	cmd1, cancel1 := launch(t, dir+"/boot.json")
	ag := agent.NewKrucibleClient(c, f, "")
	if err := ag.WaitReady(ctx, 30*time.Second); err != nil { cancel1(); t.Fatalf("boot WaitReady: %v", err) }
	t.Log("booted + agent ready")
	// 2. pause + snapshot
	t.Logf("PAUSE -> %s", ctlCmd(t, k, "PAUSE"))
	t.Logf("SNAPSHOT -> %s", ctlCmd(t, k, "SNAPSHOT "+snap))
	// 3. kill original
	cancel1(); cmd1.Wait(); os.Remove(c); os.Remove(f); os.Remove(k)
	time.Sleep(300 * time.Millisecond)
	// 4. restore
	cmd2, cancel2 := launch(t, dir+"/restore.json"); defer func(){ cancel2(); cmd2.Wait() }()
	// 5. probe the restored guest (Activity = no fs access)
	ag2 := agent.NewKrucibleClient(c, f, "")
	var lastErr error
	for i := 0; i < 50; i++ {
		pctx, pcancel := context.WithTimeout(ctx, 1*time.Second)
		info, err := ag2.Activity(pctx); pcancel()
		if err == nil {
			t.Logf("RESTORED guest responded to Activity after %d tries: %+v", i, info)
			// exec-after-restore touches the virtio-fs rootfs, which is NOT
			// persisted (FUSE inode map goes stale). It is expected to hang
			// until the cold-tier guest roots on a block device (see
			// docs/PLAN-krucible-cold-tier.md §1). Informational only.
			ectx, ecancel := context.WithTimeout(ctx, 2*time.Second)
			r, eerr := ag2.Exec(ectx, []string{"true"}, nil, "")
			ecancel()
			t.Logf("(informational) RESTORED exec(true) -> exit=%d err=%v "+
				"(hang/timeout expected on virtio-fs root; needs block root)", r.ExitCode, eerr)
			return
		}
		lastErr = err
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("restored guest never answered Activity: %v", lastErr)
}
