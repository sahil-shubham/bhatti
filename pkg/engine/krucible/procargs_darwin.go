package krucible

import (
	"bytes"
	"encoding/binary"
	"errors"

	"golang.org/x/sys/unix"
)

// procArgs returns the argv a running process was started with. kern.procargs2
// holds argc, the executable's path and its NUL padding, then argv and the
// environment.
func procArgs(pid int) ([]string, error) {
	b, err := unix.SysctlRaw("kern.procargs2", pid)
	if err != nil {
		return nil, err
	}
	if len(b) < 4 {
		return nil, errors.New("kern.procargs2: no argc")
	}
	argc := int(binary.NativeEndian.Uint32(b))
	b = b[4:]
	i := bytes.IndexByte(b, 0)
	if i < 0 {
		return nil, errors.New("kern.procargs2: no executable path")
	}
	b = bytes.TrimLeft(b[i:], "\x00")
	args := make([]string, 0, argc)
	for len(args) < argc {
		i := bytes.IndexByte(b, 0)
		if i < 0 {
			return nil, errors.New("kern.procargs2: argv cut short")
		}
		args = append(args, string(b[:i]))
		b = b[i+1:]
	}
	return args, nil
}

// listPIDs returns the pids of the processes running now.
func listPIDs() ([]int, error) {
	procs, err := unix.SysctlKinfoProcSlice("kern.proc.all")
	if err != nil {
		return nil, err
	}
	pids := make([]int, 0, len(procs))
	for _, p := range procs {
		if pid := int(p.Proc.P_pid); pid > 0 {
			pids = append(pids, pid)
		}
	}
	return pids, nil
}
