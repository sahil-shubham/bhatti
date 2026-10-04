package krucible

import (
	"encoding/binary"
	"fmt"
	"os"
)

// qcow2 overlay geometry. 64 KiB clusters and 16-bit refcounts are qemu-img's
// defaults, so the result is the same image `qemu-img create -f qcow2 -b <base>
// -F raw` would produce.
const (
	qcow2ClusterBits    = 16
	qcow2ClusterSize    = 1 << qcow2ClusterBits
	qcow2HeaderLen      = 104 // v2 header (72) + v3 fields (32)
	qcow2RefcountOrder  = 4   // 16-bit refcount entries
	qcow2ExtBackingFmt  = 0xE2792ACA
	qcow2MaxBackingName = 1023 // qcow2 spec limit
)

// createQcow2Overlay writes an empty qcow2 v3 image at dst that is backed by
// the raw image at backing, with virtual size `size`. Every read falls
// through to the backing file until the guest writes, so creation is O(1)
// regardless of the base's size and needs no reflink-capable filesystem.
//
// Layout (one cluster each, then the L1 table):
//
//	cluster 0  header, backing-format extension, backing file name
//	cluster 1  refcount table (one entry -> cluster 2)
//	cluster 2  refcount block (refcount 1 for every metadata cluster)
//	cluster 3+ L1 table, all zero (no L2 tables yet)
func createQcow2Overlay(dst, backing string, size uint64) error {
	if size == 0 {
		return fmt.Errorf("qcow2 overlay: zero virtual size")
	}
	// Readers treat the size as whole 512-byte sectors and drop a partial one;
	// round up (as `qemu-img create` does) so the base's tail stays visible.
	size = (size + 511) &^ 511

	// One L2 table maps clusterSize/8 clusters; one L1 entry per L2 table.
	const bytesPerL2 = uint64(qcow2ClusterSize) * (qcow2ClusterSize / 8)
	l1Entries := (size + bytesPerL2 - 1) / bytesPerL2
	l1Clusters := (l1Entries*8 + qcow2ClusterSize - 1) / qcow2ClusterSize
	const (
		refTableOff = 1 * qcow2ClusterSize
		refBlockOff = 2 * qcow2ClusterSize
		l1Off       = 3 * qcow2ClusterSize
	)
	totalClusters := 3 + l1Clusters
	// A single refcount block covers clusterSize/2 clusters (16-bit entries);
	// the L1 for any realistic disk is a handful of clusters.
	if totalClusters > qcow2ClusterSize/2 {
		return fmt.Errorf("qcow2 overlay: virtual size %d too large", size)
	}

	// Cluster 0: header + extensions + backing name.
	c0 := make([]byte, qcow2ClusterSize)
	be := binary.BigEndian
	copy(c0[0:4], "QFI\xfb")
	be.PutUint32(c0[4:], 3) // version
	// Extensions start at header_length: backing format ("raw", padded to 8),
	// then the end marker; the backing file name follows.
	ext := qcow2HeaderLen
	be.PutUint32(c0[ext:], qcow2ExtBackingFmt)
	be.PutUint32(c0[ext+4:], 3)
	copy(c0[ext+8:], "raw")
	ext += 8 + 8
	ext += 8 // end-of-extensions: type 0, length 0 (already zero)
	backingOff := ext
	copy(c0[backingOff:], backing)

	be.PutUint64(c0[8:], uint64(backingOff))
	be.PutUint32(c0[16:], uint32(len(backing)))
	be.PutUint32(c0[20:], qcow2ClusterBits)
	be.PutUint64(c0[24:], size)
	be.PutUint32(c0[32:], 0) // crypt_method
	be.PutUint32(c0[36:], uint32(l1Entries))
	be.PutUint64(c0[40:], l1Off)
	be.PutUint64(c0[48:], refTableOff)
	be.PutUint32(c0[56:], 1) // refcount_table_clusters
	// nb_snapshots, snapshots_offset, incompatible/compatible/autoclear: 0.
	be.PutUint32(c0[96:], qcow2RefcountOrder)
	be.PutUint32(c0[100:], qcow2HeaderLen)

	refTable := make([]byte, qcow2ClusterSize)
	be.PutUint64(refTable[0:], refBlockOff)

	refBlock := make([]byte, qcow2ClusterSize)
	for i := uint64(0); i < totalClusters; i++ {
		be.PutUint16(refBlock[i*2:], 1)
	}

	f, err := os.OpenFile(dst, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	// The L1 clusters are all zero: extend the file to cover them sparsely.
	err = f.Truncate(int64(totalClusters * qcow2ClusterSize))
	for _, w := range []struct {
		off int64
		b   []byte
	}{{0, c0}, {refTableOff, refTable}, {refBlockOff, refBlock}} {
		if err != nil {
			break
		}
		_, err = f.WriteAt(w.b, w.off)
	}
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(dst)
		return fmt.Errorf("qcow2 overlay %s: %w", dst, err)
	}
	return nil
}

// qcow2BackingRef is where a qcow2 image records its backing file's name.
type qcow2BackingRef struct {
	offset  int64  // of the name; 0 = no backing file
	size    uint32 // of the name
	cluster int64  // the image's cluster size
}

// readQcow2BackingRef reads the header fields that place the backing name.
func readQcow2BackingRef(f *os.File) (qcow2BackingRef, error) {
	var h [24]byte
	if _, err := f.ReadAt(h[:], 0); err != nil {
		return qcow2BackingRef{}, fmt.Errorf("read qcow2 header: %w", err)
	}
	be := binary.BigEndian
	if string(h[:4]) != "QFI\xfb" {
		return qcow2BackingRef{}, fmt.Errorf("not a qcow2 image")
	}
	if v := be.Uint32(h[4:]); v != 2 && v != 3 {
		return qcow2BackingRef{}, fmt.Errorf("qcow2 version %d", v)
	}
	// The spec allows 9 (512 B) to 21 (2 MiB).
	bits := be.Uint32(h[20:])
	if bits < 9 || bits > 21 {
		return qcow2BackingRef{}, fmt.Errorf("qcow2 cluster bits %d", bits)
	}
	ref := qcow2BackingRef{size: be.Uint32(h[16:]), cluster: 1 << bits}
	off := be.Uint64(h[8:])
	if off == 0 {
		return ref, nil
	}
	// The name lives in the header cluster, after the header extensions.
	if off >= uint64(ref.cluster) || off+uint64(ref.size) > uint64(ref.cluster) || ref.size > qcow2MaxBackingName {
		return qcow2BackingRef{}, fmt.Errorf("qcow2 backing name at %d+%d is outside the header cluster", off, ref.size)
	}
	ref.offset = int64(off)
	return ref, nil
}

// qcow2Backing returns the backing file name the qcow2 image at path records,
// "" when it has none.
func qcow2Backing(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	ref, err := readQcow2BackingRef(f)
	if err != nil || ref.offset == 0 {
		return "", err
	}
	name := make([]byte, ref.size)
	if _, err := f.ReadAt(name, ref.offset); err != nil {
		return "", fmt.Errorf("read qcow2 backing name: %w", err)
	}
	return string(name), nil
}

// setQcow2Backing points the qcow2 image at path, which has a backing file,
// at another one with the same contents (the image's clusters only make sense
// over those). The name is rewritten in place: it starts where the old one
// did, and the rest of the header cluster after the header extensions is the
// name's to use.
func setQcow2Backing(path, backing string) error {
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return err
	}
	defer f.Close()
	ref, err := readQcow2BackingRef(f)
	if err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	if ref.offset == 0 {
		return fmt.Errorf("%s has no backing file to replace", path)
	}
	if len(backing) == 0 || len(backing) > qcow2MaxBackingName || ref.offset+int64(len(backing)) > ref.cluster {
		return fmt.Errorf("%s: backing name %q doesn't fit the qcow2 header", path, backing)
	}
	// Zero what's left of a longer old name, then set the new length.
	name := make([]byte, max(len(backing), int(ref.size)))
	copy(name, backing)
	if _, err := f.WriteAt(name, ref.offset); err != nil {
		return fmt.Errorf("%s: write backing name: %w", path, err)
	}
	var size [4]byte
	binary.BigEndian.PutUint32(size[:], uint32(len(backing)))
	if _, err := f.WriteAt(size[:], 16); err != nil {
		return fmt.Errorf("%s: write backing name size: %w", path, err)
	}
	return f.Sync()
}
