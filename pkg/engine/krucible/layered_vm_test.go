//go:build krucible

package krucible

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sahil-shubham/bhatti/pkg/engine"
	"github.com/sahil-shubham/bhatti/pkg/gateway"
)

// writeBig writes mb MiB of pattern to path in the guest and returns the
// sha256 of what it wrote.
func writeBig(t *testing.T, ctx context.Context, ke *Engine, id, path string, mb int) string {
	t.Helper()
	h := sha256.New()
	r := io.TeeReader(&patternReader{left: int64(mb) << 20}, h)
	if err := ke.FileWrite(ctx, id, path, "0644", int64(mb)<<20, r); err != nil {
		t.Fatalf("write %d MiB to %s: %v", mb, path, err)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// sumInGuest is the sha256 of the file at path in the guest.
func sumInGuest(ctx context.Context, ke *Engine, id, path string) (string, error) {
	h := sha256.New()
	if _, _, err := ke.FileRead(ctx, id, path, h); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func writeSmall(t *testing.T, ctx context.Context, ke *Engine, id, path, content string) {
	t.Helper()
	if err := ke.FileWrite(ctx, id, path, "0644", int64(len(content)), strings.NewReader(content)); err != nil {
		t.Fatalf("write %s in %s: %v", path, id, err)
	}
}

func readSmall(ctx context.Context, ke *Engine, id, path string) (string, error) {
	var b bytes.Buffer
	if _, _, err := ke.FileRead(ctx, id, path, &b); err != nil {
		return "", err
	}
	return strings.TrimSpace(b.String()), nil
}

func rootOf(ke *Engine, id string) string {
	return filepath.Join(ke.cfg.DataDir, "sandboxes", id, "root.qcow2")
}

// fsSnapshot takes a disk-only snapshot of the sandbox, which every host that
// can pause a VM can do, and returns its directory and manifest.
func fsSnapshot(t *testing.T, ctx context.Context, ke *Engine, id, name string) (string, []byte) {
	t.Helper()
	if _, err := ke.Exec(ctx, id, []string{"sync"}); err != nil {
		t.Fatalf("sync: %v", err)
	}
	snaps := filepath.Join(ke.cfg.DataDir, "snapshots", "u1")
	manifest, err := ke.CheckpointTyped(ctx, id, "u1", 0, name, snaps, "filesystem")
	if err != nil {
		t.Fatalf("snapshot %s: %v", name, err)
	}
	raw, _ := json.Marshal(manifest)
	return filepath.Join(snaps, name), raw
}

func mustSum(t *testing.T, path string) string {
	t.Helper()
	sum, _, err := hashFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return sum
}

// TestKrucibleResumeIsLayeredOverTheSnapshot: resume from a snapshot with a
// large delta. Each clone's root is an overlay over the snapshot's disk that
// takes next to nothing, the guest sees the snapshot's files, a write in one
// clone reaches neither the snapshot nor its siblings nor a clone made
// afterwards, and the snapshot can't be deleted while clones are layered
// over it: afterwards it can.
func TestKrucibleResumeIsLayeredOverTheSnapshot(t *testing.T) {
	ke := newPauseEngine(t).(*Engine)
	ctx, cancel := context.WithTimeout(context.Background(), 280*time.Second)
	defer cancel()
	none := &gateway.NetPolicyWire{Default: gateway.PostureNone}

	src, err := ke.Create(ctx, engine.SandboxSpec{Name: "layersrc", CPUs: 1, MemoryMB: 512, DiskSizeMB: 1024, NetPolicy: none})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	t.Cleanup(func() { ke.Destroy(context.Background(), src.ID) })
	const deltaMB = 160
	bigSum := writeBig(t, ctx, ke, src.ID, "/workspace/big", deltaMB)
	writeSmall(t, ctx, ke, src.ID, "/etc/layer-marker", "from-the-snapshot")

	golden, manifest := fsSnapshot(t, ctx, ke, src.ID, "golden")
	disk := filepath.Join(golden, "rootfs.qcow2")
	if n := allocated(t, disk); n < deltaMB<<20 {
		t.Fatalf("the snapshot's disk holds %d bytes; the test needs a delta of %d MiB", n, deltaMB)
	}
	diskSum := mustSum(t, disk)

	const clones = 3
	ids := make([]string, clones)
	took := make([]time.Duration, clones)
	var wg sync.WaitGroup
	errs := make([]error, clones)
	start := time.Now()
	for i := range ids {
		wg.Add(1)
		go func() {
			defer wg.Done()
			t0 := time.Now()
			info, err := ke.ResumeFromManifestJSON(ctx, golden, manifest, fmt.Sprintf("clone%d", i), "u1")
			took[i], errs[i] = time.Since(t0), err
			ids[i] = info.ID
		}()
	}
	wg.Wait()
	t.Logf("%d parallel resumes of a snapshot holding %d MiB: %v in all, %v", clones, allocated(t, disk)>>20, time.Since(start), took)
	for i, err := range errs {
		if err != nil {
			t.Fatalf("resume %d: %v", i, err)
		}
		id := ids[i]
		t.Cleanup(func() { ke.Destroy(context.Background(), id) })
	}

	nodeFile, err := os.Stat(disk)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range ids {
		root := rootOf(ke, id)
		if n := allocated(t, root); n > 4<<20 {
			t.Fatalf("%s takes %d bytes: the resume copied the snapshot's delta", root, n)
		}
		backing := backingOf(t, root)
		if fi, err := os.Stat(backing); err != nil || !os.SameFile(fi, nodeFile) {
			t.Fatalf("%s is layered over %s, which isn't the snapshot's disk (%v)", root, backing, err)
		}
		// The guest sees what the snapshot holds.
		if got, err := readSmall(ctx, ke, id, "/etc/layer-marker"); err != nil || got != "from-the-snapshot" {
			t.Fatalf("clone %s: marker %q, %v", id, got, err)
		}
		if got, err := sumInGuest(ctx, ke, id, "/workspace/big"); err != nil || got != bigSum {
			t.Fatalf("clone %s: the %d MiB file is %q (%v), want %q", id, deltaMB, got, err, bigSum)
		}
	}

	// Writes stay where they were made: in the clone, not the snapshot, not a
	// sibling, not a clone made after.
	writeSmall(t, ctx, ke, ids[0], "/workspace/who", "clone0")
	writeSmall(t, ctx, ke, ids[0], "/etc/layer-marker", "changed-in-clone0")
	if _, err := ke.Exec(ctx, ids[0], []string{"sync"}); err != nil {
		t.Fatal(err)
	}
	if got, err := readSmall(ctx, ke, ids[0], "/workspace/who"); err != nil || got != "clone0" {
		t.Fatalf("clone0 lost its own write: %q, %v", got, err)
	}
	if got, err := readSmall(ctx, ke, ids[1], "/workspace/who"); err == nil {
		t.Fatalf("a sibling sees clone0's file: %q", got)
	}
	if got, _ := readSmall(ctx, ke, ids[1], "/etc/layer-marker"); got != "from-the-snapshot" {
		t.Fatalf("a sibling sees clone0's change to a file of the snapshot: %q", got)
	}
	late, err := ke.ResumeFromManifestJSON(ctx, golden, manifest, "late", "u1")
	if err != nil {
		t.Fatalf("resume after a clone wrote: %v", err)
	}
	t.Cleanup(func() { ke.Destroy(context.Background(), late.ID) })
	if got, err := readSmall(ctx, ke, late.ID, "/workspace/who"); err == nil {
		t.Fatalf("a later clone sees clone0's file: %q", got)
	}
	if got, _ := readSmall(ctx, ke, late.ID, "/etc/layer-marker"); got != "from-the-snapshot" {
		t.Fatalf("a later clone sees clone0's change: %q", got)
	}
	if mustSum(t, disk) != diskSum {
		t.Fatal("a clone's write reached the snapshot's disk")
	}

	// The snapshot is in use while the clones are.
	err = ke.RemoveSnapshot(ctx, golden)
	if !errors.Is(err, engine.ErrSnapshotInUse) || !strings.Contains(err.Error(), "4 sandboxes") {
		t.Fatalf("remove with four clones: %v", err)
	}
	if _, err := os.Stat(disk); err != nil {
		t.Fatalf("a refused removal took the disk: %v", err)
	}
	for _, id := range append(ids, late.ID) {
		if err := ke.Destroy(ctx, id); err != nil {
			t.Fatalf("destroy %s: %v", id, err)
		}
	}
	if err := ke.RemoveSnapshot(ctx, golden); err != nil {
		t.Fatalf("remove after the clones were destroyed: %v", err)
	}
	ke.waitNodeSweep()
	if left := nodeEntries(t, ke.cfg.DataDir); len(left) != 0 {
		t.Fatalf("nodes left in the store: %v", left)
	}
}

// TestKrucibleDeletingTheSnapshotDirectoryCantBreakAClone: the node store keeps
// a snapshot's disk whatever is done to its directory, so a snapshot removed
// around the check (by hand) leaves its clones running and bootable, and the
// node goes once the last of them is destroyed.
func TestKrucibleDeletingTheSnapshotDirectoryCantBreakAClone(t *testing.T) {
	ke := newPauseEngine(t).(*Engine)
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Second)
	defer cancel()
	none := &gateway.NetPolicyWire{Default: gateway.PostureNone}

	src, err := ke.Create(ctx, engine.SandboxSpec{Name: "handsrc", CPUs: 1, MemoryMB: 512, NetPolicy: none})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	t.Cleanup(func() { ke.Destroy(context.Background(), src.ID) })
	writeSmall(t, ctx, ke, src.ID, "/etc/hand-marker", "still-here")
	golden, manifest := fsSnapshot(t, ctx, ke, src.ID, "golden")
	clone, err := ke.ResumeFromManifestJSON(ctx, golden, manifest, "handclone", "u1")
	if err != nil {
		t.Fatalf("resume: %v", err)
	}
	t.Cleanup(func() { ke.Destroy(context.Background(), clone.ID) })

	if err := os.RemoveAll(golden); err != nil {
		t.Fatal(err)
	}
	if got, err := readSmall(ctx, ke, clone.ID, "/etc/hand-marker"); err != nil || got != "still-here" {
		t.Fatalf("the running clone after its snapshot was deleted: %q, %v", got, err)
	}
	// A power cycle opens the chain again, from the names in the header.
	if err := ke.Stop(ctx, clone.ID); err != nil {
		t.Fatalf("stop: %v", err)
	}
	if err := ke.Start(ctx, clone.ID); err != nil {
		t.Fatalf("start after the snapshot's directory was deleted: %v", err)
	}
	if got, err := readSmall(ctx, ke, clone.ID, "/etc/hand-marker"); err != nil || got != "still-here" {
		t.Fatalf("the restarted clone: %q, %v", got, err)
	}

	if len(nodeEntries(t, ke.cfg.DataDir)) != 1 {
		t.Fatalf("store holds %v", nodeEntries(t, ke.cfg.DataDir))
	}
	if err := ke.Destroy(ctx, clone.ID); err != nil {
		t.Fatal(err)
	}
	ke.waitNodeSweep()
	if left := nodeEntries(t, ke.cfg.DataDir); len(left) != 0 {
		t.Fatalf("destroying the last clone of a deleted snapshot left %v", left)
	}
}

// TestKrucibleGenerationsOfSnapshotsBootPastTheDepthCap: snapshot a resumed
// sandbox, resume that, and so on past maxChainDepth. Every generation boots
// and sees the files of every generation before it (the VMM's qcow2 driver
// walks a chain of overlays over overlays), a disk never goes past the cap,
// and the snapshots outlive the sandboxes they were taken from.
func TestKrucibleGenerationsOfSnapshotsBootPastTheDepthCap(t *testing.T) {
	ke := newPauseEngine(t).(*Engine)
	ctx, cancel := context.WithTimeout(context.Background(), 290*time.Second)
	defer cancel()
	none := &gateway.NetPolicyWire{Default: gateway.PostureNone}

	cur, err := ke.Create(ctx, engine.SandboxSpec{Name: "gen0", CPUs: 1, MemoryMB: 512, NetPolicy: none})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	live := cur.ID
	t.Cleanup(func() { ke.Destroy(context.Background(), live) })
	writeSmall(t, ctx, ke, live, "/workspace/gen0", "generation 0")

	var depths []int
	for gen := 1; gen <= maxChainDepth+2; gen++ {
		dir, manifest := fsSnapshot(t, ctx, ke, live, fmt.Sprintf("g%d", gen-1))
		next, err := ke.ResumeFromManifestJSON(ctx, dir, manifest, fmt.Sprintf("gen%d", gen), "u1")
		if err != nil {
			t.Fatalf("resume generation %d: %v", gen, err)
		}
		// The sandbox the snapshot was taken from can go: the snapshot doesn't
		// need it, and what the next generation sits on is kept by the store.
		if err := ke.Destroy(ctx, live); err != nil {
			t.Fatal(err)
		}
		live = next.ID
		d, err := chainDepth(rootOf(ke, live))
		if err != nil {
			t.Fatal(err)
		}
		depths = append(depths, d)
		writeSmall(t, ctx, ke, live, fmt.Sprintf("/workspace/gen%d", gen), fmt.Sprintf("generation %d", gen))
		for g := 0; g <= gen; g++ {
			want := fmt.Sprintf("generation %d", g)
			if got, err := readSmall(ctx, ke, live, fmt.Sprintf("/workspace/gen%d", g)); err != nil || got != want {
				t.Fatalf("generation %d can't see generation %d's file: %q, %v", gen, g, got, err)
			}
		}
	}
	for i, d := range depths {
		want := min(i+2, maxChainDepth)
		if d != want {
			t.Fatalf("depth by generation = %v; generation %d should be %d", depths, i+1, want)
		}
	}
}

// TestKrucibleForkLayersOverACopyOfTheSource: where the VMM can save a guest's
// memory, a fork copies its source's overlay once (while the source is
// paused) and the child is layered over that copy, which stays in the node
// store until nothing is layered over it. The source keeps running and the
// child sees its disk, its volume and its memory.
func TestKrucibleForkLayersOverACopyOfTheSource(t *testing.T) {
	ke := newCheckpointEngine(t).(*Engine)
	ctx, cancel := context.WithTimeout(context.Background(), 250*time.Second)
	defer cancel()

	volPath := filepath.Join(t.TempDir(), "vol.ext4")
	if out, err := exec.Command("mke2fs", "-t", "ext4", "-F", "-q", volPath, "64M").CombinedOutput(); err != nil {
		t.Fatalf("mke2fs volume: %v: %s", err, out)
	}
	vol := engine.ResolvedVolume{FilePath: volPath, DriveID: "vol0", Name: "data", Mount: "/data"}
	// Owned, as every sandbox the server makes is.
	src, err := ke.Create(ctx, engine.SandboxSpec{Name: "layerfork", UserID: "layerfork-owner", CPUs: 1, MemoryMB: 512, DiskSizeMB: 1024, ResolvedVolumes: []engine.ResolvedVolume{vol}})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	t.Cleanup(func() { ke.Destroy(context.Background(), src.ID) })
	bigSum := writeBig(t, ctx, ke, src.ID, "/workspace/big", 96)
	writeSmall(t, ctx, ke, src.ID, "/data/vmark", "volume-of-the-source")
	writeSmall(t, ctx, ke, src.ID, "/tmp/rammark", "ram-of-the-source")
	if _, err := ke.Exec(ctx, src.ID, []string{"sync"}); err != nil {
		t.Fatal(err)
	}

	fork, err := ke.Fork(ctx, src.ID, "layerforked")
	if err != nil {
		t.Fatalf("Fork: %v", err)
	}
	t.Cleanup(func() { ke.Destroy(context.Background(), fork.ID) })

	root := rootOf(ke, fork.ID)
	if n := allocated(t, root); n > 4<<20 {
		t.Fatalf("the fork's root takes %d bytes: it was copied again after the checkpoint", n)
	}
	chain, err := backingChain(root)
	if err != nil || len(chain) != 2 || filepath.Dir(chain[0]) != nodesDir(ke.cfg.DataDir) {
		t.Fatalf("fork's root chain %v, %v; want a node, then the base", chain, err)
	}
	if fi, _ := os.Stat(chain[0]); fi != nil {
		if src, _ := os.Stat(rootOf(ke, src.ID)); src != nil && os.SameFile(fi, src) {
			t.Fatal("the fork is layered over the source's live overlay")
		}
	}
	if left, _ := filepath.Glob(filepath.Join(ke.cfg.DataDir, "fork-*")); len(left) != 0 {
		t.Fatalf("staging left behind: %v", left)
	}
	if got, err := sumInGuest(ctx, ke, fork.ID, "/workspace/big"); err != nil || got != bigSum {
		t.Fatalf("fork's big file %q, %v", got, err)
	}
	for path, want := range map[string]string{"/data/vmark": "volume-of-the-source", "/tmp/rammark": "ram-of-the-source"} {
		if got, err := readSmall(ctx, ke, fork.ID, path); err != nil || got != want {
			t.Fatalf("fork %s = %q, %v; want %q", path, got, err, want)
		}
	}
	// The volume is the fork's own: its overlay is over a frozen copy.
	if got, err := readSmall(ctx, ke, fork.ID, "/data/vmark"); err != nil || got == "" {
		t.Fatal(err)
	}
	writeSmall(t, ctx, ke, fork.ID, "/data/vmark", "volume-of-the-fork")
	if got, _ := readSmall(ctx, ke, src.ID, "/data/vmark"); got != "volume-of-the-source" {
		t.Fatalf("the source's volume after the fork wrote: %q", got)
	}
	// The source kept running.
	if r, err := ke.Exec(ctx, src.ID, []string{"echo", "alive"}); err != nil || !strings.Contains(r.Stdout, "alive") {
		t.Fatalf("source after the fork: %v %q", err, r.Stdout)
	}

	if err := ke.Destroy(ctx, fork.ID); err != nil {
		t.Fatal(err)
	}
	ke.waitNodeSweep()
	if left := nodeEntries(t, ke.cfg.DataDir); len(left) != 0 {
		t.Fatalf("nodes left after the only fork was destroyed: %v", left)
	}
}
