package store

import (
	"encoding/json"
	"fmt"
	"time"
)

type TemplateMountSpec struct {
	VolumeName string `json:"volume_name"` // empty = "bhatti-{sandbox_name}-workspace"
	Target     string `json:"target"`
	ReadOnly   bool   `json:"readonly"`
	AutoCreate bool   `json:"auto_create"` // create volume if missing
}

// Template is a sandbox blueprint.
type Template struct {
	ID         string              `json:"id"`
	Name       string              `json:"name"`
	Engine     string              `json:"engine"`
	Image      string              `json:"image"`
	CPUs       float64             `json:"cpus"`
	MemoryMB   int                 `json:"memory_mb"`
	DiskSizeMB int                 `json:"disk_size_mb"`
	UserData   string              `json:"userdata"`
	Secrets    []string            `json:"secrets"`
	Labels     map[string]string   `json:"labels"`
	Mounts     []TemplateMountSpec `json:"mounts"`
	CreatedAt  time.Time           `json:"created_at"`
}

func (s *Store) CreateTemplate(t Template) error {
	secretsJSON, _ := json.Marshal(t.Secrets)
	labelsJSON, _ := json.Marshal(t.Labels)
	mountsJSON, _ := json.Marshal(t.Mounts)
	if t.Mounts == nil {
		mountsJSON = []byte("[]")
	}
	_, err := s.db.Exec(
		`INSERT INTO templates (id, name, engine, image, cpus, memory_mb, disk_size_mb, userdata, secrets_json, labels_json, mounts_json, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		t.ID, t.Name, t.Engine, t.Image, t.CPUs, t.MemoryMB, t.DiskSizeMB, t.UserData,
		string(secretsJSON), string(labelsJSON), string(mountsJSON), t.CreatedAt,
	)
	return classify(err)
}

func (s *Store) GetTemplate(id string) (*Template, error) {
	row := s.db.QueryRow(`SELECT id, name, engine, image, cpus, memory_mb, disk_size_mb, userdata, secrets_json, labels_json, mounts_json, created_at FROM templates WHERE id = ?`, id)
	return scanTemplate(row)
}

func (s *Store) ListTemplates() ([]Template, error) {
	rows, err := s.db.Query(`SELECT id, name, engine, image, cpus, memory_mb, disk_size_mb, userdata, secrets_json, labels_json, mounts_json, created_at FROM templates ORDER BY created_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Template
	for rows.Next() {
		t, err := scanTemplate(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *t)
	}
	return out, rows.Err()
}

func (s *Store) DeleteTemplate(id string) error {
	res, err := s.db.Exec(`DELETE FROM templates WHERE id = ?`, id)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return wrap(ErrNotFound, "template %q not found", id)
	}
	return nil
}

// scanner is satisfied by both *sql.Row and *sql.Rows.
type scanner interface {
	Scan(dest ...any) error
}

func scanTemplate(s scanner) (*Template, error) {
	var t Template
	var secretsJSON, labelsJSON, mountsJSON string
	err := s.Scan(&t.ID, &t.Name, &t.Engine, &t.Image, &t.CPUs, &t.MemoryMB, &t.DiskSizeMB, &t.UserData, &secretsJSON, &labelsJSON, &mountsJSON, &t.CreatedAt)
	if err != nil {
		return nil, classify(err)
	}
	if err := json.Unmarshal([]byte(secretsJSON), &t.Secrets); err != nil {
		return nil, fmt.Errorf("unmarshal secrets: %w", err)
	}
	if err := json.Unmarshal([]byte(labelsJSON), &t.Labels); err != nil {
		return nil, fmt.Errorf("unmarshal labels: %w", err)
	}
	if err := json.Unmarshal([]byte(mountsJSON), &t.Mounts); err != nil {
		return nil, fmt.Errorf("unmarshal mounts: %w", err)
	}
	return &t, nil
}

// --- Sandboxes ---

// --- Secrets ---
