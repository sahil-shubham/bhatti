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
