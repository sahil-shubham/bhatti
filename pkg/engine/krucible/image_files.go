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
	if raw {
		// A raw publisher knows the driver. Removing the old sidecar alone
		// would let guest-controlled magic select the legacy qcow2 reader.
		if err := e.recordNodeMetadata(stagingPath, "raw", []diskLayer{}); err != nil {
			return err
		}
	} else {
		var m nodeMetadata
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
	if err := removeNodeMetadata(imagePath); err != nil {
		return err
	}
	if err := os.Rename(stagingPath, imagePath); err != nil {
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
