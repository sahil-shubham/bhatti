package krucible

import (
	"bytes"
	"os"
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
