//go:build linux

package main

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"

	"golang.org/x/sys/unix"
)

// In a child process (Landlock can't be undone): after landlockReadOnly("/etc")
// it can still read under /etc but can't write anywhere — a file it could
// create a moment earlier is now refused.
func TestLandlockReadOnly(t *testing.T) {
	if dir := os.Getenv("NETD_LANDLOCK_CHILD"); dir != "" {
		// confine sets no_new_privs before Landlock; restrict_self requires it
		// for an unprivileged caller.
		if _, _, errno := syscall.AllThreadsSyscall(syscall.SYS_PRCTL, unix.PR_SET_NO_NEW_PRIVS, 1, 0); errno != 0 {
			t.Fatalf("no_new_privs: %v", errno)
		}
		if err := landlockReadOnly("/etc"); err != nil {
			if errors.Is(err, unix.ENOSYS) || errors.Is(err, unix.EOPNOTSUPP) {
				os.Exit(3)
			}
			t.Fatalf("landlockReadOnly: %v", err)
		}
		if _, err := os.ReadFile("/etc/hostname"); err != nil {
			t.Fatalf("read under /etc after confine: %v", err)
		}
		if err := os.WriteFile(filepath.Join(dir, "after"), []byte("x"), 0o600); !errors.Is(err, os.ErrPermission) {
			t.Fatalf("write after confine: err=%v, want permission denied", err)
		}
		return
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "before"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(os.Args[0], "-test.run", "^TestLandlockReadOnly$", "-test.v")
	cmd.Env = append(os.Environ(), "NETD_LANDLOCK_CHILD="+dir)
	out, err := cmd.CombinedOutput()
	var ee *exec.ExitError
	if errors.As(err, &ee) && ee.ExitCode() == 3 {
		t.Skip("kernel without Landlock")
	}
	if err != nil {
		t.Fatalf("confined child: %v\n%s", err, out)
	}
}
