//go:build linux

package main

import (
	"errors"
	"fmt"
	"log"
	"os"
	"syscall"
	"unsafe"

	"golang.org/x/sys/unix"
)

// confine drops what netd doesn't need once its sockets are open. netd parses
// untrusted guest traffic, so a bug in it shouldn't be worth more than the
// sockets it already holds.
//
//   - Started as root (the daemon spawns it so): become uid/gid, which also
//     drops every capability.
//   - no_new_privs: nothing it execs can regain privilege.
//   - Landlock: no filesystem access except reading /etc (the resolver's
//     config). Its listening sockets are already open, and it creates nothing.
//
// Applied to every thread (syscall.AllThreadsSyscall); that needs a cgo-free
// binary, which `make netd` builds. Changing identity failing is fatal: a netd
// running as root by accident is the case this exists to prevent. A kernel
// without Landlock only loses that layer, with a warning.
func confine(uid, gid int) error {
	if os.Geteuid() == 0 {
		if err := syscall.Setgroups(nil); err != nil {
			return fmt.Errorf("setgroups: %w", err)
		}
		if err := syscall.Setgid(gid); err != nil {
			return fmt.Errorf("setgid %d: %w", gid, err)
		}
		if err := syscall.Setuid(uid); err != nil {
			return fmt.Errorf("setuid %d: %w", uid, err)
		}
	}
	if _, _, errno := syscall.AllThreadsSyscall(syscall.SYS_PRCTL, unix.PR_SET_NO_NEW_PRIVS, 1, 0); errno != 0 {
		return fmt.Errorf("no_new_privs: %w", errno)
	}
	if err := landlockReadOnly("/etc"); err != nil {
		if errors.Is(err, unix.ENOSYS) || errors.Is(err, unix.EOPNOTSUPP) {
			log.Printf("bhatti-netd: Landlock unavailable on this kernel (%v); running without filesystem confinement", err)
			return nil
		}
		return fmt.Errorf("landlock: %w", err)
	}
	return nil
}

// landlockReadOnly restricts every thread to reading beneath dir, and nothing
// else, for every filesystem right this kernel's Landlock ABI knows.
func landlockReadOnly(dir string) error {
	abi, _, errno := unix.Syscall(unix.SYS_LANDLOCK_CREATE_RULESET, 0, 0, unix.LANDLOCK_CREATE_RULESET_VERSION)
	if errno != 0 {
		return errno
	}
	handled := uint64(unix.LANDLOCK_ACCESS_FS_EXECUTE | unix.LANDLOCK_ACCESS_FS_WRITE_FILE |
		unix.LANDLOCK_ACCESS_FS_READ_FILE | unix.LANDLOCK_ACCESS_FS_READ_DIR |
		unix.LANDLOCK_ACCESS_FS_REMOVE_DIR | unix.LANDLOCK_ACCESS_FS_REMOVE_FILE |
		unix.LANDLOCK_ACCESS_FS_MAKE_CHAR | unix.LANDLOCK_ACCESS_FS_MAKE_DIR |
		unix.LANDLOCK_ACCESS_FS_MAKE_REG | unix.LANDLOCK_ACCESS_FS_MAKE_SOCK |
		unix.LANDLOCK_ACCESS_FS_MAKE_FIFO | unix.LANDLOCK_ACCESS_FS_MAKE_BLOCK |
		unix.LANDLOCK_ACCESS_FS_MAKE_SYM)
	if abi >= 2 {
		handled |= unix.LANDLOCK_ACCESS_FS_REFER
	}
	if abi >= 3 {
		handled |= unix.LANDLOCK_ACCESS_FS_TRUNCATE
	}
	if abi >= 5 {
		handled |= unix.LANDLOCK_ACCESS_FS_IOCTL_DEV
	}
	attr := unix.LandlockRulesetAttr{Access_fs: handled}
	// The kernel accepts the full struct as long as the fields it doesn't know are zero.
	fd, _, errno := unix.Syscall(unix.SYS_LANDLOCK_CREATE_RULESET,
		uintptr(unsafe.Pointer(&attr)), unsafe.Sizeof(attr), 0)
	if errno != 0 {
		return errno
	}
	defer unix.Close(int(fd))

	parent, err := unix.Open(dir, unix.O_PATH|unix.O_CLOEXEC, 0)
	if err != nil {
		return fmt.Errorf("open %s: %w", dir, err)
	}
	defer unix.Close(parent)
	rule := unix.LandlockPathBeneathAttr{
		Allowed_access: unix.LANDLOCK_ACCESS_FS_READ_FILE | unix.LANDLOCK_ACCESS_FS_READ_DIR,
		Parent_fd:      int32(parent),
	}
	if _, _, errno := unix.Syscall6(unix.SYS_LANDLOCK_ADD_RULE, fd, unix.LANDLOCK_RULE_PATH_BENEATH,
		uintptr(unsafe.Pointer(&rule)), 0, 0, 0); errno != 0 {
		return fmt.Errorf("add rule %s: %w", dir, errno)
	}
	if _, _, errno := syscall.AllThreadsSyscall(unix.SYS_LANDLOCK_RESTRICT_SELF, fd, 0, 0); errno != 0 {
		return errno
	}
	return nil
}
