package krucible

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/sahil-shubham/bhatti/pkg/engine"
)

func TestNewReclaimsNodesLeftAfterACrash(t *testing.T) {
	if runtime.GOOS == "linux" && os.Geteuid() == 0 {
		if _, err := kvmGroup(); err != nil {
			t.Skipf("root engine initialization needs an accessible /dev/kvm: %v", err)
		}
	}
	f := newNodeFixture(t)
	snap := f.snapshot("u1", "golden", f.freshRoot("source"))
	root := f.resume("clone", snap)
	node := backingOf(t, root)
	if err := os.RemoveAll(snap); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(filepath.Dir(root)); err != nil {
		t.Fatal(err)
	}
	// A dead daemon cannot ask for the sweep after its last clone vanished.
	if _, err := os.Stat(node); err != nil {
		t.Fatal(err)
	}
	helper := filepath.Join(f.data, "helper")
	if err := os.WriteFile(helper, []byte("#!/bin/sh\nprintf '%s\\n' '{}'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if os.Geteuid() == 0 {
		if err := os.Chmod(filepath.Dir(f.data), 0o711); err != nil {
			t.Fatal(err)
		}
	}
	e, err := New(Config{DataDir: f.data, BaseImage: f.base, VMMBinary: helper,
		NetdBinary: helper, KernelImage: f.base, BlockRoot: true, SocketDir: shortSockDir(t), Cgroups: "none"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(e.Shutdown)
	if _, err := os.Stat(node); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("startup left an unreferenced node: %v", err)
	}
}

func TestFailedResumeReclaimsItsUnheldNode(t *testing.T) {
	f := newNodeFixture(t)
	f.e.cfg.BlockRoot, f.e.cfg.SocketDir = true, shortSockDir(t)
	snap := f.snapshot("u1", "golden", f.freshRoot("source"))
	disk := filepath.Join(snap, "rootfs.qcow2")
	original := linkNode
	linkNode = func(src, dst string) error {
		if err := original(src, dst); err != nil {
			return err
		}
		// Removing the snapshot by hand after its node is linked must not
		// leave that link forever when this resume subsequently fails.
		return os.Remove(src)
	}
	t.Cleanup(func() { linkNode = original })
	_, err := f.e.create(context.Background(), engine.SandboxSpec{Name: "failed"},
		createOpts{nodeDisk: disk, nodeFormat: "qcow2", snapshotDir: snap})
	if !errors.Is(err, errNoCheckpoint) {
		t.Fatalf("expected the unsupported memory restore to fail after disk preparation: %v", err)
	}
	f.e.waitNodeSweep()
	if left := nodeEntries(t, f.data); len(left) != 0 {
		t.Fatalf("failed resume left unheld nodes: %v", left)
	}
}

func captureControlPeer(t *testing.T, quiesce func(string) error, observers ...func(string) error) (string, <-chan string) {
	t.Helper()
	path := filepath.Join(shortSockDir(t), "capture.sock")
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	operations := make(chan string, 16)
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			line, err := bufio.NewReader(c).ReadString('\n')
			words := strings.Fields(line)
			if err == nil && len(words) > 0 {
				operations <- words[0]
				if words[0] == "SAVE" && len(words) == 2 {
					err = os.Mkdir(words[1], 0o700)
					if err == nil {
						err = os.WriteFile(filepath.Join(words[1], memoryFile), []byte("saved RAM"), 0o600)
					}
				}
				if err == nil && quiesce != nil && (words[0] == "PAUSE" || words[0] == "SAVE") {
					err = quiesce(words[0])
				}
				for _, observe := range observers {
					if err != nil {
						break
					}
					err = observe(words[0])
				}
			}
			if err == nil {
				_, _ = fmt.Fprintln(c, "OK")
			} else {
				_, _ = fmt.Fprintln(c, "ERR", err)
			}
			_ = c.Close()
		}
	}()
	return path, operations
}

func TestCheckpointPinsBackingThroughCaptureAndPublication(t *testing.T) {
	for _, entry := range []string{"untyped", "typed-default", "typed-memory", "typed-filesystem"} {
		for _, outcome := range []string{"success", "manifest-error"} {
			t.Run(entry+"/"+outcome, func(t *testing.T) {
				f := newNodeFixture(t)
				parent := f.snapshot("u1", "parent", f.freshRoot("original"))
				root := f.resume("source", parent)
				node := backingOf(t, root)
				if err := os.RemoveAll(parent); err != nil {
					t.Fatal(err)
				}
				snapRoot := filepath.Join(f.data, "snapshots", "u1")
				final := filepath.Join(snapRoot, "captured")
				probes := make(chan error, 2)
				ctlSock, ops := captureControlPeer(t, nil, func(op string) error {
					if op != "PAUSE" && op != "SAVE" && op != "RESUME" {
						return nil
					}
					probes <- tryBasesLock(f.data, syscall.LOCK_EX)
					if err := tryBasesLock(f.data, syscall.LOCK_SH); err != nil {
						return fmt.Errorf("checkpoint holds an exclusive rather than shared lease: %w", err)
					}
					if op == "RESUME" {
						if _, err := os.Stat(filepath.Join(final, snapshotManifestFile)); !errors.Is(err, os.ErrNotExist) {
							return fmt.Errorf("manifest published before the source resumed: %v", err)
						}
						if outcome == "manifest-error" {
							// Fail manifest publication after capture and resume,
							// rather than rejecting the source during preflight.
							return os.Mkdir(filepath.Join(final, snapshotManifestFile), 0o700)
						}
					}
					return nil
				})
				agSock, _, _ := durabilityPeer(t, 0)
				e, vm := durabilityEngine(t, "hot", agSock, ctlSock)
				e.cfg, e.caps.Checkpoint = f.e.cfg, true
				vm.UserID, vm.SandboxDir = "u1", filepath.Dir(root)
				vm.baseSpec = VMSpec{RootDisk: root, RootDiskFormat: "qcow2"}
				if err := e.recordDiskBacking(&vm.baseSpec); err != nil {
					t.Fatal(err)
				}
				ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
				defer cancel()
				var manifest any
				var err error
				if entry == "untyped" {
					manifest, err = e.Checkpoint(ctx, vm.ID, "u1", 0, "captured", snapRoot)
				} else {
					typ := strings.TrimPrefix(entry, "typed-")
					if typ == "default" {
						typ = ""
					}
					manifest, err = e.CheckpointTyped(ctx, vm.ID, "u1", 0, "captured", snapRoot, typ)
				}
				quiesce := "SAVE"
				if entry == "typed-filesystem" {
					quiesce = "PAUSE"
				}
				if got := readOperations(ops); !slices.Equal(got, []string{quiesce, "RESUME"}) {
					t.Fatalf("checkpoint did not capture and resume exactly once: %v", got)
				}
				for range 2 {
					select {
					case probe := <-probes:
						if !errors.Is(probe, syscall.EWOULDBLOCK) {
							t.Errorf("exclusive bases probe during capture/resume = %v, want a shared lease", probe)
						}
					default:
						t.Fatal("checkpoint missed a capture/resume lock probe")
					}
				}
				requireBasesLease(t, f.data, false)
				if outcome == "manifest-error" {
					if err == nil || !strings.Contains(err.Error(), "checkpoint: manifest:") || manifest != nil {
						t.Fatalf("expected a post-capture manifest failure, got %v, %v", manifest, err)
					}
					mustNotExist(t, final)
					return
				}
				if err != nil {
					t.Fatal(err)
				}
				local, found, err := readSnapshotManifest(final)
				if err != nil || !found || len(local.Backing) == 0 || local.Backing[0].Path != node {
					t.Fatalf("checkpoint lost its published node authority: %+v, %v, %v", local, found, err)
				}
				// Once the source vanishes, only the completed checkpoint's
				// manifest can keep its store-only ancestor alive.
				if err := os.RemoveAll(filepath.Dir(root)); err != nil {
					t.Fatal(err)
				}
				if removed, err := SweepNodes(f.data, false); err != nil || len(removed) != 0 {
					t.Fatalf("published checkpoint lost its ancestor: %v, %v", removed, err)
				}
				if _, err := os.Stat(node); err != nil {
					t.Fatal(err)
				}
			})
		}
	}
}

func TestCheckpointLockWaitHonorsContextBeforeCapture(t *testing.T) {
	for _, entry := range []string{"untyped", "typed-default", "typed-memory", "typed-filesystem"} {
		t.Run(entry, func(t *testing.T) {
			f := newNodeFixture(t)
			f.e.caps = VMMCapabilities{Checkpoint: true, Pause: true}
			release, err := lockBases(f.data, true)
			if err != nil {
				t.Fatal(err)
			}
			defer release()
			ctx, cancel := context.WithCancel(t.Context())
			cancel()
			snapRoot := filepath.Join(f.data, "snapshots", "u1")
			if entry == "untyped" {
				_, err = f.e.Checkpoint(ctx, "missing", "u1", 0, "cancelled", snapRoot)
			} else {
				typ := strings.TrimPrefix(entry, "typed-")
				if typ == "default" {
					typ = ""
				}
				_, err = f.e.CheckpointTyped(ctx, "missing", "u1", 0, "cancelled", snapRoot, typ)
			}
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("checkpoint reached source lookup/capture before a canceled lock wait: %v", err)
			}
			mustNotExist(t, filepath.Join(snapRoot, "cancelled"))
			release()
			requireBasesLease(t, f.data, false)
		})
	}
}

func TestCaptureCannotLaunderAnotherTenantsBackingNode(t *testing.T) {
	for _, which := range []string{"filesystem-root", "memory-root", "memory-volume", "saved-image"} {
		t.Run(which, func(t *testing.T) {
			testCaptureRejectsBackingRewrite(t, which, "foreign-node")
		})
	}
}

func TestCaptureRejectsAuthorizedBackingRewriteBeforeQuiescing(t *testing.T) {
	for _, replacement := range []string{"owner-image", "shared-base"} {
		t.Run(replacement, func(t *testing.T) {
			for _, which := range []string{"filesystem-root", "memory-root", "memory-volume", "saved-image"} {
				t.Run(which, func(t *testing.T) {
					testCaptureRejectsBackingRewrite(t, which, replacement)
				})
			}
		})
	}
}

func testCaptureRejectsBackingRewrite(t *testing.T, which, replacement string) {
	t.Helper()
	f := newNodeFixture(t)
	agSock, _, _ := durabilityPeer(t, 0)
	ctlSock, ops := captureControlPeer(t, nil)
	e, vm := durabilityEngine(t, "hot", agSock, ctlSock)
	e.cfg = f.e.cfg
	e.caps.Checkpoint = true
	vm.UserID = "u1"
	vm.baseSpec = VMSpec{RootDisk: f.freshRoot("source"), RootDiskFormat: "qcow2"}
	vm.SandboxDir = filepath.Dir(vm.baseSpec.RootDisk)
	tampered := vm.baseSpec.RootDisk
	if which == "memory-volume" {
		tampered = f.freshRoot("volume")
		vm.baseSpec.Volumes = []VMVolume{{BlockID: "vol0", Path: tampered, Format: "qcow2"}}
	}
	if err := e.recordDiskBacking(&vm.baseSpec); err != nil {
		t.Fatal(err)
	}
	capture := func(name string) (string, error) {
		if which == "saved-image" {
			dst := filepath.Join(f.data, "images", "u1", name+".img")
			if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
				return dst, err
			}
			return dst, e.SaveImage(context.Background(), vm.ID, dst)
		}
		typ := "memory"
		if which == "filesystem-root" {
			typ = "filesystem"
		}
		parent := filepath.Join(f.data, "snapshots", "u1")
		_, err := e.CheckpointTyped(context.Background(), vm.ID, "u1", 0, name, parent, typ)
		return filepath.Join(parent, name), err
	}
	copies := observeSnapshotCopies(t)
	if _, err := capture("allowed"); err != nil {
		t.Fatalf("capture of the recorded chain failed: %v", err)
	}
	if got := readOperations(ops); !slices.Contains(got, "RESUME") {
		t.Fatalf("successful capture did not exercise quiesce/resume: %v", got)
	}
	if got, err := os.ReadFile(copies); err != nil || len(got) == 0 {
		t.Fatalf("successful capture did not exercise the copy observer: %q, %v", got, err)
	}

	var target, targetFormat string
	switch replacement {
	case "foreign-node":
		other := f.snapshot("u2", "private", f.freshRoot("other"))
		var err error
		target, err = f.e.nodeLink(filepath.Join(other, "rootfs.qcow2"))
		if err != nil {
			t.Fatal(err)
		}
		targetFormat = "qcow2"
	case "owner-image":
		target = filepath.Join(f.data, "images", "u1", "alternate.qcow2")
		if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := createQcow2OverlayOn(target, f.base, "raw", uint64(f.size)); err != nil {
			t.Fatal(err)
		}
		targetFormat = "qcow2"
	case "shared-base":
		target = filepath.Join(f.data, "images", "bases", "alternate.ext4")
		writeBase(t, target, 1)
		targetFormat = "raw"
	default:
		t.Fatalf("unknown replacement %q", replacement)
	}
	if replacement != "foreign-node" && !e.vmmBaseAllowed(vm, target) {
		t.Fatalf("replacement is not independently authorized for this owner: %s", target)
	}
	recorded, err := json.Marshal(vm.baseSpec)
	if err != nil {
		t.Fatal(err)
	}
	// Even an otherwise authorized image cannot replace the exact chain the
	// daemon recorded. A header write must fail before any SAVE or disk copy.
	if err := createQcow2OverlayOn(tampered, target, targetFormat, uint64(f.size)); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(copies); err != nil {
		t.Fatal(err)
	}
	dst, err := capture("rejected")
	if err == nil {
		t.Fatal("capture trusted a chain different from the daemon's recorded list")
	}
	if got := readOperations(ops); len(got) != 0 {
		t.Fatalf("backing preflight rejection quiesced the running source: %v", got)
	}
	if got, err := os.ReadFile(copies); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("backing preflight rejection copied a disk: %q, %v", got, err)
	}
	if _, err := os.Stat(dst); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("rejected capture left its disk available: %v", err)
	}
	if after, err := json.Marshal(vm.baseSpec); err != nil || !bytes.Equal(after, recorded) {
		t.Fatalf("capture promoted a rewritten header into source metadata: %s, %v", after, err)
	}
	if vm.Status != "running" || vm.Thermal != "hot" {
		t.Fatalf("rejected capture changed source state: %s/%s", vm.Status, vm.Thermal)
	}
	if result, err := e.Exec(context.Background(), vm.ID, []string{"sync"}); err != nil || result.ExitCode != 0 {
		t.Fatalf("source stopped serving after the rejected capture: %+v, %v", result, err)
	}
}

func TestCaptureRevalidatesBackingAfterQuiescingBeforeCopy(t *testing.T) {
	for _, which := range []string{"filesystem-root", "memory-root", "memory-volume", "saved-image"} {
		t.Run(which, func(t *testing.T) {
			f := newNodeFixture(t)
			spec := VMSpec{RootDisk: f.freshRoot("source"), RootDiskFormat: "qcow2"}
			tampered := spec.RootDisk
			quiesce := "PAUSE"
			if strings.HasPrefix(which, "memory-") {
				quiesce = "SAVE"
			}
			if which == "memory-volume" {
				tampered = f.freshRoot("volume")
				spec.Volumes = []VMVolume{{BlockID: "vol0", Path: tampered, Format: "qcow2"}}
			}
			if err := f.e.recordDiskBacking(&spec); err != nil {
				t.Fatal(err)
			}
			target := filepath.Join(f.data, "images", "bases", "alternate.ext4")
			writeBase(t, target, 1)
			rewrites := make(chan error, 1)
			ctlSock, ops := captureControlPeer(t, func(op string) error {
				var err error
				if op != quiesce {
					err = fmt.Errorf("unexpected quiesce operation %s, want %s", op, quiesce)
				} else {
					// The initial preflight saw the recorded chain. The
					// helper changes it only as PAUSE/SAVE finishes.
					err = createQcow2OverlayOn(tampered, target, "raw", uint64(f.size))
				}
				rewrites <- err
				return err
			})
			agSock, _, _ := durabilityPeer(t, 0)
			e, vm := durabilityEngine(t, "hot", agSock, ctlSock)
			e.cfg, e.caps.Checkpoint = f.e.cfg, true
			vm.UserID, vm.baseSpec, vm.SandboxDir = "u1", spec, filepath.Dir(spec.RootDisk)
			if err := e.validateDiskBacking(vm, vm.baseSpec); err != nil {
				t.Fatalf("initial source chain does not match its recorded metadata: %v", err)
			}
			if !e.vmmBaseAllowed(vm, target) {
				t.Fatalf("quiesce rewrite target is not independently authorized: %s", target)
			}
			recorded, err := json.Marshal(vm.baseSpec)
			if err != nil {
				t.Fatal(err)
			}
			copies := observeSnapshotCopies(t)
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			var dst string
			if which == "saved-image" {
				dst = filepath.Join(f.data, "images", "u1", "rejected.img")
				if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
					t.Fatal(err)
				}
				err = e.SaveImage(ctx, vm.ID, dst)
			} else {
				typ := "memory"
				if which == "filesystem-root" {
					typ = "filesystem"
				}
				parent := filepath.Join(f.data, "snapshots", "u1")
				dst = filepath.Join(parent, "rejected")
				_, err = e.CheckpointTyped(ctx, vm.ID, "u1", 0, "rejected", parent, typ)
			}
			select {
			case rewriteErr := <-rewrites:
				if rewriteErr != nil {
					t.Fatalf("fake helper failed to rewrite the quiesced source: %v", rewriteErr)
				}
			default:
				t.Fatal("capture never quiesced its initially valid source")
			}
			if err == nil || !strings.Contains(err.Error(), target) || !strings.Contains(err.Error(), "recorded") {
				t.Errorf("capture did not reject the chain rewritten during quiesce: %v", err)
			}
			if got := readOperations(ops); !slices.Equal(got, []string{quiesce, "RESUME"}) {
				t.Errorf("quiesced rejection did not resume its source exactly once: %v", got)
			}
			if got, err := os.ReadFile(copies); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("capture copied a disk before checking the quiesced chain: %q, %v", got, err)
			}
			if _, err := os.Stat(dst); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("quiesced rejection left a captured disk available: %v", err)
			}
			if quiesce == "SAVE" {
				stages, err := os.ReadDir(filepath.Join(vm.SandboxDir, "save"))
				if err != nil || len(stages) != 0 {
					t.Errorf("quiesced rejection left helper SAVE stages: %v, %v", stages, err)
				}
			}
			if backingOf(t, tampered) != target || backingFormatOf(t, tampered) != "raw" {
				t.Error("fake helper did not install the on-disk backing rewrite")
			}
			if after, err := json.Marshal(vm.baseSpec); err != nil || !bytes.Equal(after, recorded) {
				t.Errorf("quiesced rejection promoted the new chain into source metadata: %s, %v", after, err)
			}
			if vm.Status != "running" || vm.Thermal != "hot" {
				t.Errorf("quiesced rejection changed source state: %s/%s", vm.Status, vm.Thermal)
			}
			if result, err := e.Exec(ctx, vm.ID, []string{"sync"}); err != nil || result.ExitCode != 0 {
				t.Errorf("source stopped serving after quiesced rejection: %+v, %v", result, err)
			}
		})
	}
}

func observeSnapshotCopies(t *testing.T) string {
	t.Helper()
	cp, err := exec.LookPath("cp")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	log := filepath.Join(dir, "copies")
	script := fmt.Sprintf("#!/bin/sh\nprintf '%%s\\n' \"$*\" >> %q\nexec %q \"$@\"\n", log, cp)
	if err := os.WriteFile(filepath.Join(dir, "cp"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return log
}

func TestCheckpointResumeKeepsQcow2LookingRawVolumeOpaque(t *testing.T) {
	for _, targetKind := range []string{"foreign-node", "host-secret"} {
		t.Run(targetKind, func(t *testing.T) {
			e := fakeReseedEngine(t, "supported")
			e.confineVMM, e.caps.Pause = true, true
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()

			var target, targetFormat, privateDir string
			if targetKind == "foreign-node" {
				privateDir = filepath.Join(e.cfg.DataDir, "snapshots", "u2", "private")
				if err := os.MkdirAll(privateDir, 0o700); err != nil {
					t.Fatal(err)
				}
				private := filepath.Join(privateDir, "rootfs.qcow2")
				if err := createQcow2OverlayOn(private, e.cfg.BaseImage, "raw", 1<<20); err != nil {
					t.Fatal(err)
				}
				var err error
				target, err = e.nodeLink(private)
				if err != nil {
					t.Fatal(err)
				}
				targetFormat = "qcow2"
			} else {
				target = filepath.Join(e.cfg.DataDir, "private", "secret.img")
				writeFile(t, target, []byte("host-only secret"))
				targetFormat = "raw"
			}
			raw := filepath.Join(e.cfg.DataDir, "volumes", "u1", "persistent.img")
			if err := os.MkdirAll(filepath.Dir(raw), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := createQcow2OverlayOn(raw, target, targetFormat, 1<<20); err != nil {
				t.Fatal(err)
			}
			before, err := os.ReadFile(raw)
			if err != nil {
				t.Fatal(err)
			}
			if got, err := qcow2Backing(raw); err != nil || got != target {
				t.Fatalf("raw fixture lacks its realistic forged backing header: %q, %v", got, err)
			}
			if got, err := qcow2VirtualSize(raw); err != nil || got != 1<<20 {
				t.Fatalf("raw fixture lacks a valid qcow2 virtual size: %d, %v", got, err)
			}

			spec := noNetworkSpec("opaque-source")
			spec.UserID = "u1"
			spec.ResolvedVolumes = []engine.ResolvedVolume{{FilePath: raw, Mount: "/data"}}
			source, err := e.Create(ctx, spec)
			if err != nil {
				t.Fatalf("create treated explicit raw guest bytes as backing authority: %v", err)
			}
			sourceVM, err := e.getVM(source.ID)
			if err != nil {
				t.Fatal(err)
			}
			sourceHelper := assertSnapshotHelperCannotAccess(t, sourceVM, target)
			if len(sourceHelper.Volumes) != 1 || sourceHelper.Volumes[0].Format != "raw" ||
				len(sourceHelper.Volumes[0].Backing) != 0 {
				t.Fatalf("source helper reinterpreted its raw volume: %+v", sourceHelper.Volumes)
			}

			parent := filepath.Join(e.cfg.DataDir, "snapshots", "u1")
			captured, err := e.Checkpoint(ctx, source.ID, "u1", 0, "opaque", parent)
			if err != nil {
				t.Fatalf("checkpoint followed an opaque raw volume's forged header: %v", err)
			}
			m := captured.(krucibleSnapManifest)
			if m.DiskFormat != "qcow2" || len(m.Volumes) != 1 || m.Volumes[0].Format != "raw" ||
				len(m.Volumes[0].Backing) != 0 {
				t.Fatalf("checkpoint did not preserve independent disk formats and chains: %+v", m)
			}
			snap := filepath.Join(parent, "opaque")
			frozen := filepath.Join(snap, m.Volumes[0].File)
			manifest := filepath.Join(snap, snapshotManifestFile)
			manifestBefore, err := os.ReadFile(manifest)
			if err != nil {
				t.Fatal(err)
			}
			assertSnapshotHelperCannotAccess(t, sourceVM, target, manifest)

			// The API's SQLite copy may be broader than the local record. It
			// must not turn raw guest data into a qcow2 graph on restore.
			sqlite := m
			sqlite.Backing = append(slices.Clone(m.Backing), diskLayer{Path: target, Format: targetFormat})
			sqlite.Volumes = slices.Clone(m.Volumes)
			sqlite.Volumes[0].Format = "qcow2"
			sqlite.Volumes[0].Backing = []diskLayer{{Path: target, Format: targetFormat}}
			sqliteJSON, err := json.Marshal(sqlite)
			if err != nil {
				t.Fatal(err)
			}
			restored, err := e.ResumeFromManifestJSON(ctx, snap, sqliteJSON, "opaque-restored", "u1")
			if err != nil {
				t.Fatalf("resume let SQLite override the local raw-volume record: %v", err)
			}
			restoredVM, err := e.getVM(restored.ID)
			if err != nil {
				t.Fatal(err)
			}
			helper := assertSnapshotHelperCannotAccess(t, restoredVM, target, manifest)
			if len(helper.Volumes) != 1 || helper.Volumes[0].Format != "qcow2" ||
				len(helper.Volumes[0].Backing) != 1 || helper.Volumes[0].Backing[0].Format != "raw" {
				t.Fatalf("restored helper did not stop at its declared raw ancestor: %+v", helper.Volumes)
			}
			rawNode := helper.Volumes[0].Backing[0].Path
			if backingOf(t, helper.Volumes[0].Path) != rawNode || backingFormatOf(t, helper.Volumes[0].Path) != "raw" {
				t.Fatal("restored volume's on-disk header disagrees with its typed raw ancestor")
			}
			nodeInfo, err := os.Stat(rawNode)
			if err != nil {
				t.Fatal(err)
			}
			frozenInfo, err := os.Stat(frozen)
			if err != nil || !os.SameFile(nodeInfo, frozenInfo) {
				t.Fatalf("restore did not layer over the frozen raw bytes: %v", err)
			}
			if after, err := os.ReadFile(manifest); err != nil || !bytes.Equal(after, manifestBefore) {
				t.Fatalf("resume changed the authoritative local manifest: %q, %v", after, err)
			}
			if result, err := e.Exec(ctx, source.ID, []string{"sync"}); err != nil || result.ExitCode != 0 {
				t.Fatalf("checkpoint/restore stopped the source: %+v, %v", result, err)
			}

			if privateDir != "" {
				if err := os.RemoveAll(privateDir); err != nil {
					t.Fatal(err)
				}
			}
			removed, err := SweepNodes(e.cfg.DataDir, false)
			if err != nil {
				t.Fatalf("GC interpreted a raw guest header as a dependency: %v", err)
			}
			if targetKind == "foreign-node" {
				if !slices.Contains(removed, target) {
					t.Fatalf("raw guest bytes pinned an unrelated owner's node: %v", removed)
				}
				if _, err := os.Stat(target); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("forged backing target survived GC: %v", err)
				}
			} else if secret, err := os.ReadFile(target); err != nil || string(secret) != "host-only secret" {
				t.Fatalf("snapshot lifecycle changed the host secret: %q, %v", secret, err)
			}
			for _, path := range []string{raw, frozen, rawNode} {
				if got, err := os.ReadFile(path); err != nil || !bytes.Equal(got, before) {
					t.Fatalf("raw disk bytes changed across checkpoint/resume/GC at %s: %v", path, err)
				}
			}
			if err := e.Destroy(ctx, restored.ID); err != nil {
				t.Fatal(err)
			}
			if err := e.RemoveSnapshot(ctx, snap); err != nil {
				t.Fatal(err)
			}
			e.waitNodeSweep()
			if left := nodeEntries(t, e.cfg.DataDir); len(left) != 0 {
				t.Fatalf("destroying the final restore left snapshot nodes: %v", left)
			}
		})
	}
}

func TestCreateRawRootKeepsDefaultBaseBytesOpaque(t *testing.T) {
	t.Setenv("KRUCIBLE_ROOT_RAW", "1")
	for _, contents := range []string{"plain-raw", "qcow2-looking-raw"} {
		t.Run(contents, func(t *testing.T) {
			e := fakeReseedEngine(t, "supported")
			e.confineVMM = true
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			forbidden := []string{e.cfg.BaseImage}
			var secret string
			if contents == "qcow2-looking-raw" {
				secret = filepath.Join(e.cfg.DataDir, "private", "secret.img")
				writeFile(t, secret, []byte("host-only secret"))
				if err := createQcow2OverlayOn(e.cfg.BaseImage, secret, "raw", 1<<20); err != nil {
					t.Fatal(err)
				}
				if got, err := qcow2Backing(e.cfg.BaseImage); err != nil || got != secret {
					t.Fatalf("raw base fixture lacks its realistic forged header: %q, %v", got, err)
				}
				forbidden = append(forbidden, secret)
			}
			before, err := os.ReadFile(e.cfg.BaseImage)
			if err != nil {
				t.Fatal(err)
			}
			spec := noNetworkSpec("raw-root")
			spec.UserID = "u1"
			created, err := e.Create(ctx, spec)
			if err != nil {
				t.Fatalf("explicit raw-root create rejected its %s default base: %v", contents, err)
			}
			vm, err := e.getVM(created.ID)
			if err != nil {
				t.Fatal(err)
			}
			helper := assertSnapshotHelperCannotAccess(t, vm, forbidden...)
			if helper.RootDiskFormat != "raw" || len(helper.Backing) != 0 ||
				vm.baseSpec.RootDiskFormat != "raw" || len(vm.baseSpec.Backing) != 0 {
				t.Fatalf("raw-root mode promoted guest bytes into backing metadata: %+v, %+v", helper, vm.baseSpec)
			}
			for _, path := range []string{e.cfg.BaseImage, helper.RootDisk} {
				if got, err := os.ReadFile(path); err != nil || !bytes.Equal(got, before) {
					t.Errorf("raw-root create changed bytes at %s: %v", path, err)
				}
			}
			if secret != "" {
				if got, err := os.ReadFile(secret); err != nil || string(got) != "host-only secret" {
					t.Errorf("raw-root create changed its forged backing target: %q, %v", got, err)
				}
			}
			if result, err := e.Exec(ctx, created.ID, []string{"sync"}); err != nil || result.ExitCode != 0 {
				t.Fatalf("raw-root sandbox is not serving after creation: %+v, %v", result, err)
			}
		})
	}
}

func TestSavedForkImagePreservesMetadataOnlyOwnerBackingAuthority(t *testing.T) {
	e := fakeReseedEngine(t, "supported")
	e.confineVMM, e.caps.Pause = true, true
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	sourceSpec := noNetworkSpec("source")
	sourceSpec.UserID = "u1"
	source, err := e.Create(ctx, sourceSpec)
	if err != nil {
		t.Fatal(err)
	}
	fork, err := e.Fork(ctx, source.ID, "fork")
	if err != nil {
		t.Fatal(err)
	}
	forkVM, err := e.getVM(fork.ID)
	if err != nil {
		t.Fatal(err)
	}
	forkDir := forkVM.SandboxDir
	forkBacking := slices.Clone(forkVM.baseSpec.Backing)
	if len(forkBacking) < 2 || forkBacking[0].Format != "qcow2" ||
		filepath.Dir(forkBacking[0].Path) != nodesDir(e.cfg.DataDir) {
		t.Fatalf("fork fixture is not layered over its frozen checkpoint node: %+v", forkBacking)
	}
	image := filepath.Join(e.cfg.DataDir, "images", "u1", "fork.img")
	if err := os.MkdirAll(filepath.Dir(image), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := e.SaveImage(ctx, fork.ID, image); err != nil {
		t.Fatalf("save image from fork: %v", err)
	}
	imageBefore, err := os.ReadFile(image)
	if err != nil {
		t.Fatal(err)
	}
	sidecar := nodeMetadataPath(image)
	sidecarBefore, err := os.ReadFile(sidecar)
	if err != nil {
		t.Fatalf("save image did not preserve its typed provenance sidecar: %v", err)
	}
	var metadata nodeMetadata
	if found, err := readDaemonMetadata(sidecar, &metadata); err != nil || !found ||
		metadata.Format != "qcow2" || !slices.Equal(metadata.Backing, forkBacking) {
		t.Fatalf("saved image sidecar lost the fork's exact recorded chain: %+v, %t, %v", metadata, found, err)
	}
	if err := e.Destroy(ctx, fork.ID); err != nil {
		t.Fatal(err)
	}
	e.waitNodeSweep()
	if _, err := os.Stat(forkDir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("destroyed fork still supplies an owner-provenance record: %v", err)
	}
	node := forkBacking[0].Path
	nodeInfo, err := os.Stat(node)
	if err != nil {
		t.Fatalf("destroying the fork reclaimed a node held by its saved image: %v", err)
	}
	nodeKey, ok := keyOf(nodeInfo)
	if !ok {
		t.Fatal("saved fork node has no observable file identity")
	}
	var nodeRecord nodeMetadata
	if found, err := readDaemonMetadata(nodeMetadataPath(node), &nodeRecord); err != nil || !found ||
		nodeRecord.Format != forkBacking[0].Format || !slices.Equal(nodeRecord.Backing, forkBacking[1:]) {
		t.Fatalf("fork's preserved node lost its exact daemon sidecar: %+v, %t, %v", nodeRecord, found, err)
	}

	// With the fork and its staging snapshot gone, only the saved image's
	// daemon sidecar may authorize its node. The image header alone must not.
	held := filepath.Join(t.TempDir(), "image-metadata")
	if err := os.Rename(sidecar, held); err != nil {
		t.Fatal(err)
	}
	withoutMetadata, lookupErr := ownedNodeKeys(e.cfg.DataDir, "u1")
	restoreErr := os.Rename(held, sidecar)
	if lookupErr != nil || restoreErr != nil {
		t.Fatalf("inspect metadata-only owner authority: %v; restore sidecar: %v", lookupErr, restoreErr)
	}
	if withoutMetadata[nodeKey] {
		t.Fatal("saved image header invented owner authority without its daemon sidecar")
	}
	withMetadata, err := ownedNodeKeys(e.cfg.DataDir, "u1")
	if err != nil || !withMetadata[nodeKey] {
		t.Fatalf("saved image sidecar did not preserve its owner's node authority after fork destruction: %v", err)
	}
	otherOwner, err := ownedNodeKeys(e.cfg.DataDir, "u2")
	if err != nil || otherOwner[nodeKey] {
		t.Fatalf("saved image metadata authorized a different owner: %v", err)
	}

	createSpec := noNetworkSpec("from-image")
	createSpec.UserID, createSpec.BaseImage = "u1", image
	created, err := e.Create(ctx, createSpec)
	if err != nil {
		t.Fatalf("create from saved fork image after destroying the fork: %v", err)
	}
	vm, err := e.getVM(created.ID)
	if err != nil {
		t.Fatal(err)
	}
	helper := assertSnapshotHelperCannotAccess(t, vm, image, sidecar)
	if helper.RootDiskFormat != "qcow2" || !slices.Equal(helper.Backing, metadata.Backing) {
		t.Fatalf("new helper did not inherit the saved image's exact typed graph: %+v", helper.Backing)
	}
	if actual, err := readDiskChain(helper.RootDisk, helper.RootDiskFormat, nil); err != nil ||
		!slices.Equal(actual, metadata.Backing) {
		t.Fatalf("saved image's new root header differs from its preserved graph: %+v, %v", actual, err)
	}
	for path, before := range map[string][]byte{image: imageBefore, sidecar: sidecarBefore} {
		if after, err := os.ReadFile(path); err != nil || !bytes.Equal(after, before) {
			t.Errorf("create changed its saved image or provenance record %s: %v", path, err)
		}
	}
	if result, err := e.Exec(ctx, created.ID, []string{"sync"}); err != nil || result.ExitCode != 0 {
		t.Fatalf("sandbox created from the saved fork image is not serving: %+v, %v", result, err)
	}
}
