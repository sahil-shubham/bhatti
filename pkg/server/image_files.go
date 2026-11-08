package server

import (
	"context"
	"errors"
	"os"

	"github.com/sahil-shubham/bhatti/pkg/engine"
)

func (s *Server) removeImageFiles(ctx context.Context, path string) error {
	if files, ok := s.engine.(engine.ImageFiles); ok {
		return files.RemoveImageFiles(ctx, path)
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

func (s *Server) publishImageFiles(ctx context.Context, staging, final string, raw bool) error {
	if files, ok := s.engine.(engine.ImageFiles); ok {
		if raw {
			return files.PublishRawImage(ctx, staging, final)
		}
		return files.PublishSavedImage(ctx, staging, final)
	}
	return os.Rename(staging, final)
}

func imageStage(dir string) (string, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	f, err := os.CreateTemp(dir, ".image-*.ext4")
	if err != nil {
		return "", err
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(f.Name())
		return "", err
	}
	return f.Name(), nil
}
