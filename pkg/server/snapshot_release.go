package server

import (
	"context"

	"github.com/sahil-shubham/bhatti/pkg/engine"
)

// removeSnapshotFiles has the engine delete the snapshot directory when the
// engine can say whether anything is built on it (engine.SnapshotRemover), and
// reports whether it did; an engine whose snapshots are self-contained leaves
// the deleting to the caller. The error is the engine's, ErrSnapshotInUse
// among them.
func (s *Server) removeSnapshotFiles(ctx context.Context, snapDir string) (removed bool, err error) {
	rm, ok := s.engine.(engine.SnapshotRemover)
	if !ok {
		return false, nil
	}
	return true, rm.RemoveSnapshot(ctx, snapDir)
}
