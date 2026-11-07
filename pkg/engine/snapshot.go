package engine

import (
	"context"
	"errors"
	"io"
)

// ErrBadSnapshotArchive is wrapped by import errors for an archive that isn't
// a whole, intact snapshot export: truncated, corrupt, or holding something
// other than what its manifest lists. The API reports it as 400.
var ErrBadSnapshotArchive = errors.New("not a valid snapshot archive")

// ErrSnapshotIncompatible is wrapped by import errors for a snapshot this host
// can't run: taken on another architecture or OS, on a CPU with features this
// host lacks, or over a base image this host doesn't have. The API reports it
// as 422 with the engine's message.
var ErrSnapshotIncompatible = errors.New("snapshot can't run on this host")

// ErrSnapshotInUse is wrapped by RemoveSnapshot's refusal of a snapshot whose
// disks other disks are layered over: the sandboxes, snapshots and saved
// images that would stop working. The API reports it as 409 with the engine's
// message, which says how many.
var ErrSnapshotInUse = errors.New("snapshot is in use")

// SnapshotRemover is optionally implemented by engines whose snapshot disks
// can be layered over: resuming creates a disk over the snapshot's, so
// deleting the snapshot's files is not always safe.
type SnapshotRemover interface {
	// RemoveSnapshot deletes the snapshot directory snapDir, or refuses with
	// ErrSnapshotInUse while a disk made over its files is still there. A
	// directory that is gone already is not an error.
	RemoveSnapshot(ctx context.Context, snapDir string) error
}

// SnapshotExport configures ExportSnapshot.
type SnapshotExport struct {
	// Name is the snapshot's name, offered to the importer as its default.
	Name string
	// IncludeBase carries the root disk's base image in the archive, for a
	// host that doesn't have it.
	IncludeBase bool
}

// SnapshotImport describes a snapshot ImportSnapshot unpacked.
type SnapshotImport struct {
	Name         string // the snapshot's name where it was exported
	Type         string // "memory" | "filesystem"
	ManifestJSON []byte // the engine's manifest for the unpacked snapshot directory
}

// SnapshotPorter is optionally implemented by engines that can move a named
// snapshot to another host of the same OS and architecture.
type SnapshotPorter interface {
	// ExportSnapshot writes the snapshot in snapDir, whose manifest the engine
	// returned when it was taken, to w as one self-contained archive. It
	// validates everything it can before writing the first byte; an error
	// after that leaves w with a truncated archive the caller must discard.
	ExportSnapshot(ctx context.Context, w io.Writer, snapDir string, manifestJSON []byte, opts SnapshotExport) error

	// ImportSnapshot unpacks an archive ExportSnapshot wrote into destDir,
	// which it creates (its parent must exist), and checks that this host can
	// run the snapshot. On error destDir is gone. The directory can be renamed
	// before use: the manifest names its files relative to it.
	ImportSnapshot(ctx context.Context, r io.Reader, destDir string) (SnapshotImport, error)
}
