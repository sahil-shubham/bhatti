package collector

import (
	"fmt"

	"golang.org/x/sys/unix"
)

// hostReader reads the machine's own figures. The platform files give it its
// read method.
type hostReader struct {
	rd       *usageReader // for the buffer
	procRoot string       // where /proc is
	dataDir  string       // the filesystem whose free space is reported
}

// readDisk is the space on the filesystem path is on.
func readDisk(path string) (*Disk, error) {
	var st unix.Statfs_t
	if err := unix.Statfs(path, &st); err != nil {
		return nil, fmt.Errorf("statfs %s: %w", path, err)
	}
	bsize := uint64(st.Bsize)
	return &Disk{Path: path, TotalBytes: uint64(st.Blocks) * bsize, FreeBytes: uint64(st.Bavail) * bsize}, nil
}
