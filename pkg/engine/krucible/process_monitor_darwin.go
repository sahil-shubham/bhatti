//go:build darwin

package krucible

import (
	"time"

	"golang.org/x/sys/unix"
)

func waitProcessExit(pid int, matches func() bool, stop <-chan struct{}) bool {
	fd, err := unix.Kqueue()
	if err != nil {
		return pollProcessExit(matches, stop)
	}
	defer unix.Close(fd)
	change := unix.Kevent_t{Ident: uint64(pid), Filter: unix.EVFILT_PROC, Flags: unix.EV_ADD | unix.EV_ENABLE | unix.EV_ONESHOT, Fflags: unix.NOTE_EXIT}
	if _, err := unix.Kevent(fd, []unix.Kevent_t{change}, nil, nil); err != nil {
		if err == unix.ESRCH {
			return true
		}
		return pollProcessExit(matches, stop)
	}
	// Installing the kevent and identifying the process are separate operations.
	// A recycled pid must never make us watch (or later kill) another program.
	if processIdentityGone(matches, stop) {
		return true
	}
	for {
		select {
		case <-stop:
			return false
		default:
		}
		var events [1]unix.Kevent_t
		deadline := unix.NsecToTimespec(int64(time.Second))
		n, err := unix.Kevent(fd, nil, events[:], &deadline)
		if err == unix.EINTR {
			continue
		}
		if err != nil {
			return pollProcessExit(matches, stop)
		}
		if n > 0 {
			if events[0].Flags&unix.EV_ERROR != 0 {
				return pollProcessExit(matches, stop)
			}
			if events[0].Filter == unix.EVFILT_PROC && events[0].Fflags&unix.NOTE_EXIT != 0 {
				return true
			}
		}
	}
}
