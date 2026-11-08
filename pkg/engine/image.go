package engine

import "context"

// ImageFiles is optionally implemented by engines whose saved images include
// daemon-owned format or provenance records beside the disk.
type ImageFiles interface {
	RemoveImageFiles(ctx context.Context, imagePath string) error
	PublishRawImage(ctx context.Context, stagingPath, imagePath string) error
	PublishSavedImage(ctx context.Context, stagingPath, imagePath string) error
}
