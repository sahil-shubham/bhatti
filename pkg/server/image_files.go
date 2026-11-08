package server

import (
	"context"
	"errors"
	"os"
	"sync"

	"github.com/sahil-shubham/bhatti/pkg/engine"
	"github.com/sahil-shubham/bhatti/pkg/store"
)

type imageName struct{ userID, name string }

type imageGate struct {
	mu   sync.Mutex
	refs int // holders and waiters; retain the gate until both are gone
}

// lockImage serializes reuse of a name, not just writes to its current record.
// Conversion/capture use private stages and need not hold this gate.
func (s *Server) lockImage(userID, name string) func() {
	key := imageName{userID, name}
	s.imageMu.Lock()
	if s.imageGates == nil {
		s.imageGates = make(map[imageName]*imageGate)
	}
	gate := s.imageGates[key]
	if gate == nil {
		gate = &imageGate{}
		s.imageGates[key] = gate
	}
	gate.refs++
	s.imageMu.Unlock()
	if s.onImageLock != nil {
		s.onImageLock(userID, name)
	}
	gate.mu.Lock()
	return func() {
		gate.mu.Unlock()
		s.imageMu.Lock()
		gate.refs--
		if gate.refs == 0 {
			delete(s.imageGates, key)
		}
		s.imageMu.Unlock()
	}
}

func (s *Server) registerImage(ctx context.Context, img store.ImageRecord, staging string, raw bool) error {
	unlock := s.lockImage(img.UserID, img.Name)
	defer unlock()
	if err := s.store.CreateImage(detached(ctx), img); err != nil {
		return err
	}
	if err := s.publishImageFiles(ctx, staging, img.FilePath, raw); err != nil {
		// The name is reusable; cleanup owns only this registration's identity.
		_ = s.store.DeleteImageByID(detached(ctx), img.ID)
		return err
	}
	return nil
}

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
