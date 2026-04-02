// Package backup provides volume backup to S3-compatible object storage.
package backup

import (
	"context"
	"io"
	"time"
)

// Entry describes a backup object in remote storage.
type Entry struct {
	Key       string
	Size      int64
	Timestamp time.Time
}

// Backend is the interface for backup storage providers.
type Backend interface {
	Upload(ctx context.Context, key string, r io.ReadSeeker, size int64) error
	Download(ctx context.Context, key string) (io.ReadCloser, error)
	List(ctx context.Context, prefix string) ([]Entry, error)
	Delete(ctx context.Context, key string) error
}
