package store

import (
	"database/sql"
	"time"
)

type TaskRecord struct {
	ID          string     `json:"id"`
	UserID      string     `json:"user_id"`
	Type        string     `json:"type"`
	Status      string     `json:"status"` // "running", "completed", "failed"
	Progress    string     `json:"progress"`
	ResultJSON  string     `json:"result,omitempty"`
	Error       string     `json:"error,omitempty"`
	CreatedAt   time.Time  `json:"created_at"`
	CompletedAt *time.Time `json:"completed_at,omitempty"`
}

// Sandbox is a running or stopped sandbox instance.

func (s *Store) CreateTask(t TaskRecord) error {
	_, err := s.db.Exec(
		`INSERT INTO tasks (id, user_id, type, status, progress, result_json, error, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		t.ID, t.UserID, t.Type, t.Status, t.Progress, t.ResultJSON, t.Error, t.CreatedAt,
	)
	return classify(err)
}

// GetTask retrieves a task by ID.
func (s *Store) GetTask(id string) (*TaskRecord, error) {
	var t TaskRecord
	var completedAt sql.NullTime
	err := s.db.QueryRow(
		`SELECT id, user_id, type, status, progress, result_json, error, created_at, completed_at
		 FROM tasks WHERE id = ?`, id).Scan(
		&t.ID, &t.UserID, &t.Type, &t.Status, &t.Progress,
		&t.ResultJSON, &t.Error, &t.CreatedAt, &completedAt,
	)
	if err != nil {
		return nil, noRows(err, "task %q not found", id)
	}
	if completedAt.Valid {
		t.CompletedAt = &completedAt.Time
	}
	return &t, nil
}

// UpdateTaskProgress updates the progress string on a running task.
func (s *Store) UpdateTaskProgress(id, progress string) error {
	_, err := s.db.Exec(`UPDATE tasks SET progress = ? WHERE id = ?`, progress, id)
	return err
}

// CompleteTask marks a task as completed with a result.
func (s *Store) CompleteTask(id, resultJSON string) error {
	now := time.Now()
	_, err := s.db.Exec(`UPDATE tasks SET status = 'completed', result_json = ?, completed_at = ? WHERE id = ?`,
		resultJSON, now, id)
	return err
}

// FailTask marks a task as failed with an error.
func (s *Store) FailTask(id, errMsg string) error {
	now := time.Now()
	_, err := s.db.Exec(`UPDATE tasks SET status = 'failed', error = ?, completed_at = ? WHERE id = ?`,
		errMsg, now, id)
	return err
}

// CleanupOldTasks removes completed/failed tasks older than the given duration.
func (s *Store) CleanupOldTasks(maxAge time.Duration) (int64, error) {
	cutoff := time.Now().Add(-maxAge)
	res, err := s.db.Exec(`DELETE FROM tasks WHERE created_at < ? AND status IN ('completed', 'failed')`, cutoff)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// ==========================================================================
// v0.4 Publish Rules (public proxy)
// ==========================================================================

// PublishRule maps a public alias to a sandbox port.
