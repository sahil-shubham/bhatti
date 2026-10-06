//go:build linux

package main

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"sync"
	"syscall"
)

// childExit is the kernel's status, not a synthetic exec.Cmd.Wait error.
// In particular, an exit(137) is not a SIGKILL, though both report code 137.
type childExit struct {
	status syscall.WaitStatus
}

func (e childExit) code() int {
	switch {
	case e.status.Exited():
		return e.status.ExitStatus()
	case e.status.Signaled():
		return 128 + int(e.status.Signal())
	default:
		return 1
	}
}

func (e childExit) err() error {
	if e.code() == 0 {
		return nil
	}
	return fmt.Errorf("exit status %d", e.code())
}

// The lock serializes Start+subscription with wait4+dispatch. A child can
// exit before Start returns, but cannot be reaped before its PID is registered.
// Orphans (including double-forked daemons adopted by PID 1) have no entry;
// their status is intentionally discarded after wait4 reaps them.
var childReaper = struct {
	once    sync.Once
	mu      sync.Mutex
	wait    map[int]chan childExit
	reapAll bool
}{}

func startChildReaper() {
	childReaper.once.Do(func() {
		// Only init adopts orphans. In a standalone systemctl invocation
		// (and in Go tests), other code may own direct children via Cmd.Wait.
		childReaper.reapAll = childReaper.reapAll || os.Getpid() == 1
		childReaper.wait = make(map[int]chan childExit)
		ch := make(chan os.Signal, 1)
		signal.Notify(ch, syscall.SIGCHLD)
		go func() {
			for range ch {
				childReaper.mu.Lock()
				if childReaper.reapAll {
					for {
						var status syscall.WaitStatus
						pid, err := syscall.Wait4(-1, &status, syscall.WNOHANG, nil)
						if pid > 0 {
							if done := childReaper.wait[pid]; done != nil {
								delete(childReaper.wait, pid)
								done <- childExit{status: status}
							}
						}
						if err == syscall.EINTR {
							continue
						}
						if err != nil || pid <= 0 {
							break
						}
					}
				} else {
					for pid, done := range childReaper.wait {
						var status syscall.WaitStatus
						var got int
						for {
							var err error
							got, err = syscall.Wait4(pid, &status, syscall.WNOHANG, nil)
							if err != syscall.EINTR {
								break
							}
						}
						if got == pid {
							delete(childReaper.wait, pid)
							done <- childExit{status: status}
						}
					}
				}
				childReaper.mu.Unlock()
			}
		}()
	})
}

// Callers must use nil or *os.File for stdio: Cmd.Start otherwise starts
// io-copy goroutines that only Cmd.Wait can join. Pipe readers close their
// own descriptors after draining them.
func startTracked(cmd *exec.Cmd) (<-chan childExit, error) {
	startChildReaper()
	childReaper.mu.Lock()
	defer childReaper.mu.Unlock()
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	done := make(chan childExit, 1)
	childReaper.wait[cmd.Process.Pid] = done
	return done, nil
}

// waitTracked is used only after startTracked. Go's Cmd.Wait also invokes
// wait4, so it must not be used for these processes. Release closes Go's
// process handle (including the Linux pidfd); callers close any parent-side
// pipes they created after draining them.
func waitTracked(cmd *exec.Cmd, done <-chan childExit) childExit {
	exit := <-done
	if err := cmd.Process.Release(); err != nil {
		logf("release process %d: %v", cmd.Process.Pid, err)
	}
	return exit
}

func runTracked(cmd *exec.Cmd) error {
	done, err := startTracked(cmd)
	if err != nil {
		return err
	}
	return waitTracked(cmd, done).err()
}

// combinedOutputTracked captures stdout and stderr through a real pipe, not
// Cmd's io-copy goroutines (which would otherwise require Cmd.Wait to join).
func combinedOutputTracked(cmd *exec.Cmd) ([]byte, error) {
	reader, writer, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	defer reader.Close()
	cmd.Stdout, cmd.Stderr = writer, writer
	done, err := startTracked(cmd)
	writer.Close()
	if err != nil {
		return nil, err
	}
	out, readErr := io.ReadAll(reader)
	exit := waitTracked(cmd, done)
	if readErr != nil {
		return out, readErr
	}
	return out, exit.err()
}
