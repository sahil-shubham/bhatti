//go:build darwin

package collector

import (
	"encoding/binary"
	"errors"
	"fmt"
	"runtime"

	"golang.org/x/sys/unix"
)

// read reads what macOS offers without cgo: its memory pressure level, the
// share of memory the kernel counts as free, swap, and the disk. There is no
// cheap per-CPU time to read (it takes a Mach call), so the host has no CPU
// figures here; what a sandbox's helper uses is measured per process.
func (h *hostReader) read() (Host, error) {
	host := Host{CPUs: runtime.NumCPU()}
	var errs []error

	total, err := unix.SysctlUint64("hw.memsize")
	if err != nil {
		errs = append(errs, fmt.Errorf("hw.memsize: %w", err))
	} else {
		mem := HostMemory{TotalBytes: total}
		// kern.memorystatus_level is the percentage of memory the kernel counts as
		// available, the figure memory_pressure(1) prints as "free percentage".
		if pct, err := unix.SysctlUint32("kern.memorystatus_level"); err != nil {
			errs = append(errs, fmt.Errorf("kern.memorystatus_level: %w", err))
		} else {
			mem.AvailableBytes = total / 100 * uint64(min(pct, 100))
		}
		if swap, err := swapUsage(); err != nil {
			errs = append(errs, err)
		} else {
			mem.SwapTotalBytes, mem.SwapFreeBytes = swap.total, swap.avail
		}
		// 1 is normal, 2 warning, 4 critical.
		if lvl, err := unix.SysctlUint32("kern.memorystatus_vm_pressure_level"); err != nil {
			errs = append(errs, fmt.Errorf("kern.memorystatus_vm_pressure_level: %w", err))
		} else {
			mem.PressureLevel = pressureLevelName(lvl)
		}
		host.Memory = &mem
	}

	if h.dataDir != "" {
		if d, err := readDisk(h.dataDir); err != nil {
			errs = append(errs, err)
		} else {
			host.Disk = d
		}
	}
	return host, errors.Join(errs...)
}

func pressureLevelName(level uint32) string {
	switch level {
	case 1:
		return "normal"
	case 2:
		return "warn"
	case 4:
		return "critical"
	}
	return ""
}

type swapUse struct{ total, avail uint64 }

// swapUsage reads vm.swapusage, a struct xsw_usage: total, available and used
// bytes (uint64 each), then the page size and an encrypted flag.
func swapUsage() (swapUse, error) {
	b, err := unix.SysctlRaw("vm.swapusage")
	if err != nil {
		return swapUse{}, fmt.Errorf("vm.swapusage: %w", err)
	}
	if len(b) < 24 {
		return swapUse{}, fmt.Errorf("vm.swapusage: %d bytes, want at least 24", len(b))
	}
	return swapUse{total: binary.NativeEndian.Uint64(b[0:8]), avail: binary.NativeEndian.Uint64(b[8:16])}, nil
}
