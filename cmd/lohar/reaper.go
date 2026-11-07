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
//
// Release also rewrites cmd.Process.Pid to -1, unsynchronised, so once a wait
// may be running no other goroutine may read cmd.Process. Anything that has to
// signal the child meanwhile goes through childGroup.
func waitTracked(cmd *exec.Cmd, done <-chan childExit) childExit {
	pid := cmd.Process.Pid
	exit := <-done
	if err := cmd.Process.Release(); err != nil {
		logf("release process %d: %v", pid, err)
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

// childGroup is a tracked child and the process group it leads, for callers
// that signal the group from one goroutine while another waits for the exit.
//
// Those goroutines must not read cmd.Process.Pid (see waitTracked), and a
// late one would read -1 and signal PID 1. The group ID is copied here before
// any other goroutine can see cmd. Once wait has collected the exit status
// the PID is free for the kernel to hand to an unrelated process, so signal
// stops: a stale KILL frame or idle timer must not reach whoever holds it now.
type childGroup struct {
	cmd  *exec.Cmd
	done <-chan childExit
	pgid int

	mu        sync.Mutex
	collected bool
}

// startGroup is startTracked for a child that leads its own process group
// (Setpgid or Setsid); signal addresses -pgid, which names no group otherwise.
func startGroup(cmd *exec.Cmd) (*childGroup, error) {
	done, err := startTracked(cmd)
	if err != nil {
		return nil, err
	}
	return &childGroup{cmd: cmd, done: done, pgid: cmd.Process.Pid}, nil
}

// signal sends sig to every process in the group until wait has returned.
// Orphans that outlive the leader (and may still hold its pipes open) are
// reached up to that point.
func (g *childGroup) signal(sig syscall.Signal) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if !g.collected {
		syscall.Kill(-g.pgid, sig)
	}
}

// wait is waitTracked plus the end of signalling.
func (g *childGroup) wait() childExit {
	exit := waitTracked(g.cmd, g.done)
	g.mu.Lock()
	g.collected = true
	g.mu.Unlock()
	return exit
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
