package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

type ImageRecord struct {
	ID            string    `json:"id"`
	UserID        string    `json:"user_id"` // "" = admin/global
	Name          string    `json:"name"`
	Source        string    `json:"source"`
	FilePath      string    `json:"-"`
	SizeMB        int       `json:"size_mb"`
	OCIDigest     string    `json:"oci_digest,omitempty"`
	OCIConfigJSON string    `json:"-"`
	CreatedAt     time.Time `json:"created_at"`
}

func (s *Store) CreateImage(ctx context.Context, img ImageRecord) error {
	_, err := s.db.ExecContext(
		ctx, `INSERT INTO images (id, user_id, name, source, file_path, size_mb, oci_digest, oci_config_json, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		img.ID, img.UserID, img.Name, img.Source, img.FilePath,
		img.SizeMB, img.OCIDigest, img.OCIConfigJSON, img.CreatedAt,
	)
	return classify(err)
}

// GetImage retrieves an image by user and name: the user's own, else one
// shared with them, else a global admin image (user_id=”). A query that fails
// ends the search instead of reading as "not there", so it can't fall through
// to a different image.
func (s *Store) GetImage(ctx context.Context, userID, name string) (*ImageRecord, error) {
	var img ImageRecord
	const cols = `id, user_id, name, source, file_path, size_mb, oci_digest, oci_config_json, created_at`
	for _, q := range []struct {
		sql  string
		args []any
	}{
		{`SELECT ` + cols + ` FROM images WHERE user_id = ? AND name = ?`, []any{userID, name}},
		{`SELECT i.id, i.user_id, i.name, i.source, i.file_path,
		i.size_mb, i.oci_digest, i.oci_config_json, i.created_at
		FROM images i JOIN image_shares s ON s.image_id = i.id
		WHERE s.user_id = ? AND i.name = ?`, []any{userID, name}},
		{`SELECT ` + cols + ` FROM images WHERE user_id = '' AND name = ?`, []any{name}},
	} {
		err := s.db.QueryRowContext(ctx, q.sql, q.args...).Scan(&img.ID, &img.UserID, &img.Name, &img.Source, &img.FilePath,
			&img.SizeMB, &img.OCIDigest, &img.OCIConfigJSON, &img.CreatedAt)
		if err == nil {
			return &img, nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return nil, err
		}
	}
	return nil, wrap(ErrNotFound, "image %q not found", name)
}

// ListImages returns all images visible to a user (their own + shared + admin).
func (s *Store) ListImages(ctx context.Context, userID string) ([]ImageRecord, error) {
	rows, err := s.db.QueryContext(
		ctx, `SELECT id, user_id, name, source, file_path, size_mb, oci_digest, oci_config_json, created_at
		 FROM images
		 WHERE user_id = ?
		    OR user_id = ''
		    OR id IN (SELECT image_id FROM image_shares WHERE user_id = ?)
		 ORDER BY created_at DESC`, userID, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ImageRecord
	for rows.Next() {
		var img ImageRecord
		if err := rows.Scan(&img.ID, &img.UserID, &img.Name, &img.Source, &img.FilePath,
			&img.SizeMB, &img.OCIDigest, &img.OCIConfigJSON, &img.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, img)
	}
	return out, rows.Err()
}

// DeleteImage removes an image record.
func (s *Store) DeleteImage(ctx context.Context, userID, name string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM images WHERE user_id = ? AND name = ?`, userID, name)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return wrap(ErrNotFound, "image %q not found", name)
	}
	return nil
}

// DeleteImageByID removes only the registration owned by a failed publisher.
// A missing identity is already clean; a same-name replacement is not ours.
func (s *Store) DeleteImageByID(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM images WHERE id = ?`, id)
	return err
}

// --- Image Sharing ---

// ShareImage grants a user access to an image.
func (s *Store) ShareImage(ctx context.Context, imageID, userID string) error {
	_, err := s.db.ExecContext(ctx, `INSERT OR IGNORE INTO image_shares (image_id, user_id) VALUES (?, ?)`, imageID, userID)
	return err
}

// UnshareImage revokes a user's access to an image.
func (s *Store) UnshareImage(ctx context.Context, imageID, userID string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM image_shares WHERE image_id = ? AND user_id = ?`, imageID, userID)
	return err
}

// ListImageShares returns user IDs that an image is shared with.
func (s *Store) ListImageShares(ctx context.Context, imageID string) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT s.user_id, COALESCE(u.name, s.user_id)
		FROM image_shares s LEFT JOIN users u ON u.id = s.user_id
		WHERE s.image_id = ?`, imageID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var names []string
	for rows.Next() {
		var uid, name string
		if err := rows.Scan(&uid, &name); err != nil {
			return nil, fmt.Errorf("store: scan image share: %w", err)
		}
		names = append(names, name)
	}
	return names, rows.Err()
}

// GetImageByName looks up an image by name regardless of owner.
func (s *Store) GetImageByName(ctx context.Context, name string) (*ImageRecord, error) {
	var img ImageRecord
	err := s.db.QueryRowContext(
		ctx, `SELECT id, user_id, name, source, file_path, size_mb, oci_digest, oci_config_json, created_at
		 FROM images WHERE name = ? LIMIT 1`, name).Scan(
		&img.ID, &img.UserID, &img.Name, &img.Source, &img.FilePath,
		&img.SizeMB, &img.OCIDigest, &img.OCIConfigJSON, &img.CreatedAt)
	if err != nil {
		return nil, noRows(err, "image %q not found", name)
	}
	return &img, nil
}

// GetUserByName looks up a user by name.
