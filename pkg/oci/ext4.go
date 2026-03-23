package oci

import (
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
)

// createExt4FromDir creates an ext4 image from a directory tree using mke2fs -d.
// No mount/umount needed — avoids leaked loop devices.
func createExt4FromDir(srcDir, outputPath string) error {
	totalSize, fileCount, err := dirStats(srcDir)
	if err != nil {
		return err
	}

	// 30% headroom for ext4 metadata. Minimum 512MB.
	sizeMB := int(totalSize/1024/1024) * 130 / 100
	if sizeMB < 512 {
		sizeMB = 512
	}

	// Create sparse file
	f, err := os.Create(outputPath)
	if err != nil {
		return err
	}
	if err := f.Truncate(int64(sizeMB) << 20); err != nil {
		f.Close()
		return err
	}
	f.Close()

	// Inode count: 1 per 4KB or fileCount*1.5, whichever is more
	inodes := fileCount * 3 / 2
	if altInodes := totalSize / 4096; altInodes > int64(inodes) {
		inodes = int(altInodes)
	}
	if inodes < 1024 {
		inodes = 1024
	}

	cmd := exec.Command("mke2fs",
		"-t", "ext4",
		"-d", srcDir,
		"-N", fmt.Sprint(inodes),
		"-F", "-q",
		outputPath)
	if out, err := cmd.CombinedOutput(); err != nil {
		os.Remove(outputPath)
		return fmt.Errorf("mke2fs: %s: %w", out, err)
	}

	return nil
}

// dirStats walks a directory tree and returns total size in bytes and file count.
func dirStats(dir string) (totalSize int64, fileCount int, err error) {
	err = filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if !d.IsDir() {
			fileCount++
			if info, err := d.Info(); err == nil {
				totalSize += info.Size()
			}
		}
		return nil
	})
	return
}
