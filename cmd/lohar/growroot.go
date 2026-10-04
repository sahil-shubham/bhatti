//go:build linux

package main

import (
	"fmt"
	"os"
	"unsafe"

	"golang.org/x/sys/unix"
)

// ext4IocResizeFS is EXT4_IOC_RESIZE_FS: _IOW('f', 16, __u64).
const ext4IocResizeFS = 0x40086610

// growRoot grows the mounted root ext4 to fill /dev/vda. The host may give a
// sandbox a root disk larger than the image's filesystem (--disk-size); ext4
// resizes online, so this runs at every boot and does nothing once grown.
func growRoot() error {
	dev, err := os.Open("/dev/vda")
	if err != nil {
		return err
	}
	devBytes, err := unix.IoctlGetInt(int(dev.Fd()), unix.BLKGETSIZE64)
	dev.Close()
	if err != nil {
		return fmt.Errorf("BLKGETSIZE64: %w", err)
	}
	var st unix.Statfs_t
	if err := unix.Statfs("/", &st); err != nil {
		return err
	}
	if st.Type != unix.EXT4_SUPER_MAGIC || st.Bsize <= 0 {
		return nil // not ext4: nothing we know how to grow
	}
	want := uint64(devBytes) / uint64(st.Bsize)
	if want <= st.Blocks {
		return nil
	}
	root, err := os.Open("/")
	if err != nil {
		return err
	}
	defer root.Close()
	if _, _, errno := unix.Syscall(unix.SYS_IOCTL, root.Fd(), ext4IocResizeFS, uintptr(unsafe.Pointer(&want))); errno != 0 {
		return fmt.Errorf("EXT4_IOC_RESIZE_FS to %d blocks: %w", want, errno)
	}
	return nil
}
