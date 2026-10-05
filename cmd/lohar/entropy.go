//go:build linux

package main

import (
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"unsafe"

	"github.com/sahil-shubham/bhatti/pkg/agent/proto"
	"golang.org/x/sys/unix"
)

// seedEntropy credits the host's per-boot seed to the kernel's entropy pool,
// which seeds the CRNG at once. A guest without a hardware entropy source (no
// RDRAND/RNDR at boot — the case under HVF — and no virtio-rng, which the VMM
// doesn't attach) otherwise stays unseeded for minutes, and every getrandom(2)
// caller blocks until then: a Go program's first TLS handshake, say, which
// blocks in the vDSO still holding its P, so on a one-vCPU guest even the
// program's own timeouts never fire. Writing the seed to /dev/urandom would mix
// it in without crediting it.
func seedEntropy(seed []byte) error {
	if len(seed) == 0 {
		return nil // a host that sends none: the kernel's own sources, as before
	}
	defer clear(seed)
	f, err := os.OpenFile("/dev/urandom", os.O_WRONLY, 0)
	if err != nil {
		return err
	}
	defer f.Close()
	// struct rand_pool_info { int entropy_count; int buf_size; __u32 buf[]; }
	info := make([]byte, 8+len(seed))
	defer clear(info)
	binary.NativeEndian.PutUint32(info[0:], uint32(8*len(seed)))
	binary.NativeEndian.PutUint32(info[4:], uint32(len(seed)))
	copy(info[8:], seed)
	if _, _, errno := unix.Syscall(unix.SYS_IOCTL, f.Fd(), unix.RNDADDENTROPY, uintptr(unsafe.Pointer(&info[0]))); errno != 0 {
		return fmt.Errorf("RNDADDENTROPY: %w", errno)
	}
	return nil
}

// A saved memory image also saves the kernel CRNG; a clone must receive fresh
// host entropy before it can be returned to a caller.
func reseedEntropy(seed []byte) error {
	if len(seed) != proto.ReseedBytes {
		return fmt.Errorf("expected %d entropy bytes, got %d", proto.ReseedBytes, len(seed))
	}
	f, err := os.OpenFile("/dev/random", os.O_RDWR, 0)
	if err != nil {
		return fmt.Errorf("open /dev/random: %w", err)
	}
	defer f.Close()
	var info [8 + proto.ReseedBytes]byte // struct rand_pool_info: entropy_count, buf_size, buf
	defer clear(info[:])
	binary.NativeEndian.PutUint32(info[0:], uint32(8*len(seed)))
	binary.NativeEndian.PutUint32(info[4:], uint32(len(seed)))
	copy(info[8:], seed)
	if _, _, errno := unix.Syscall(unix.SYS_IOCTL, f.Fd(), unix.RNDADDENTROPY, uintptr(unsafe.Pointer(&info[0]))); errno != 0 {
		return fmt.Errorf("RNDADDENTROPY: %w", errno)
	}
	if _, _, errno := unix.Syscall(unix.SYS_IOCTL, f.Fd(), unix.RNDRESEEDCRNG, 0); errno != 0 &&
		!errors.Is(errno, unix.ENOTTY) && !errors.Is(errno, unix.EINVAL) {
		return fmt.Errorf("RNDRESEEDCRNG: %w", errno)
	}
	// Linux 5.18+ immediately reseeds on credited entropy even when the
	// explicit reseed ioctl isn't supported.
	return nil
}
