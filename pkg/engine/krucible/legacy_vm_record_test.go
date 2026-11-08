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
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/sahil-shubham/bhatti/pkg/agent"
)

type legacyVMRecordFixture struct {
	e         *Engine
	vm        *VM
	ancestor  string
	stable    string
	originals map[string][]byte
}

func stripLegacyVMBackingFormat(t *testing.T, path string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	_, writeErr := f.WriteAt(make([]byte, 8), int64(qcow2HeaderLen))
	if err := errors.Join(writeErr, f.Close()); err != nil {
		t.Fatal(err)
	}
	if format, err := qcow2BackingFormat(path); err != nil || format != "" {
		t.Fatalf("legacy VM fixture still records a backing format: %q, %v", format, err)
	}
}

func newLegacyVMRecordFixture(t *testing.T) *legacyVMRecordFixture {
	t.Helper()
	e := fakeReseedEngine(t, "supported")
	base := filepath.Join(e.cfg.DataDir, "images", "bases", "legacy-base.ext4")
	if err := os.MkdirAll(filepath.Dir(base), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(e.cfg.BaseImage, base); err != nil {
		t.Fatal(err)
	}
	e.cfg.BaseImage = base
	e.confineVMM, e.caps.Pause = true, true
	specPath, ctl := sandboxPaths(t, e, "legacy")
	dir, socks := filepath.Dir(specPath), filepath.Dir(ctl)
	stable := filepath.Join(e.cfg.DataDir, "images", "u1", "stable.qcow2")
	ancestor := filepath.Join(filepath.Dir(stable), "legacy.qcow2")
	if err := os.MkdirAll(filepath.Dir(stable), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := createQcow2OverlayOn(stable, e.cfg.BaseImage, "raw", 1<<20); err != nil {
		t.Fatal(err)
	}
	if err := createQcow2OverlayOn(ancestor, filepath.Base(stable), "qcow2", 1<<20); err != nil {
		t.Fatal(err)
	}
	stripLegacyVMBackingFormat(t, ancestor)
	relative, err := filepath.Rel(dir, ancestor)
	if err != nil {
		t.Fatal(err)
	}
	root, volume := filepath.Join(dir, "root.qcow2"), filepath.Join(dir, "data.qcow2")
	for _, path := range []string{root, volume} {
		if err := createQcow2OverlayOn(path, relative, "qcow2", 1<<20); err != nil {
			t.Fatal(err)
		}
		stripLegacyVMBackingFormat(t, path)
	}
	config := filepath.Join(dir, "config.json")
	writeFile(t, config, []byte(`{"token":"legacy-token"}`))
	// Spell out the old wire shape: known root/volume drivers, no disk_backing
	// keys. Marshaling today's VMSpec would instead write typed-chain fields.
	record := map[string]any{
		"id": "legacy", "name": "legacy", "user_id": "u1", "sandbox_dir": dir, "sock_dir": socks,
		"control_uds": filepath.Join(socks, "c.sock"), "forward_uds": filepath.Join(socks, "f.sock"),
		"ctl_sock_uds": ctl, "mem_mib": 64, "thermal": "cold", "status": "stopped",
		"token": "legacy-token", "log_path": filepath.Join(dir, "vmm.log"),
		"net_policy": noNetworkSpec("").NetPolicy,
		"base_spec": map[string]any{
			"root_disk": root, "root_disk_format": "qcow2", "vcpus": 1, "mem_mib": 64,
			"pid1": true, "exec_path": "/init.krun", "vsock_control_uds": filepath.Join(socks, "c.sock"),
			"vsock_forward_uds": filepath.Join(socks, "f.sock"), "control_socket_uds": ctl,
			"volumes": []map[string]any{{"block_id": "data", "path": volume, "format": "qcow2"}},
		},
	}
	data, err := json.MarshalIndent(record, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(data, []byte("disk_backing")) {
		t.Fatal("legacy VM fixture unexpectedly contains modern disk authority")
	}
	writeFile(t, stateFilePath(dir), data)
	rec := readRecord(t, dir)
	if rec.BaseSpec.RootDiskFormat != "qcow2" || rec.BaseSpec.Backing != nil ||
		len(rec.BaseSpec.Volumes) != 1 || rec.BaseSpec.Volumes[0].Format != "qcow2" || rec.BaseSpec.Volumes[0].Backing != nil {
		t.Fatalf("old VM record lost its known drivers or acquired typed chains: %+v", rec.BaseSpec)
	}
	if err := e.recover(); err != nil {
		t.Fatal(err)
	}
	vm, err := e.getVM(rec.ID)
	if err != nil {
		t.Fatal(err)
	}
	if vm.Status != "stopped" || vm.Thermal != "cold" || vm.baseSpec.Backing != nil || vm.baseSpec.Volumes[0].Backing != nil {
		t.Fatalf("recovery did not preserve the cold legacy VM: %s/%s, %+v", vm.Status, vm.Thermal, vm.baseSpec)
	}
	f := &legacyVMRecordFixture{e: e, vm: vm, ancestor: ancestor, stable: stable, originals: map[string][]byte{}}
	for _, path := range []string{root, volume, ancestor, stable, e.cfg.BaseImage, config} {
		f.originals[path], err = os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
	}
	return f
}

func (f *legacyVMRecordFixture) assertSourcesUnchanged(t *testing.T) {
	t.Helper()
	for path, before := range f.originals {
		if after, err := os.ReadFile(path); err != nil || !bytes.Equal(after, before) {
			t.Errorf("capture changed a legacy source file %s: %v", path, err)
		}
		if _, err := os.Lstat(nodeMetadataPath(path)); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("capture promoted an original source into typed sidecar authority: %s, %v", path, err)
		}
	}
	if f.vm.baseSpec.Backing != nil || f.vm.baseSpec.Volumes[0].Backing != nil {
		t.Error("capture promoted inferred chains into the running VM")
	}
	rec := readRecord(t, f.vm.SandboxDir)
	if rec.BaseSpec.Backing != nil || rec.BaseSpec.Volumes[0].Backing != nil {
		t.Error("capture persisted inferred, unnormalized source chains")
	}
}

func captureLegacyVM(t *testing.T, f *legacyVMRecordFixture, kind, name string) (string, error) {
	t.Helper()
	if kind == "saved-image" {
		dst := filepath.Join(f.e.cfg.DataDir, "images", "u1", name+".ext4")
		return dst, f.e.SaveImage(t.Context(), f.vm.ID, dst)
	}
	parent := filepath.Join(f.e.cfg.DataDir, "snapshots", "u1")
	_, err := f.e.CheckpointTyped(t.Context(), f.vm.ID, "u1", 0, name, parent, kind)
	return filepath.Join(parent, name), err
}

func serveLegacyVMCapture(t *testing.T, f *legacyVMRecordFixture, quiesce func(string) error, observers ...func(string) error) <-chan string {
	t.Helper()
	ctl, operations := captureControlPeer(t, quiesce, observers...)
	agentSock, _, _ := durabilityPeer(t, 0)
	f.vm.CtlSockUDS = ctl
	f.vm.Status, f.vm.Thermal = "running", "hot"
	f.vm.Agent = agent.NewKrucibleClient(agentSock, "", f.vm.Token)
	return operations
}

func TestLegacyVMRecordMissingFormatsColdStartsAndCaptures(t *testing.T) {
	f := newLegacyVMRecordFixture(t)
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	// Start traverses the actual cold-launch/confineLaunch seam, using the
	// existing protocol-speaking helper rather than requiring a hypervisor.
	if err := f.e.Start(ctx, f.vm.ID); err != nil {
		t.Fatalf("known-qcow2 legacy VM cannot cold start: %v", err)
	}
	policy, err := os.ReadFile(filepath.Join(f.vm.SandboxDir, "vmm.policy"))
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{f.ancestor, f.stable, f.e.cfg.BaseImage} {
		if !bytes.Contains(policy, []byte(fmt.Sprintf("path %x %s\x00", vmmReadFile, path))) {
			t.Errorf("legacy cold-launch policy omitted its authorized read-only ancestor %s", path)
		}
	}
	f.assertSourcesUnchanged(t)
	checkDisk := func(t *testing.T, path, source, format string, chain []diskLayer) {
		t.Helper()
		if format != "qcow2" || chain == nil {
			t.Fatalf("published legacy copy lacks typed format authority: %s, %q, %+v", path, format, chain)
		}
		if err := validateRecordedDisk(path, format, chain); err != nil {
			t.Fatalf("published legacy copy is not strictly valid: %s: %v", path, err)
		}
		actual, err := readDiskChain(path, format, nil)
		if err != nil || !slices.Equal(actual, chain) {
			t.Fatalf("published authority differs from its normalized graph: %+v, %+v, %v", actual, chain, err)
		}
		if len(chain) != 3 || chain[0].Path == f.ancestor || chain[0].Format != "qcow2" ||
			chain[1] != (diskLayer{Path: f.stable, Format: "qcow2"}) ||
			chain[2] != (diskLayer{Path: f.e.cfg.BaseImage, Format: "raw"}) {
			t.Fatalf("normalization lost a layer or copied an already-strict ancestor: %+v", chain)
		}
		for _, pair := range [][2]string{{path, source}, {chain[0].Path, f.ancestor}} {
			after, err := os.ReadFile(pair[0])
			before := f.originals[pair[1]]
			if err != nil || len(after) != len(before) || !bytes.Equal(after[qcow2ClusterSize:], before[qcow2ClusterSize:]) {
				t.Fatalf("normalization changed clusters outside the legacy header: %s, %v", pair[1], err)
			}
		}
	}
	for _, kind := range []string{"filesystem", "memory", "saved-image"} {
		t.Run(kind, func(t *testing.T) {
			dst, err := captureLegacyVM(t, f, kind, "captured-"+kind)
			if err != nil {
				t.Fatalf("legacy %s capture failed: %v", kind, err)
			}
			if kind == "saved-image" {
				var metadata nodeMetadata
				found, err := readDaemonMetadata(nodeMetadataPath(dst), &metadata)
				if err != nil || !found {
					t.Fatalf("saved legacy image has no authoritative sidecar: %v, %v", found, err)
				}
				checkDisk(t, dst, f.vm.baseSpec.RootDisk, metadata.Format, metadata.Backing)
			} else {
				m, found, err := readSnapshotManifest(dst)
				if err != nil || !found {
					t.Fatalf("legacy checkpoint has no authoritative manifest: %v, %v", found, err)
				}
				checkDisk(t, filepath.Join(dst, m.DiskFile), f.vm.baseSpec.RootDisk, m.DiskFormat, m.Backing)
				wantVolumes := 0
				if kind == "memory" {
					wantVolumes = 1
				}
				if len(m.Volumes) != wantVolumes {
					t.Fatalf("capture changed the expected device set: %+v", m.Volumes)
				}
				if wantVolumes != 0 {
					v := m.Volumes[0]
					checkDisk(t, filepath.Join(dst, v.File), f.vm.baseSpec.Volumes[0].Path, v.Format, v.Backing)
				}
			}
			f.assertSourcesUnchanged(t)
			if result, err := f.e.Exec(ctx, f.vm.ID, []string{"sync"}); err != nil || result.ExitCode != 0 {
				t.Fatalf("legacy source stopped serving after capture: %+v, %v", result, err)
			}
		})
	}
}

func TestLegacyVMRecordRejectsOutsideBackingBeforeAnyRead(t *testing.T) {
	for _, disk := range []string{"root", "volume"} {
		for _, kind := range []string{"cold-launch", "filesystem", "memory", "saved-image"} {
			t.Run(disk+"/"+kind, func(t *testing.T) {
				f := newLegacyVMRecordFixture(t)
				if kind != "cold-launch" {
					serveLegacyVMCapture(t, f, nil)
				}
				outside := filepath.Join(t.TempDir(), "outside.qcow2")
				if err := syscall.Mkfifo(outside, 0o600); err != nil {
					t.Fatal(err)
				}
				fd, err := syscall.Open(outside, syscall.O_RDWR|syscall.O_NONBLOCK, 0o600)
				if err != nil {
					t.Fatal(err)
				}
				defer syscall.Close(fd)
				prefix := f.originals[f.vm.baseSpec.RootDisk][:qcow2HeaderLen]
				if n, err := syscall.Write(fd, prefix); err != nil || n != len(prefix) {
					t.Fatalf("prime unauthorized ancestor FIFO: %d, %v", n, err)
				}
				path := f.vm.baseSpec.RootDisk
				if disk == "volume" {
					path = f.vm.baseSpec.Volumes[0].Path
				}
				if err := createQcow2OverlayOn(path, outside, "qcow2", 1<<20); err != nil {
					t.Fatal(err)
				}
				stripLegacyVMBackingFormat(t, path)
				if kind == "cold-launch" {
					err = f.e.Start(t.Context(), f.vm.ID)
				} else {
					_, err = captureLegacyVM(t, f, kind, "rejected")
				}
				remaining := make([]byte, len(prefix)+1)
				n, readErr := syscall.Read(fd, remaining)
				if readErr != nil || n != len(prefix) || !bytes.Equal(remaining[:n], prefix) {
					t.Fatalf("legacy %s probed an ancestor before authorization: %d/%d remain, %v; operation: %v", disk, n, len(prefix), readErr, err)
				}
				if err == nil || !strings.Contains(err.Error(), outside) || !strings.Contains(err.Error(), "authorized") {
					t.Fatalf("legacy %s accepted an unauthorized ancestor: %v", disk, err)
				}
			})
		}
	}
}

func TestLegacyVMRecordCaptureRejectsPointerChangesWhileQuiesced(t *testing.T) {
	for _, which := range []string{"filesystem-root", "memory-root", "memory-volume", "saved-image"} {
		t.Run(which, func(t *testing.T) {
			f := newLegacyVMRecordFixture(t)
			kind, quiesce := "memory", "SAVE"
			if which == "filesystem-root" {
				kind, quiesce = "filesystem", "PAUSE"
			} else if which == "saved-image" {
				kind, quiesce = "saved-image", "PAUSE"
			}
			path := f.vm.baseSpec.RootDisk
			if which == "memory-volume" {
				path = f.vm.baseSpec.Volumes[0].Path
			}
			target := filepath.Join(f.e.cfg.DataDir, "images", "u1", "alternate.raw")
			writeFile(t, target, make([]byte, 1<<20))
			if !f.e.vmmBaseAllowed(f.vm, target) {
				t.Fatal("quiesce substitution is not independently authorized")
			}
			ops := serveLegacyVMCapture(t, f, func(op string) error {
				if op != quiesce {
					return fmt.Errorf("unexpected quiesce operation %s", op)
				}
				return createQcow2OverlayOn(path, target, "raw", 1<<20)
			})
			copies := observeSnapshotCopies(t)
			dst, err := captureLegacyVM(t, f, kind, "rejected")
			if err == nil || !strings.Contains(err.Error(), "paused source disk backing") || !strings.Contains(err.Error(), target) {
				t.Fatalf("legacy baseline accepted a substituted authorized pointer: %v", err)
			}
			if got := readOperations(ops); !slices.Equal(got, []string{quiesce, "RESUME"}) {
				t.Fatalf("quiesced rejection did not resume exactly once: %v", got)
			}
			if data, err := os.ReadFile(copies); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("capture copied a disk before rejecting the substituted pointer: %q, %v", data, err)
			}
			mustNotExist(t, dst)
			if f.vm.baseSpec.Backing != nil || f.vm.baseSpec.Volumes[0].Backing != nil {
				t.Fatal("rejected capture replaced legacy VM authority")
			}
		})
	}
}

func TestLegacyVMRecordTypedChainsRemainStrict(t *testing.T) {
	for _, disk := range []string{"root", "volume"} {
		for _, exact := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/exact=%t", disk, exact), func(t *testing.T) {
				f := newLegacyVMRecordFixture(t)
				path := f.vm.baseSpec.RootDisk
				if disk == "volume" {
					path = f.vm.baseSpec.Volumes[0].Path
				}
				chain := []diskLayer{}
				if exact {
					var err error
					chain, err = f.e.legacyDiskChain(f.vm, path, "qcow2")
					if err != nil {
						t.Fatal(err)
					}
				}
				if disk == "root" {
					f.vm.baseSpec.Backing = chain
				} else {
					f.vm.baseSpec.Volumes[0].Backing = chain
				}
				if err := f.e.Start(t.Context(), f.vm.ID); err == nil || !strings.Contains(err.Error(), "recorded") {
					t.Fatalf("cold launch applied legacy inference to a nonnil typed chain: %v", err)
				}
				ops := serveLegacyVMCapture(t, f, nil)
				if _, err := captureLegacyVM(t, f, "memory", "rejected"); err == nil || !strings.Contains(err.Error(), "recorded") {
					t.Fatalf("capture applied legacy inference to a nonnil typed chain: %v", err)
				}
				if got := readOperations(ops); len(got) != 0 {
					t.Fatalf("typed-chain rejection quiesced its source: %v", got)
				}
				if _, err := readDiskChain(path, "qcow2", nil); err == nil || !strings.Contains(err.Error(), "no recorded format") {
					t.Fatalf("ordinary known-driver chain reads became permissive: %v", err)
				}
			})
		}
	}
}

func TestLegacyVMRecordKnownRawTopStaysOpaque(t *testing.T) {
	f := newLegacyVMRecordFixture(t)
	f.vm.baseSpec.RootDiskFormat = "raw"
	f.vm.baseSpec.Volumes[0].Format = "raw"
	for _, path := range []string{f.vm.baseSpec.RootDisk, f.vm.baseSpec.Volumes[0].Path} {
		chain, err := readLegacyDiskChain(path, "raw", func(next string) error {
			return fmt.Errorf("raw bytes unexpectedly authorized as a header: %s", next)
		})
		if err != nil || len(chain) != 0 {
			t.Fatalf("known raw qcow2-looking disk acquired ancestors: %+v, %v", chain, err)
		}
	}
	spec := f.vm.baseSpec
	baseline, err := f.e.captureDiskBacking(f.vm, &spec)
	if err != nil || baseline.legacy() || len(spec.Backing) != 0 || len(spec.Volumes[0].Backing) != 0 {
		t.Fatalf("capture interpreted known raw qcow2 bytes: %+v, %+v, %v", baseline, spec, err)
	}
	if err := f.e.Start(t.Context(), f.vm.ID); err != nil {
		t.Fatalf("raw cold-launch seam interpreted guest bytes as qcow2: %v", err)
	}
	assertSnapshotHelperCannotAccess(t, f.vm, f.ancestor, f.stable, f.e.cfg.BaseImage)
	for _, path := range []string{f.vm.baseSpec.RootDisk, f.vm.baseSpec.Volumes[0].Path} {
		if after, err := os.ReadFile(path); err != nil || !bytes.Equal(after, f.originals[path]) {
			t.Fatalf("raw legacy launch normalized opaque guest bytes: %s, %v", path, err)
		}
	}
}

func TestSaveLegacyImagePinsNormalizationThroughPublicationAndCleanup(t *testing.T) {
	for _, outcome := range []string{"success", "clone-error", "validation-error", "metadata-error"} {
		t.Run(outcome, func(t *testing.T) {
			f := newLegacyVMRecordFixture(t)
			dst := filepath.Join(f.e.cfg.DataDir, "images", "u1", "saved.ext4")
			if outcome == "clone-error" || outcome == "validation-error" {
				cp, err := exec.LookPath("cp")
				if err != nil {
					t.Fatal(err)
				}
				bin := t.TempDir()
				var script string
				if outcome == "clone-error" {
					script = fmt.Sprintf("#!/bin/sh\nfor arg in \"$@\"; do last=\"$arg\"; done\nif [ \"$last\" = %q ]; then printf partial > \"$last\"; exit 1; fi\nexec %q \"$@\"\n", dst, cp)
				} else {
					malicious := filepath.Join(bin, "substituted.qcow2")
					if err := createQcow2OverlayOn(malicious, f.e.cfg.BaseImage, "raw", 1<<20); err != nil {
						t.Fatal(err)
					}
					script = fmt.Sprintf("#!/bin/sh\nfor arg in \"$@\"; do last=\"$arg\"; done\n%q \"$@\" || exit\nif [ \"$last\" = %q ]; then %q %q \"$last\"; fi\n", cp, dst, cp, malicious)
				}
				writeFile(t, filepath.Join(bin, "cp"), []byte(script))
				if err := os.Chmod(filepath.Join(bin, "cp"), 0o755); err != nil {
					t.Fatal(err)
				}
				t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
			}
			probes := make(chan error, 2)
			publication := make(chan error, 1)
			ops := serveLegacyVMCapture(t, f, nil, func(op string) error {
				if op != "PAUSE" && op != "RESUME" {
					return fmt.Errorf("unexpected image capture operation %s", op)
				}
				probes <- tryBasesLock(f.e.cfg.DataDir, syscall.LOCK_EX)
				if err := tryBasesLock(f.e.cfg.DataDir, syscall.LOCK_SH); err != nil {
					return fmt.Errorf("image capture did not use a shared lease: %w", err)
				}
				if op == "PAUSE" {
					if outcome == "metadata-error" {
						return os.Mkdir(nodeMetadataPath(dst), 0o700)
					}
					if outcome != "success" {
						return os.WriteFile(nodeMetadataPath(dst), []byte("obsolete sidecar"), 0o600)
					}
					return nil
				}
				var err error
				if outcome == "success" {
					var metadata nodeMetadata
					var found bool
					found, err = readDaemonMetadata(nodeMetadataPath(dst), &metadata)
					if err == nil && !found {
						err = errors.New("image sidecar was not published before lease-protected resume")
					}
					if err == nil {
						err = validateRecordedDisk(dst, metadata.Format, metadata.Backing)
					}
				} else {
					for _, path := range []string{dst, nodeMetadataPath(dst)} {
						if _, statErr := os.Lstat(path); !errors.Is(statErr, os.ErrNotExist) {
							err = errors.Join(err, fmt.Errorf("failed image artifact survived cleanup: %s, %v", path, statErr))
						}
					}
				}
				publication <- err
				return err
			})
			err := f.e.SaveImage(t.Context(), f.vm.ID, dst)
			if outcome == "success" {
				if err != nil {
					t.Fatal(err)
				}
			} else {
				stage := map[string]string{"clone-error": "copy overlay", "validation-error": "captured root disk", "metadata-error": "record image backing"}[outcome]
				if err == nil || !strings.Contains(err.Error(), stage) {
					t.Fatalf("did not exercise the intended image failure stage %s: %v", outcome, err)
				}
			}
			if got := readOperations(ops); !slices.Equal(got, []string{"PAUSE", "RESUME"}) {
				t.Fatalf("image capture did not pause/resume exactly once: %v", got)
			}
			for range 2 {
				select {
				case err := <-probes:
					if !errors.Is(err, syscall.EWOULDBLOCK) {
						t.Errorf("image normalization/publication did not pin nodes with a shared lease: %v", err)
					}
				default:
					t.Fatal("image capture missed a lease probe")
				}
			}
			select {
			case err := <-publication:
				if err != nil {
					t.Fatal(err)
				}
			default:
				t.Fatal("image capture never reached publication/cleanup before resume")
			}
			f.e.waitNodeSweep()
			requireBasesLease(t, f.e.cfg.DataDir, false)
			f.assertSourcesUnchanged(t)
		})
	}
}

func TestLegacyVMRecordCaptureRejectsPresentFormatChanges(t *testing.T) {
	for _, disk := range []string{"root", "volume"} {
		t.Run(disk, func(t *testing.T) {
			f := newLegacyVMRecordFixture(t)
			path := f.vm.baseSpec.RootDisk
			if disk == "volume" {
				path = f.vm.baseSpec.Volumes[0].Path
			}
			ops := serveLegacyVMCapture(t, f, func(op string) error {
				if op != "SAVE" {
					return fmt.Errorf("unexpected quiesce operation %s", op)
				}
				// The path is unchanged and authorized, but its newly present
				// format disagrees with the pre-quiesce inferred driver.
				return createQcow2OverlayOn(path, f.ancestor, "raw", 1<<20)
			})
			copies := observeSnapshotCopies(t)
			dst, err := captureLegacyVM(t, f, "memory", "rejected")
			if err == nil || !strings.Contains(err.Error(), "paused source disk backing") ||
				!strings.Contains(err.Error(), "does not match authorized backing") {
				t.Fatalf("legacy baseline accepted a changed present format: %v", err)
			}
			if got := readOperations(ops); !slices.Equal(got, []string{"SAVE", "RESUME"}) {
				t.Fatalf("format rejection did not resume exactly once: %v", got)
			}
			if data, err := os.ReadFile(copies); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("capture copied before rejecting a substituted format: %q, %v", data, err)
			}
			mustNotExist(t, dst)
		})
	}
}
