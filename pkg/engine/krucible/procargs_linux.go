package krucible

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// procArgs returns the argv a running process was started with.
func procArgs(pid int) ([]string, error) {
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/cmdline")
	if err != nil {
		return nil, err
	}
	b = bytes.TrimSuffix(b, []byte{0})
	if len(b) == 0 {
		return nil, nil // a zombie: its argv is gone with its memory
	}
	return strings.Split(string(b), "\x00"), nil
}

func procExecutableMatches(pid int, binary string) bool {
	exe, err := os.Readlink("/proc/" + strconv.Itoa(pid) + "/exe")
	if err != nil {
		return os.IsPermission(err)
	}
	expected, err := filepath.EvalSymlinks(binary)
	if err != nil {
		return false
	}
	expected, err = filepath.Abs(expected)
	// Replacing an installed helper does not invalidate the still-running
	// previous version: procfs appends this suffix to its original path.
	return err == nil && strings.TrimSuffix(exe, " (deleted)") == expected
}

func procStartTime(pid int) (uint64, error) {
	stat, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return 0, err
	}
	return parseProcStartTime(stat)
}

func parseProcStartTime(stat []byte) (uint64, error) {
	// comm may contain spaces, newlines and ')', so only its final delimiter
	// identifies where the ordinary fields begin. Start time is field 22.
	end := bytes.LastIndexByte(stat, ')')
	if end < 0 {
		return 0, errors.New("proc stat: missing comm delimiter")
	}
	rest := stat[end+1:]
	for field := 3; ; field++ {
		rest = bytes.TrimLeft(rest, " \t\r\n\v\f")
		if len(rest) == 0 {
			return 0, errors.New("proc stat: missing start time")
		}
		end = bytes.IndexAny(rest, " \t\r\n\v\f")
		if end < 0 {
			end = len(rest)
		}
		if field == 22 {
			return strconv.ParseUint(string(rest[:end]), 10, 64)
		}
		rest = rest[end:]
	}
}

// listPIDs returns the pids of the processes running now.
func listPIDs() ([]int, error) {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil, err
	}
	pids := make([]int, 0, len(entries))
	for _, e := range entries {
		if pid, err := strconv.Atoi(e.Name()); err == nil && pid > 0 {
			pids = append(pids, pid)
		}
	}
	return pids, nil
}
