//go:build linux

package krucible

import "golang.org/x/sys/unix"

// A pidfd pins the process, not its recyclable numeric pid. Recheck argv after
// opening it: the old process could have exited between adoption and pidfd_open.
func waitProcessExit(pid int, matches func() bool, stop <-chan struct{}) bool {
	fd, err := unix.PidfdOpen(pid, 0)
	if err == nil {
		defer unix.Close(fd)
		if processIdentityGone(matches, stop) {
			return true
		}
		for {
			select {
			case <-stop:
				return false
			default:
			}
			fds := []unix.PollFd{{Fd: int32(fd), Events: unix.POLLIN}}
			n, err := unix.Poll(fds, 1000)
			if err == unix.EINTR {
				continue
			}
			if err != nil {
				break // old kernels and sandboxes may not support poll on pidfds
			}
			if n > 0 && fds[0].Revents&(unix.POLLIN|unix.POLLHUP) != 0 {
				return true
			}
		}
	} else if err == unix.ESRCH {
		return true
	}
	return pollProcessExit(matches, stop)
}
