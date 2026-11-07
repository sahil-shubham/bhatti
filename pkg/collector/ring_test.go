package collector

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"
)

func TestRingKeepsTheLastCapacityValuesOldestFirst(t *testing.T) {
	r := newRing[int](4)
	if _, ok := r.last(); ok || r.len() != 0 || len(r.appendTo(nil)) != 0 {
		t.Fatal("a new ring isn't empty")
	}
	var want []int
	for i := 1; i <= 11; i++ {
		r.push(i)
		want = append(want, i)
		if len(want) > 4 {
			want = want[1:]
		}
		if got := r.appendTo(nil); !slices.Equal(got, want) {
			t.Fatalf("after pushing %d: %v, want %v", i, got, want)
		}
		if last, ok := r.last(); !ok || last != i {
			t.Fatalf("after pushing %d: last = %d, %v", i, last, ok)
		}
		if r.len() != len(want) {
			t.Fatalf("after pushing %d: len = %d, want %d", i, r.len(), len(want))
		}
	}
}

func TestRingOfOne(t *testing.T) {
	r := newRing[string](1)
	r.push("a")
	r.push("b")
	if got := r.appendTo(nil); !slices.Equal(got, []string{"b"}) {
		t.Errorf("ring of one = %v", got)
	}
}

func TestRingGrowsAValueAtATime(t *testing.T) {
	// A sandbox that has lived a minute holds six points, not an hour's room.
	r := newRing[Point](360)
	for range 6 {
		r.push(Point{})
	}
	if cap(r.buf) > 16 {
		t.Errorf("a ring holding 6 values reserved room for %d", cap(r.buf))
	}
	if r.len() != 6 {
		t.Errorf("len = %d", r.len())
	}
}

func TestRingAppendToKeepsWhatIsAlreadyThere(t *testing.T) {
	r := newRing[int](3)
	for i := 1; i <= 5; i++ {
		r.push(i)
	}
	if got := r.appendTo([]int{0}); !slices.Equal(got, []int{0, 3, 4, 5}) {
		t.Errorf("appendTo = %v", got)
	}
}

func TestBlockTopologyFindsDevicesBuiltOnOthers(t *testing.T) {
	root := t.TempDir()
	mk := func(dev string, slaves ...string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Join(root, dev, "slaves"), 0o755); err != nil {
			t.Fatal(err)
		}
		for _, s := range slaves {
			if err := os.Symlink("../../"+s, filepath.Join(root, dev, "slaves", s)); err != nil {
				t.Fatal(err)
			}
		}
	}
	mk("9:0", "nvme0n1p2", "nvme1n1p2") // an md device on two disks
	mk("259:0")                         // a disk: a slaves directory with nothing in it
	now := time.Unix(1000, 0)
	topo := newBlockTopology(root, func() time.Time { return now })
	if !topo.stacked(9, 0) {
		t.Error("md0 on two disks isn't stacked")
	}
	if topo.stacked(259, 0) {
		t.Error("a disk is stacked")
	}
	// A device sysfs doesn't know, or a host with no sysfs, is counted, not dropped.
	if topo.stacked(8, 0) {
		t.Error("an unknown device is stacked")
	}
	if newBlockTopology(filepath.Join(root, "absent"), time.Now).stacked(9, 0) {
		t.Error("a host without sysfs has stacked devices")
	}

	// The answer is kept for a minute: a topology changes when someone sets up a
	// volume, not between samples.
	if err := os.RemoveAll(filepath.Join(root, "9:0")); err != nil {
		t.Fatal(err)
	}
	now = now.Add(topologyTTL - time.Second)
	if !topo.stacked(9, 0) {
		t.Error("the answer wasn't kept")
	}
	now = now.Add(2 * time.Second)
	if topo.stacked(9, 0) {
		t.Error("a device that is gone is still stacked after the answer expired")
	}
}

func TestDeviceNumbersDontCollideInTheTopologyCache(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "1:256", "slaves"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "1:256", "slaves", "x"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	topo := newBlockTopology(root, time.Now)
	if !topo.stacked(1, 256) {
		t.Fatal("1:256 isn't stacked")
	}
	// 1:256 and 257:0 would be one key if the major and minor were added, or
	// shifted by too few bits.
	if topo.stacked(257, 0) {
		t.Error("257:0 took 1:256's answer")
	}
}
