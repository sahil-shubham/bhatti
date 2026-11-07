//go:build darwin

package collector

import (
	"errors"
	"fmt"
	"unsafe"

	"golang.org/x/sys/unix"
)

// macOS has no cgroups, so a VM helper's use is read per process, the way
// Activity Monitor does: from the kernel's proc_info call, whose
// PROC_INFO_CALL_PIDRUSAGE flavor is what libproc's proc_pid_rusage(3) wraps.
// Going to the call directly keeps the daemon free of cgo; `ps` would give CPU
// time and resident size, but to the hundredth of a second, with a fork per
// sample, and nothing about disk I/O.
const (
	procInfoCallPidRusage = 9 // PROC_INFO_CALL_PIDRUSAGE in <sys/proc_info.h>
	rusageInfoFlavorV2    = 2 // RUSAGE_INFO_V2 in <sys/resource.h>: through the disk byte counters
)

// rusageInfoV2 is struct rusage_info_v2 of <sys/resource.h>, field for field.
type rusageInfoV2 struct {
	UUID                [16]byte
	UserTime            uint64 // Mach absolute time ticks
	SystemTime          uint64 // Mach absolute time ticks
	PkgIdleWkups        uint64
	InterruptWkups      uint64
	Pageins             uint64
	WiredSize           uint64
	ResidentSize        uint64 // bytes
	PhysFootprint       uint64
	ProcStartAbsTime    uint64
	ProcExitAbsTime     uint64
	ChildUserTime       uint64
	ChildSystemTime     uint64
	ChildPkgIdleWkups   uint64
	ChildInterruptWkups uint64
	ChildPageins        uint64
	ChildElapsedAbsTime uint64
	DiskioBytesRead     uint64
	DiskioBytesWritten  uint64
}

// processReader reads one process's figures at a time.
type processReader struct {
	ticksPerSec uint64
	err         error // why ticksPerSec is unknown
}

func newProcessReader() *processReader {
	// CPU times come in Mach absolute time ticks: nanoseconds on Intel, 1/24 of
	// a microsecond on Apple silicon. hw.tbfrequency is how many a second.
	freq, err := unix.SysctlUint64("hw.tbfrequency")
	if err == nil && freq == 0 {
		err = errors.New("is zero")
	}
	if err != nil {
		return &processReader{err: fmt.Errorf("hw.tbfrequency: %w", err)}
	}
	return &processReader{ticksPerSec: freq}
}

// usage reads pid's CPU time, resident memory and the bytes it has read and
// written at the disk. errGone if the process has exited.
func (p *processReader) usage(pid int) (Usage, error) {
	if p.err != nil {
		return Usage{}, p.err
	}
	var ru rusageInfoV2
	_, _, errno := unix.Syscall6(unix.SYS_PROC_INFO, procInfoCallPidRusage, uintptr(pid), rusageInfoFlavorV2, 0,
		uintptr(unsafe.Pointer(&ru)), unsafe.Sizeof(ru))
	if errno == unix.ESRCH {
		return Usage{}, errGone
	}
	if errno != 0 {
		return Usage{}, fmt.Errorf("proc_info rusage of pid %d: %w", pid, errno)
	}
	user, system := ticksToUsec(ru.UserTime, p.ticksPerSec), ticksToUsec(ru.SystemTime, p.ticksPerSec)
	return Usage{
		CPU:    &CPU{UsageUsec: user + system, UserUsec: user, SystemUsec: system},
		Memory: &Memory{Current: ru.ResidentSize},
		IO:     &IO{ReadBytes: ru.DiskioBytesRead, WriteBytes: ru.DiskioBytesWritten},
	}, nil
}

// ticksToUsec converts without overflowing: ticks * 1e6 passes 2^64 within
// minutes of CPU time at 24 MHz.
func ticksToUsec(ticks, perSec uint64) uint64 {
	return ticks/perSec*1_000_000 + ticks%perSec*1_000_000/perSec
}
