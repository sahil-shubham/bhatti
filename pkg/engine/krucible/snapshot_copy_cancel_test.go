package krucible

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"
)

// Run as cp through PATH, without a child process keeping CombinedOutput's
// pipes open after CommandContext kills the blocked copy.
func TestCaptureCopyStandin(t *testing.T) {
	if os.Getenv("BHATTI_TEST_COPY_HELPER") != "1" {
		return
	}
	args := os.Args[slices.Index(os.Args, "--")+1:]
	kind := "fallback"
	if len(args) == 3 {
		kind, args = "primary", args[1:]
	}
	if len(args) != 2 {
		os.Exit(2)
	}
	log, err := os.OpenFile(os.Getenv("BHATTI_TEST_COPY_LOG"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		os.Exit(2)
	}
	fmt.Fprintln(log, kind)
	log.Close()
	match := args[0] == os.Getenv("BHATTI_TEST_COPY_SOURCE") ||
		(os.Getenv("BHATTI_TEST_COPY_ADOPT") == "1" && strings.HasPrefix(filepath.Base(args[1]), ".adopt-"))
	if match {
		if kind != os.Getenv("BHATTI_TEST_COPY_ATTEMPT") {
			os.Exit(1)
		}
		conn, err := net.Dial("unix", os.Getenv("BHATTI_TEST_COPY_SOCKET"))
		if err != nil {
			os.Exit(2)
		}
		fmt.Fprintln(conn, kind)
		io.Copy(io.Discard, conn)
		conn.Close()
		os.Exit(1)
	}
	if err := exec.Command(os.Getenv("BHATTI_TEST_REAL_CP"), args...).Run(); err != nil {
		os.Exit(1)
	}
	os.Exit(0)
}

type enteredCopy struct {
	conn net.Conn
	err  error
}

func gateSnapshotCopy(t *testing.T, source, attempt string, adopt bool) (<-chan enteredCopy, string) {
	t.Helper()
	cp, err := exec.LookPath("cp")
	if err != nil {
		t.Fatal(err)
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	log := filepath.Join(dir, "copies")
	sock := filepath.Join(shortSockDir(t), "cp.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	entered := make(chan enteredCopy, 1)
	go func() {
		conn, err := ln.Accept()
		if err == nil {
			_, err = bufio.NewReader(conn).ReadString('\n')
		}
		entered <- enteredCopy{conn, err}
	}()
	script := fmt.Sprintf("#!/bin/sh\nexec %q -test.run '^TestCaptureCopyStandin$' -- \"$@\"\n", executable)
	if err := os.WriteFile(filepath.Join(dir, "cp"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("BHATTI_TEST_COPY_HELPER", "1")
	t.Setenv("BHATTI_TEST_COPY_LOG", log)
	t.Setenv("BHATTI_TEST_REAL_CP", cp)
	t.Setenv("BHATTI_TEST_COPY_SOCKET", sock)
	t.Setenv("BHATTI_TEST_COPY_SOURCE", source)
	t.Setenv("BHATTI_TEST_COPY_ATTEMPT", attempt)
	if adopt {
		t.Setenv("BHATTI_TEST_COPY_ADOPT", "1")
	}
	return entered, log
}

func waitForEnteredCopy(t *testing.T, entered <-chan enteredCopy, done <-chan error) net.Conn {
	t.Helper()
	select {
	case entry := <-entered:
		if entry.err != nil {
			t.Fatal(entry.err)
		}
		return entry.conn
	case err := <-done:
		t.Fatalf("capture returned before entering the copy: %v", err)
	case <-time.After(10 * time.Second):
		t.Fatal("capture never entered the copy")
	}
	return nil
}

func TestCaptureCancellationStopsEnteredCopyAndResumesSource(t *testing.T) {
	for _, point := range []string{"filesystem-root", "memory-root", "memory-volume", "saved-image-root", "filesystem-ancestor", "saved-image-ancestor", "filesystem-adopt", "saved-image-adopt"} {
		t.Run(point, func(t *testing.T) {
			f := newNodeFixture(t)
			root := f.freshRoot("source")
			spec := VMSpec{RootDisk: root, RootDiskFormat: "qcow2"}
			target := root
			legacy := strings.HasSuffix(point, "ancestor") || strings.HasSuffix(point, "adopt")
			if legacy {
				ancestor := filepath.Join(f.data, "images", "u1", "ancestor.ext4")
				if err := os.MkdirAll(filepath.Dir(ancestor), 0o700); err != nil {
					t.Fatal(err)
				}
				relative, err := filepath.Rel(filepath.Dir(ancestor), f.base)
				if err != nil {
					t.Fatal(err)
				}
				if err := createQcow2OverlayOn(ancestor, relative, "raw", uint64(f.size)); err != nil {
					t.Fatal(err)
				}
				header, err := os.OpenFile(ancestor, os.O_WRONLY, 0)
				if err != nil {
					t.Fatal(err)
				}
				_, writeErr := header.WriteAt(make([]byte, 8), int64(qcow2HeaderLen))
				closeErr := header.Close()
				if writeErr != nil || closeErr != nil {
					t.Fatalf("remove legacy backing-format extension: %v, %v", writeErr, closeErr)
				}
				if err := createQcow2OverlayOn(root, ancestor, "qcow2", uint64(f.size)); err != nil {
					t.Fatal(err)
				}
				target = ancestor
			} else {
				if point == "memory-volume" {
					target = filepath.Join(filepath.Dir(root), "volume.img")
					writeFile(t, target, []byte("frozen volume"))
					spec.Volumes = []VMVolume{{BlockID: "vol0", Path: target, Format: "raw"}}
				}
				if err := f.e.recordDiskBacking(&spec); err != nil {
					t.Fatal(err)
				}
			}
			adopt := strings.HasSuffix(point, "adopt")
			if adopt {
				target = "" // The generated .adopt-* destination selects this copy.
				oldLink := linkNode
				t.Cleanup(func() { linkNode = oldLink })
				linkNode = func(src, dst string) error { return syscall.EXDEV }
			}
			ctlSock, ops := captureControlPeer(t, nil)
			agSock, _, _ := durabilityPeer(t, 0)
			e, vm := durabilityEngine(t, "hot", agSock, ctlSock)
			e.cfg, e.caps.Checkpoint = f.e.cfg, true
			vm.UserID, vm.baseSpec, vm.SandboxDir = "u1", spec, filepath.Dir(root)
			entered, log := gateSnapshotCopy(t, target, "primary", adopt)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			done, finished := make(chan error, 1), make(chan struct{})
			var conn net.Conn
			t.Cleanup(func() {
				if conn != nil {
					conn.Close() // Release the fake only after all assertions.
				}
				<-finished
				e.waitNodeSweep()
			})
			go func() {
				var err error
				if strings.HasPrefix(point, "saved-image") {
					dst := filepath.Join(f.data, "images", "saved.ext4")
					err = e.SaveImage(ctx, vm.ID, dst)
				} else {
					typ := "filesystem"
					if strings.HasPrefix(point, "memory") {
						typ = "memory"
					}
					_, err = e.CheckpointTyped(ctx, vm.ID, "u1", 0, "cancelled", filepath.Join(f.data, "snapshots", "u1"), typ)
				}
				done <- err
				close(finished)
			}()
			conn = waitForEnteredCopy(t, entered, done)
			requireBasesLease(t, f.data, true)
			if vm.launchMu.TryLock() {
				vm.launchMu.Unlock()
				t.Fatal("entered capture did not hold launchMu")
			}
			cancel()
			select {
			case err := <-done:
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("cancelled copy returned %v", err)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("cancellation left the source paused inside the entered copy")
			}
			quiesce := "PAUSE"
			if strings.HasPrefix(point, "memory") {
				quiesce = "SAVE"
			}
			if got := readOperations(ops); !slices.Equal(got, []string{quiesce, "RESUME"}) {
				t.Fatalf("cancelled capture did not resume its source exactly once: %v", got)
			}
			// Failed legacy captures may schedule their own exclusive sweep.
			e.waitNodeSweep()
			requireBasesLease(t, f.data, false)
			if !vm.launchMu.TryLock() {
				t.Fatal("cancelled capture retained launchMu")
			}
			vm.launchMu.Unlock()
			calls, err := os.ReadFile(log)
			if err != nil || strings.Contains(string(calls), "fallback") {
				t.Fatalf("cancelled primary started a fallback: %q, %v", calls, err)
			}
			if result, err := e.Exec(t.Context(), vm.ID, []string{"sync"}); err != nil || result.ExitCode != 0 {
				t.Fatalf("resumed source stopped serving: %+v, %v", result, err)
			}
		})
	}
}

func TestCloneFileCancellationStopsEnteredFallback(t *testing.T) {
	source := filepath.Join(t.TempDir(), "source")
	writeFile(t, source, []byte("source"))
	entered, log := gateSnapshotCopy(t, source, "fallback", false)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done, finished := make(chan error, 1), make(chan struct{})
	var conn net.Conn
	t.Cleanup(func() {
		if conn != nil {
			conn.Close()
		}
		<-finished
	})
	go func() {
		done <- cloneFile(ctx, source, filepath.Join(t.TempDir(), "destination"))
		close(finished)
	}()
	conn = waitForEnteredCopy(t, entered, done)
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cancelled fallback returned %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("cancellation did not stop the entered fallback")
	}
	calls, err := os.ReadFile(log)
	if err != nil || string(calls) != "primary\nfallback\n" {
		t.Fatalf("unexpected copy attempts: %q, %v", calls, err)
	}
}
