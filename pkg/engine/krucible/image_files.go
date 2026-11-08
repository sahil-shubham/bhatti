package krucible

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/sahil-shubham/bhatti/pkg/engine"
)

var _ engine.ImageFiles = (*Engine)(nil)

func (e *Engine) RemoveImageFiles(ctx context.Context, imagePath string) error {
	unlock, err := lockBasesCtx(ctx, e.cfg.DataDir, true)
	if err != nil {
		return err
	}
	defer unlock()
	return removeImageFiles(imagePath)
}

func removeImageFiles(imagePath string) error {
	// Retiring the authority first prevents inode reuse at this pathname from
	// inheriting an unrelated saved image's driver or backing permissions.
	if err := removeNodeMetadata(imagePath); err != nil {
		return err
	}
	if err := os.Remove(imagePath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return syncNodeDirectory(filepath.Dir(imagePath))
}

func (e *Engine) PublishRawImage(ctx context.Context, stagingPath, imagePath string) error {
	return e.publishImageFiles(ctx, stagingPath, imagePath, true)
}

func (e *Engine) PublishSavedImage(ctx context.Context, stagingPath, imagePath string) error {
	return e.publishImageFiles(ctx, stagingPath, imagePath, false)
}

func (e *Engine) publishImageFiles(ctx context.Context, stagingPath, imagePath string, raw bool) error {
	unlock, err := lockBasesCtx(ctx, e.cfg.DataDir, true)
	if err != nil {
		return err
	}
	defer unlock()
	var m nodeMetadata
	if raw {
		// A raw publisher knows the driver. Removing the old sidecar alone
		// would let guest-controlled magic select the legacy qcow2 reader.
		m = nodeMetadata{Format: "raw", Backing: []diskLayer{}}
	} else {
		found, err := readDaemonMetadata(nodeMetadataPath(stagingPath), &m)
		if err != nil {
			return err
		}
		if !found {
			return fmt.Errorf("saved image %s has no disk metadata", stagingPath)
		}
		if err := validateRecordedDisk(stagingPath, m.Format, m.Backing); err != nil {
			return err
		}
	}
	if err := flushFiles(stagingPath); err != nil {
		return err
	}
	fi, err := os.Stat(stagingPath)
	if err != nil {
		return err
	}
	if !raw && !m.matches(fi) {
		return fmt.Errorf("saved image %s has mismatched disk metadata", stagingPath)
	}
	k, ok := keyOf(fi)
	if !ok {
		return fmt.Errorf("image %s has no file identity", stagingPath)
	}
	m.Dev, m.Ino = k.dev, k.ino
	m.ImagePath, err = filepath.Abs(imagePath)
	if err != nil {
		return err
	}
	// The inode binds the two renames: startup can finish authority only for
	// the exact disk already moved into the final image pathname.
	if err := writeDaemonMetadata(nodeMetadataPath(stagingPath), m); err != nil {
		return err
	}
	if err := removeNodeMetadata(imagePath); err != nil {
		return err
	}
	if err := renameNode(stagingPath, imagePath); err != nil {
		return err
	}
	if err := os.Rename(nodeMetadataPath(stagingPath), nodeMetadataPath(imagePath)); err != nil {
		_ = removeImageFiles(imagePath)
		return err
	}
	if err := syncNodeDirectory(filepath.Dir(imagePath)); err != nil {
		_ = removeImageFiles(imagePath)
		return err
	}
	return nil
}

// Only startup can discard image stages: handlers release their storage lease
// between capture and publication, so an exclusive runtime sweep is not enough.
func cleanImageStages(dataDir string) error {
	if err := walkImageMetadata(dataDir, func(path, disk string) error {
		var m nodeMetadata
		found, err := readDaemonMetadata(path, &m)
		if err != nil || !found || m.ImagePath == "" || m.ImagePath == disk {
			return nil
		}
		fi, err := os.Stat(m.ImagePath)
		if errors.Is(err, os.ErrNotExist) || (err == nil && !m.matches(fi)) {
			return removeNodeMetadata(disk)
		}
		if err != nil {
			return err
		}
		if err := os.Rename(path, nodeMetadataPath(m.ImagePath)); err != nil {
			return err
		}
		return syncNodeDirectory(filepath.Dir(m.ImagePath))
	}); err != nil {
		return err
	}
	return walkManagedDiskFiles(dataDir, filepath.Join(dataDir, "images"), func(path string, _ bool) error {
		name := filepath.Base(path)
		if strings.HasPrefix(name, "..image-") && strings.HasSuffix(name, ".json") {
			path = filepath.Join(filepath.Dir(path), strings.TrimSuffix(strings.TrimPrefix(name, "."), ".json"))
			name = filepath.Base(path)
		}
		if strings.HasPrefix(name, ".image-") {
			return removeImageFiles(path)
		}
		return nil
	})
}
