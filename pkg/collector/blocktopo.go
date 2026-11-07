package collector

import (
	"os"
	"strconv"
	"time"
)

// blockTopology tells which block devices are built on other devices (md, dm,
// LVM), from the slaves/ directory sysfs keeps for each. The kernel counts a
// cgroup's I/O at every layer a request passes, so io.stat lists both a RAID
// device and the disks beneath it for the same write; summing every row would
// count it twice or more. The answers are kept for a while: the topology
// changes when someone sets up a volume, not between samples.
//
// It isn't safe for concurrent use: the sampler goroutine owns it.
type blockTopology struct {
	root  string // /sys/dev/block
	now   func() time.Time
	cache map[uint64]stackedEntry
}

type stackedEntry struct {
	stacked bool
	at      time.Time
}

const topologyTTL = time.Minute

func newBlockTopology(root string, now func() time.Time) *blockTopology {
	return &blockTopology{root: root, now: now, cache: map[uint64]stackedEntry{}}
}

// stacked reports whether the device has devices beneath it. A device sysfs
// doesn't know, or a host that doesn't show sysfs, is not stacked: its row is
// counted rather than dropped.
func (t *blockTopology) stacked(major, minor uint32) bool {
	key := uint64(major)<<32 | uint64(minor)
	now := t.now()
	if e, ok := t.cache[key]; ok && now.Sub(e.at) < topologyTTL {
		return e.stacked
	}
	slaves, err := os.ReadDir(t.root + "/" + strconv.FormatUint(uint64(major), 10) + ":" + strconv.FormatUint(uint64(minor), 10) + "/slaves")
	e := stackedEntry{stacked: err == nil && len(slaves) > 0, at: now}
	t.cache[key] = e
	return e.stacked
}
