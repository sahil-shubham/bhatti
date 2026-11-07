//go:build linux

package collector

// read is the host's /proc, and its disk.
func (h *hostReader) read() (Host, error) { return h.readProc() }
