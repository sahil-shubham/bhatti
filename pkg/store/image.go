package store

import (
	"database/sql"
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

func (s *Store) CreateImage(img ImageRecord) error {
	_, err := s.db.Exec(
		`INSERT INTO images (id, user_id, name, source, file_path, size_mb, oci_digest, oci_config_json, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		img.ID, img.UserID, img.Name, img.Source, img.FilePath,
		img.SizeMB, img.OCIDigest, img.OCIConfigJSON, img.CreatedAt,
	)
	return err
}

// GetImage retrieves an image by user and name. Falls back to admin images (user_id=”).
func (s *Store) GetImage(userID, name string) (*ImageRecord, error) {
	var img ImageRecord
	const cols = `id, user_id, name, source, file_path, size_mb, oci_digest, oci_config_json, created_at`
	scanImg := func(row *sql.Row) error {
		return row.Scan(&img.ID, &img.UserID, &img.Name, &img.Source, &img.FilePath,
			&img.SizeMB, &img.OCIDigest, &img.OCIConfigJSON, &img.CreatedAt)
	}

	// 1. User's own image
	if scanImg(s.db.QueryRow(`SELECT `+cols+` FROM images WHERE user_id = ? AND name = ?`, userID, name)) == nil {
		return &img, nil
	}
	// 2. Image shared with this user
	if scanImg(s.db.QueryRow(`SELECT i.id, i.user_id, i.name, i.source, i.file_path,
		i.size_mb, i.oci_digest, i.oci_config_json, i.created_at
		FROM images i JOIN image_shares s ON s.image_id = i.id
		WHERE s.user_id = ? AND i.name = ?`, userID, name)) == nil {
		return &img, nil
	}
	// 3. Global admin image (user_id = '')
	if scanImg(s.db.QueryRow(`SELECT `+cols+` FROM images WHERE user_id = '' AND name = ?`, name)) == nil {
		return &img, nil
	}
	return nil, fmt.Errorf("image %q not found", name)
}

// ListImages returns all images visible to a user (their own + shared + admin).
func (s *Store) ListImages(userID string) ([]ImageRecord, error) {
	rows, err := s.db.Query(
		`SELECT id, user_id, name, source, file_path, size_mb, oci_digest, oci_config_json, created_at
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
func (s *Store) DeleteImage(userID, name string) error {
	res, err := s.db.Exec(`DELETE FROM images WHERE user_id = ? AND name = ?`, userID, name)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return fmt.Errorf("image %q not found", name)
	}
	return nil
}

// --- Image Sharing ---

// ShareImage grants a user access to an image.
func (s *Store) ShareImage(imageID, userID string) error {
	_, err := s.db.Exec(`INSERT OR IGNORE INTO image_shares (image_id, user_id) VALUES (?, ?)`, imageID, userID)
	return err
}

// UnshareImage revokes a user's access to an image.
func (s *Store) UnshareImage(imageID, userID string) error {
	_, err := s.db.Exec(`DELETE FROM image_shares WHERE image_id = ? AND user_id = ?`, imageID, userID)
	return err
}

// ListImageShares returns user IDs that an image is shared with.
func (s *Store) ListImageShares(imageID string) ([]string, error) {
	rows, err := s.db.Query(`SELECT s.user_id, COALESCE(u.name, s.user_id)
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
func (s *Store) GetImageByName(name string) (*ImageRecord, error) {
	var img ImageRecord
	err := s.db.QueryRow(
		`SELECT id, user_id, name, source, file_path, size_mb, oci_digest, oci_config_json, created_at
		 FROM images WHERE name = ? LIMIT 1`, name).Scan(
		&img.ID, &img.UserID, &img.Name, &img.Source, &img.FilePath,
		&img.SizeMB, &img.OCIDigest, &img.OCIConfigJSON, &img.CreatedAt)
	if err != nil {
		return nil, fmt.Errorf("image %q not found", name)
	}
	return &img, nil
}

// GetUserByName looks up a user by name.
