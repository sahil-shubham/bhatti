package krucible

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func nodeGroupTestEngine(t *testing.T) *Engine {
	t.Helper()
	return &Engine{cfg: Config{DataDir: t.TempDir()}, dropVMM: true,
		vmmIDs: map[uint32]string{}, netds: map[string]*netdInstance{}}
}

func requireNodeGID(t *testing.T, e *Engine, vm *VM) uint32 {
	t.Helper()
	gid, err := e.nodeGIDFor(vm)
	if err != nil {
		t.Fatal(err)
	}
	if !isNodeGID(gid) || gid == vmmGroup || gid < vmmNetGIDBase+vmmIDSlots {
		t.Fatalf("node GID %d overlaps an ephemeral identity range", gid)
	}
	return gid
}

func TestNodeGroupReservationsSurviveRestartAndLastVM(t *testing.T) {
	e := nodeGroupTestEngine(t)
	vm := &VM{ID: "last-sandbox", UserID: "owner-a"}
	gid := requireNodeGID(t, e, vm)
	if _, err := e.vmmUIDFor(vm); err != nil {
		t.Fatal(err)
	}
	key := ownerKeyFor(vm.UserID, vm.ID)
	dir := filepath.Join(e.cfg.DataDir, "netd")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	e.netds[key] = &netdInstance{owner: key, refs: 1, dir: dir, vmmGID: e.netdGIDLocked(key)}
	e.releaseVMM(vm)
	e.releaseNetd(key, -1)
	if len(e.vmmIDs) != 0 || len(e.netds) != 0 {
		t.Fatal("last helper/netd reservation was not released")
	}
	restarted := &Engine{cfg: e.cfg, dropVMM: true}
	if got := requireNodeGID(t, restarted, &VM{ID: "new-sandbox", UserID: vm.UserID}); got != gid {
		t.Fatalf("last-VM removal/restart changed node GID: %d -> %d", gid, got)
	}
	if other := requireNodeGID(t, restarted, &VM{ID: "other", UserID: "owner-b"}); other == gid {
		t.Fatal("a retained owner's node group was reused")
	}
	r, err := readNodeGroups(e.cfg.DataDir)
	if err != nil || len(r.Groups) != 2 {
		t.Fatalf("durable reservations: %+v, %v", r, err)
	}
	for _, g := range r.Groups {
		if len(g.Identity) > 32 || g.Identity != nodeOwnerIdentity(g.Owner) {
			t.Fatalf("unstable or oversized logical identity: %+v", g)
		}
	}
	for _, path := range []string{nodeGroupsFile, nodeGroupsLock} {
		fi, err := os.Stat(filepath.Join(e.cfg.DataDir, path))
		if err != nil || !nodeAccessRecord(fi) {
			t.Fatalf("%s is not daemon-only: %v, %v", path, fi, err)
		}
	}
}

func TestNodeGroupAnonymousOwnersShareStorageScope(t *testing.T) {
	e := nodeGroupTestEngine(t)
	a := requireNodeGID(t, e, &VM{ID: "a"})
	b := requireNodeGID(t, e, &VM{ID: "b"})
	named := requireNodeGID(t, e, &VM{ID: "unrelated", UserID: "anonymous"})
	if a != b || a == named {
		t.Fatalf("anonymous storage scope was split or merged with named owner: %d %d %d", a, b, named)
	}
	if again := requireNodeGID(t, &Engine{cfg: e.cfg, dropVMM: true}, &VM{}); again != a {
		t.Fatalf("anonymous identity changed across restart: %d -> %d", a, again)
	}
}

func TestNodeGroupHashCollisionsNeverShareOrReuse(t *testing.T) {
	e := nodeGroupTestEngine(t)
	seen := map[uint32]string{}
	var a, b string
	for i := range vmmIDSlots + 1 {
		owner := fmt.Sprintf("collision-%d", i)
		slot := nodeGroupSlot(ownerKeyFor(owner, ""))
		if previous, ok := seen[slot]; ok {
			a, b = previous, owner
			break
		}
		seen[slot] = owner
	}
	if a == "" {
		t.Fatal("pigeonhole search did not produce a slot collision")
	}
	ga := requireNodeGID(t, e, &VM{ID: "a", UserID: a})
	restarted := &Engine{cfg: e.cfg, dropVMM: true}
	gb := requireNodeGID(t, restarted, &VM{ID: "b", UserID: b})
	if ga == gb || ga != vmmNodeGIDBase+nodeGroupSlot(ownerKeyFor(a, "")) {
		t.Fatalf("colliding owners did not retain distinct reservations: %d %d", ga, gb)
	}
	if got := requireNodeGID(t, restarted, &VM{ID: "a-again", UserID: a}); got != ga {
		t.Fatalf("collision replaced the first owner's group: %d -> %d", ga, got)
	}
	// Even a logical-identity collision must fail rather than share the group.
	r := nodeGroupRegistry{Version: 1, Groups: []nodeGroupReservation{{
		Identity: nodeOwnerIdentity("u:first"), Owner: "u:second", GID: ga,
	}}}
	if _, _, err := r.reserve("u:first"); err == nil || len(r.Groups) != 1 {
		t.Fatal("logical identity collision reused or changed the reservation")
	}
}

const nodeGroupProcessEnv = "BHATTI_TEST_NODE_GROUP_PROCESS"

func TestNodeGroupReservationProcess(t *testing.T) {
	payload := os.Getenv(nodeGroupProcessEnv)
	if payload == "" {
		t.Skip("subprocess helper")
	}
	var request struct{ Data, Owner string }
	if err := json.Unmarshal([]byte(payload), &request); err != nil {
		t.Fatal(err)
	}
	e := &Engine{cfg: Config{DataDir: request.Data}, dropVMM: true}
	gid, err := e.nodeGIDFor(&VM{ID: "child", UserID: request.Owner})
	if err != nil {
		t.Fatal(err)
	}
	fmt.Printf("NODE_GID=%d\n", gid)
}

func TestNodeGroupReservationsSerializeAcrossProcesses(t *testing.T) {
	e := nodeGroupTestEngine(t)
	// Allocation must not upgrade a shared bases flock held by a launch.
	unlock, err := lockBases(e.cfg.DataDir, false)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	const count = 8
	commands := make([]*exec.Cmd, count)
	outputs := make([]bytes.Buffer, count)
	for i := range commands {
		payload, err := json.Marshal(struct{ Data, Owner string }{e.cfg.DataDir, fmt.Sprintf("owner-%d", i)})
		if err != nil {
			t.Fatal(err)
		}
		cmd := exec.CommandContext(ctx, executable, "-test.run=^TestNodeGroupReservationProcess$")
		cmd.Env = append(os.Environ(), nodeGroupProcessEnv+"="+string(payload))
		cmd.Stdout, cmd.Stderr = &outputs[i], &outputs[i]
		commands[i] = cmd
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
	}
	seen := map[uint32]bool{}
	for i, cmd := range commands {
		if err := cmd.Wait(); err != nil {
			t.Fatalf("reservation child %d: %v\n%s", i, err, &outputs[i])
		}
		var gid uint32
		for _, line := range strings.Split(outputs[i].String(), "\n") {
			if value, found := strings.CutPrefix(line, "NODE_GID="); found {
				n, err := strconv.ParseUint(value, 10, 32)
				if err != nil {
					t.Fatal(err)
				}
				gid = uint32(n)
			}
		}
		if !isNodeGID(gid) || seen[gid] {
			t.Fatalf("child %d received missing/duplicate GID %d: %s", i, gid, &outputs[i])
		}
		seen[gid] = true
	}
	r, err := readNodeGroups(e.cfg.DataDir)
	if err != nil || len(r.Groups) != count {
		t.Fatalf("lost a cross-process reservation: %+v, %v", r, err)
	}
}

func TestNodeGroupRegistryRejectsMalformedAndUntrusted(t *testing.T) {
	valid := func() nodeGroupRegistry {
		return nodeGroupRegistry{Version: 1, Groups: []nodeGroupReservation{{
			Identity: nodeOwnerIdentity("u:owner-a"), Owner: "u:owner-a", GID: vmmNodeGIDBase,
		}}}
	}
	cases := []struct {
		name   string
		mutate func(*testing.T, string)
	}{
		{"malformed", func(t *testing.T, p string) { writeFile(t, p, []byte("{")) }},
		{"missing-version", func(t *testing.T, p string) { writeFile(t, p, []byte(`{"groups":[]}`)) }},
		{"unknown-field", func(t *testing.T, p string) { writeFile(t, p, []byte(`{"version":1,"groups":[],"trust_me":true}`)) }},
		{"trailing-data", func(t *testing.T, p string) { writeFile(t, p, []byte(`{"version":1,"groups":[]} {}`)) }},
		{"version", func(t *testing.T, p string) {
			r := valid()
			r.Version = 2
			if err := writeDaemonMetadata(p, r); err != nil {
				t.Fatal(err)
			}
		}},
		{"identity-mismatch", func(t *testing.T, p string) {
			r := valid()
			r.Groups[0].Owner = "u:other"
			if err := writeDaemonMetadata(p, r); err != nil {
				t.Fatal(err)
			}
		}},
		{"duplicate-owner", func(t *testing.T, p string) {
			r := valid()
			g := r.Groups[0]
			g.GID++
			r.Groups = append(r.Groups, g)
			if err := writeDaemonMetadata(p, r); err != nil {
				t.Fatal(err)
			}
		}},
		{"duplicate-gid", func(t *testing.T, p string) {
			r := valid()
			r.Groups = append(r.Groups, nodeGroupReservation{Identity: nodeOwnerIdentity("u:b"), Owner: "u:b", GID: r.Groups[0].GID})
			if err := writeDaemonMetadata(p, r); err != nil {
				t.Fatal(err)
			}
		}},
		{"ephemeral-gid", func(t *testing.T, p string) {
			r := valid()
			r.Groups[0].GID = vmmNetGIDBase
			if err := writeDaemonMetadata(p, r); err != nil {
				t.Fatal(err)
			}
		}},
		{"group-readable", func(t *testing.T, p string) {
			if err := os.Chmod(p, 0o640); err != nil {
				t.Fatal(err)
			}
		}},
		{"world-writable", func(t *testing.T, p string) {
			if err := os.Chmod(p, 0o622); err != nil {
				t.Fatal(err)
			}
		}},
		{"symlink", func(t *testing.T, p string) {
			if err := os.Rename(p, p+".real"); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(p+".real", p); err != nil {
				t.Fatal(err)
			}
		}},
		{"hardlink", func(t *testing.T, p string) {
			if err := os.Link(p, p+".alias"); err != nil {
				t.Fatal(err)
			}
		}},
		{"fifo", func(t *testing.T, p string) {
			if err := os.Remove(p); err != nil {
				t.Fatal(err)
			}
			if err := syscall.Mkfifo(p, 0o600); err != nil {
				t.Fatal(err)
			}
		}},
		{"foreign-owner", func(t *testing.T, p string) {
			if os.Geteuid() != 0 {
				t.Skip("requires chown")
			}
			if err := os.Chown(p, vmmUIDBase, vmmUIDBase); err != nil {
				t.Fatal(err)
			}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := nodeGroupTestEngine(t)
			path := filepath.Join(e.cfg.DataDir, nodeGroupsFile)
			if err := writeDaemonMetadata(path, valid()); err != nil {
				t.Fatal(err)
			}
			tc.mutate(t, path)
			if _, err := e.nodeGIDFor(&VM{ID: "b", UserID: "owner-b"}); err == nil {
				t.Fatal("untrusted/malformed registry issued a group")
			}
			source := filepath.Join(e.cfg.DataDir, "source.img")
			writeFile(t, source, []byte("private"))
			if err := e.prepareNodeAccess(source); err == nil {
				t.Fatal("untrusted/malformed registry permitted node publication")
			}
		})
	}
}

func TestNodeGroupRegistryRejectsUntrustedLock(t *testing.T) {
	for _, kind := range []string{"symlink", "hardlink", "group-readable"} {
		t.Run(kind, func(t *testing.T) {
			e := nodeGroupTestEngine(t)
			requireNodeGID(t, e, &VM{ID: "a", UserID: "a"})
			path := filepath.Join(e.cfg.DataDir, nodeGroupsLock)
			switch kind {
			case "symlink":
				if err := os.Rename(path, path+".real"); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(path+".real", path); err != nil {
					t.Fatal(err)
				}
			case "hardlink":
				if err := os.Link(path, path+".alias"); err != nil {
					t.Fatal(err)
				}
			case "group-readable":
				if err := os.Chmod(path, 0o640); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := e.nodeGIDFor(&VM{ID: "b", UserID: "b"}); err == nil {
				t.Fatal("untrusted lock permitted a reservation")
			}
		})
	}
}

func TestPrepareNodeAccessUnprivilegedNoop(t *testing.T) {
	path := filepath.Join(t.TempDir(), "source.img")
	writeFile(t, path, []byte("source"))
	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := (&Engine{}).prepareNodeAccess(path); err != nil {
		t.Fatal(err)
	}
	after, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if before.Mode() != after.Mode() || *before.Sys().(*syscall.Stat_t) != *after.Sys().(*syscall.Stat_t) {
		t.Fatal("unprivileged node preparation changed the inode")
	}
}

func rootNodeAccessFixture(t *testing.T) (*nodeFixture, string) {
	t.Helper()
	if runtime.GOOS != "linux" || os.Geteuid() != 0 {
		t.Skip("requires root Linux numeric credentials")
	}
	root, err := os.MkdirTemp("/tmp", "bhatti-node-access-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	if err := os.Chmod(root, 0o711); err != nil {
		t.Fatal(err)
	}
	data := filepath.Join(root, "data")
	base := filepath.Join(data, "images", "bases", "rootfs-test-0123456789abcdef.ext4")
	b := writeBase(t, base, 1)
	e := &Engine{cfg: Config{DataDir: data, BaseImage: base, SocketDir: filepath.Join(root, "sock")},
		dropVMM: true, vmmIDs: map[uint32]string{}, netds: map[string]*netdInstance{}}
	// Exercise the same ancestor-directory DAC setup as initConfinement,
	// without requiring /dev/kvm for a credentials-only subprocess test.
	for _, dir := range []string{filepath.Join(data, "sandboxes"), e.cfg.SocketDir} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := e.vmmReachable(dir); err != nil {
			t.Fatal(err)
		}
	}
	return &nodeFixture{t: t, e: e, data: data, base: base, size: int64(len(b))}, root
}

func assertNodeMode(t *testing.T, path string, gid uint32, mode os.FileMode) {
	t.Helper()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	st := fi.Sys().(*syscall.Stat_t)
	if st.Uid != 0 || st.Gid != gid || fi.Mode().Perm() != mode {
		t.Fatalf("%s is %d:%d %04o; want 0:%d %04o", path, st.Uid, st.Gid, fi.Mode().Perm(), gid, mode)
	}
}

func TestPrepareNodeAccessBeforePublication(t *testing.T) {
	f, root := rootNodeAccessFixture(t)
	source := filepath.Join(root, "external", "source.img")
	writeFile(t, source, []byte("private source"))
	if err := os.Chown(source, vmmUIDBase+17, vmmGroup); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(source, 0o666); err != nil {
		t.Fatal(err)
	}
	originalLink := linkNode
	t.Cleanup(func() { linkNode = originalLink })
	published := false
	linkNode = func(src, dst string) error {
		assertNodeMode(t, src, 0, 0o600)
		if _, err := os.Lstat(dst); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("store link was already published: %s, %v", dst, err)
		}
		published = true
		return originalLink(src, dst)
	}
	node, err := f.e.nodeLink(source)
	if err != nil {
		t.Fatal(err)
	}
	if !published {
		t.Fatal("publication hook was not reached")
	}
	linkNode = originalLink
	gid := requireNodeGID(t, f.e, &VM{ID: "a", UserID: "a"})
	if err := f.e.nodeReadable(node, gid); err != nil {
		t.Fatal(err)
	}
	// A crash after assignment must not lose the reservation or reset a reused inode.
	restarted := &Engine{cfg: f.e.cfg, dropVMM: true}
	if err := restarted.prepareNodeAccess(source); err != nil {
		t.Fatal(err)
	}
	assertNodeMode(t, source, gid, 0o640)
	assertNodeMode(t, node, gid, 0o640)
	unknown := filepath.Join(root, "external", "unknown.img")
	writeFile(t, unknown, []byte("unknown"))
	unreserved := vmmNodeGIDBase + (gid-vmmNodeGIDBase+1)%vmmIDSlots
	if err := os.Chown(unknown, vmmUIDBase+18, int(unreserved)); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(unknown, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := restarted.prepareNodeAccess(unknown); err != nil {
		t.Fatal(err)
	}
	assertNodeMode(t, unknown, 0, 0o600)
}

func TestMigrateNodeAccessRemovesLegacySharedGroup(t *testing.T) {
	f, root := rootNodeAccessFixture(t)
	f.e.dropVMM = false // Reproduce nodes published by the pre-change daemon.
	snapA := f.snapshot("a", "saved", f.freshRoot("source-a"))
	rootA := f.resume("a", snapA)
	nodeA := backingOf(t, rootA)
	snapB := f.snapshot("b", "saved", f.freshRoot("source-b"))
	nodeB := backingOf(t, f.resume("b", snapB))
	unknownSource := filepath.Join(f.data, "unrecorded", "unknown.img")
	writeFile(t, unknownSource, []byte("unknown node"))
	unknown, err := f.e.nodeLink(unknownSource)
	if err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(f.data, "images", "legacy-node.img")
	if err := os.Link(nodeA, alias); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{nodeA, nodeB, unknown, f.base, nodeMetadataPath(nodeA)} {
		if err := os.Chown(path, 0, vmmGroup); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(path, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Chown(nodesDir(f.data), 0, vmmGroup); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(nodesDir(f.data), 0o710); err != nil {
		t.Fatal(err)
	}
	opened, err := os.Open(nodeA)
	if err != nil {
		t.Fatal(err)
	}
	defer opened.Close()
	f.e.dropVMM = true
	if err := f.e.vmmReadable(alias); err == nil {
		t.Fatal("a legacy image alias re-exposed a private node by inode")
	}
	if err := f.e.migrateNodeAccess(); err != nil {
		t.Fatal(err)
	}
	ga := requireNodeGID(t, f.e, &VM{ID: "a", UserID: "a"})
	gb := requireNodeGID(t, f.e, &VM{ID: "b", UserID: "b"})
	assertNodeMode(t, nodeA, ga, 0o640)
	assertNodeMode(t, alias, ga, 0o640)
	assertNodeMode(t, nodeB, gb, 0o640)
	assertNodeMode(t, unknown, 0, 0o600)
	assertNodeMode(t, nodeMetadataPath(nodeA), 0, 0o600)
	assertNodeMode(t, f.base, vmmGroup, 0o644)
	if err := f.e.vmmReadable(alias); err == nil {
		t.Fatal("a migrated node's legacy alias was made universally readable")
	}
	binary := nodeDACTestBinary(t, root)
	runNodeDAC(t, binary, nodeDACCredentials{UID: vmmUIDBase + 500, GID: vmmUIDBase + 500, Groups: []uint32{vmmGroup}},
		nodeDACPlan{Denied: []string{nodeA, nodeB}, ReadInherited: true}, opened)
	// With all owner anchors gone, a second startup still preserves retained ownership.
	if err := os.RemoveAll(snapA); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(filepath.Dir(rootA)); err != nil {
		t.Fatal(err)
	}
	if err := (&Engine{cfg: f.e.cfg, dropVMM: true}).migrateNodeAccess(); err != nil {
		t.Fatal(err)
	}
	assertNodeMode(t, nodeA, ga, 0o640)
}

func TestNodeAccessMigrationFailsClosedOnLostRegistry(t *testing.T) {
	f, _ := rootNodeAccessFixture(t)
	source := filepath.Join(f.data, "source.img")
	writeFile(t, source, []byte("retained node"))
	node, err := f.e.nodeLink(source)
	if err != nil {
		t.Fatal(err)
	}
	gid := requireNodeGID(t, f.e, &VM{ID: "a", UserID: "a"})
	if err := f.e.nodeReadable(node, gid); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(f.data, nodeGroupsFile)); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := f.e.migrateNodeAccess(); err == nil {
			t.Fatal("lost registry was treated as a fresh namespace")
		}
		assertNodeMode(t, node, gid, 0o600)
		if _, err := f.e.nodeGIDFor(&VM{ID: "b", UserID: "b"}); err == nil {
			t.Fatal("a lost reservation was reused")
		}
	}
}

type nodeDACCredentials struct {
	UID, GID uint32
	Groups   []uint32
}

type nodeDACPlan struct {
	Read, Denied  []string
	ReadInherited bool
}

const nodeDACProcessEnv = "BHATTI_TEST_NODE_DAC_PROCESS"

// This child deliberately applies NO Landlock, seccomp, or production helper
// constructor. Only exec.Cmd's numeric SetCredential path enforces these opens.
func TestNodeAccessDACChild(t *testing.T) {
	payload := os.Getenv(nodeDACProcessEnv)
	if payload == "" {
		t.Skip("subprocess helper")
	}
	if os.Geteuid() == 0 {
		t.Fatal("DAC child still has root credentials")
	}
	var plan nodeDACPlan
	if err := json.Unmarshal([]byte(payload), &plan); err != nil {
		t.Fatal(err)
	}
	for _, path := range plan.Read {
		f, err := os.Open(path)
		if err != nil {
			t.Fatalf("open own chain %s: %v", path, err)
		}
		var b [1]byte
		_, err = f.Read(b[:])
		_ = f.Close()
		if err != nil {
			t.Fatalf("read own chain %s: %v", path, err)
		}
	}
	for _, path := range plan.Denied {
		f, err := os.Open(path)
		if f != nil {
			_ = f.Close()
		}
		if !errors.Is(err, syscall.EACCES) {
			t.Fatalf("foreign node %s: want EACCES, got %v", path, err)
		}
	}
	if plan.ReadInherited {
		f := os.NewFile(3, "adopted-chain")
		defer f.Close()
		var b [1]byte
		if _, err := f.ReadAt(b[:], 0); err != nil {
			t.Fatalf("migration invalidated an adopted helper's open chain FD: %v", err)
		}
	}
}

func nodeDACTestBinary(t *testing.T, root string) string {
	t.Helper()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	binary := filepath.Join(root, "dac-child")
	if err := cloneFile(t.Context(), executable, binary); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(binary, 0o755); err != nil {
		t.Fatal(err)
	}
	return binary
}

func runNodeDAC(t *testing.T, binary string, cred nodeDACCredentials, plan nodeDACPlan, inherited ...*os.File) {
	t.Helper()
	payload, err := json.Marshal(plan)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, binary, "-test.run=^TestNodeAccessDACChild$")
	cmd.Env = append(os.Environ(), nodeDACProcessEnv+"="+string(payload))
	cmd.SysProcAttr = &syscall.SysProcAttr{Credential: &syscall.Credential{
		Uid: cred.UID, Gid: cred.GID, Groups: cred.Groups,
	}}
	cmd.ExtraFiles = inherited
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("DAC child %d:%d groups %v: %v\n%s", cred.UID, cred.GID, cred.Groups, err, out)
	}
}

func nodePolicyCredentials(t *testing.T, policy string) nodeDACCredentials {
	t.Helper()
	data, err := os.ReadFile(policy)
	if err != nil {
		t.Fatal(err)
	}
	var cred nodeDACCredentials
	for _, record := range strings.Split(string(data), "\x00") {
		key, value, _ := strings.Cut(record, " ")
		if key != "uid" && key != "gid" && key != "group" {
			continue
		}
		n, err := strconv.ParseUint(value, 10, 32)
		if err != nil {
			t.Fatal(err)
		}
		switch key {
		case "uid":
			cred.UID = uint32(n)
		case "gid":
			cred.GID = uint32(n)
		case "group":
			cred.Groups = append(cred.Groups, uint32(n))
		}
	}
	if cred.UID == 0 || cred.GID == 0 {
		t.Fatal("policy omitted numeric helper credentials")
	}
	return cred
}

func nodeDACVM(t *testing.T, f *nodeFixture, owner, id string, layered bool) (*VM, VMSpec, nodeDACCredentials) {
	t.Helper()
	root := f.freshRoot(id)
	if layered {
		snap := f.snapshot(owner, id+"-one", root)
		parent := f.resume(id+"-parent", snap)
		snap = f.snapshot(owner, id+"-two", parent)
		root = f.resume(id+"-layered", snap)
	}
	vm := &VM{ID: id, UserID: owner, SandboxDir: filepath.Dir(root), SockDir: filepath.Join(f.e.cfg.SocketDir, id)}
	if err := os.MkdirAll(vm.SockDir, 0o700); err != nil {
		t.Fatal(err)
	}
	spec := VMSpec{RootDisk: root, RootDiskFormat: "qcow2"}
	if layered {
		snapshot := filepath.Join(f.data, "snapshots", owner, id+"-volumes")
		source := filepath.Join(snapshot, "vol0.img")
		writeFile(t, source, bytes.Repeat([]byte("private volume"), 512))
		parentDir := filepath.Join(f.data, "sandboxes", id+"-volume-parent")
		if err := os.MkdirAll(parentDir, 0o700); err != nil {
			t.Fatal(err)
		}
		var parent VMSpec
		if err := f.e.layerRestoredVolumes(t.Context(), parentDir, &parent, []restoreVol{{path: source, format: "raw"}}); err != nil {
			t.Fatal(err)
		}
		frozen := filepath.Join(snapshot, "vol1.img")
		if err := cloneFile(t.Context(), parent.Volumes[0].Path, frozen); err != nil {
			t.Fatal(err)
		}
		if err := f.e.layerRestoredVolumes(t.Context(), vm.SandboxDir, &spec, []restoreVol{{path: frozen, format: "qcow2"}}); err != nil {
			t.Fatal(err)
		}
	}
	if err := f.e.recordDiskBacking(&spec); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(vm.specPath(), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, policy, cleanup, err := f.e.confineLaunch(t.Context(), vm, spec, vm.specPath())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(cleanup)
	cred := nodePolicyCredentials(t, policy)
	gid := requireNodeGID(t, f.e, vm)
	if !slices.Contains(cred.Groups, vmmGroup) || !slices.Contains(cred.Groups, gid) {
		t.Fatalf("launch omitted universal-search/owner-node groups: %v, node %d", cred.Groups, gid)
	}
	return vm, spec, cred
}

func privateDACChain(spec VMSpec, base string) []string {
	var paths []string
	for _, layer := range spec.Backing {
		if layer.Path != base {
			paths = append(paths, layer.Path)
		}
	}
	for _, v := range spec.Volumes {
		for _, layer := range v.Backing {
			if layer.Path != base {
				paths = append(paths, layer.Path)
			}
		}
	}
	return paths
}

func TestNodeAccessDACWithoutLandlock(t *testing.T) {
	f, root := rootNodeAccessFixture(t)
	binary := nodeDACTestBinary(t, root)
	_, a, ca := nodeDACVM(t, f, "owner-a", "a", true)
	_, b, cb := nodeDACVM(t, f, "owner-b", "b", true)
	_, ordinary, co := nodeDACVM(t, f, "ordinary-attacker", "ordinary", false)
	aNodes, bNodes := privateDACChain(a, f.base), privateDACChain(b, f.base)
	if len(aNodes) < 4 || len(bNodes) < 4 {
		t.Fatalf("fixture lacks multi-generation root+volume chains: %v %v", aNodes, bNodes)
	}
	ga := requireNodeGID(t, f.e, &VM{ID: "a", UserID: "owner-a"})
	gb := requireNodeGID(t, f.e, &VM{ID: "b", UserID: "owner-b"})
	if len(privateDACChain(ordinary, f.base)) != 0 || len(ordinary.Volumes) != 0 {
		t.Fatal("ordinary attacker unexpectedly has private ancestors")
	}
	if slices.Contains(co.Groups, ga) || slices.Contains(co.Groups, gb) {
		t.Fatal("ordinary attacker inherited a victim's node group")
	}
	alias := filepath.Join(f.data, "images", "private-node-alias.img")
	if err := os.Link(aNodes[0], alias); err != nil {
		t.Fatal(err)
	}
	metadata := []string{nodeMetadataPath(aNodes[0]), filepath.Join(f.data, nodeGroupsFile), filepath.Join(f.data, nodeGroupsLock)}
	readA := append([]string{a.RootDisk, a.Volumes[0].Path, f.base}, aNodes...)
	readB := append([]string{b.RootDisk, b.Volumes[0].Path, f.base}, bNodes...)
	runNodeDAC(t, binary, ca, nodeDACPlan{Read: readA, Denied: append(slices.Clone(bNodes), metadata...)})
	runNodeDAC(t, binary, cb, nodeDACPlan{Read: readB, Denied: append(append(slices.Clone(aNodes), alias), metadata...)})
	denied := append(append(append(slices.Clone(aNodes), bNodes...), alias), metadata...)
	runNodeDAC(t, binary, co, nodeDACPlan{Read: []string{ordinary.RootDisk, f.base}, Denied: denied})
	for _, path := range aNodes {
		assertNodeMode(t, path, ga, 0o640)
	}
	for _, path := range bNodes {
		assertNodeMode(t, path, gb, 0o640)
	}
	assertNodeMode(t, nodesDir(f.data), vmmGroup, 0o710)
	if err := f.e.vmmReadable(alias); err == nil {
		t.Fatal("vmmReadable re-exposed a private inode alias")
	}
	if err := f.e.backingReadable(alias, gb); err == nil {
		t.Fatal("another owner stole a node through its image alias")
	}
}
