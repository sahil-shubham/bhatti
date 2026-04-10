package store

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// Event represents a single observability event.
type Event struct {
	ID        int64          `json:"id"`
	Timestamp time.Time      `json:"ts"`
	Type      string         `json:"type"`
	UserID    string         `json:"user_id,omitempty"`
	SandboxID string         `json:"sandbox_id,omitempty"`
	Meta      map[string]any `json:"meta,omitempty"`
}

// MetricsSnapshot is a single 60-second metrics snapshot.
// Counter fields are deltas (requests in the last interval).
// Gauge fields are point-in-time values.
type MetricsSnapshot struct {
	Timestamp        time.Time `json:"ts"`
	APIRequests      int64     `json:"api_requests"`
	APIErrors        int64     `json:"api_errors"`
	APIAuthFailures  int64     `json:"api_auth_failures"`
	APIRateLimited   int64     `json:"api_rate_limited"`
	ProxyRequests    int64     `json:"proxy_requests"`
	ProxyErrors      int64     `json:"proxy_errors"`
	ProxyColdWakes   int64     `json:"proxy_cold_wakes"`
	ProxyRateLimited int64     `json:"proxy_rate_limited"`
	EventsDropped    int64     `json:"events_dropped"`
	SandboxesTotal   int       `json:"sandboxes_total"`
	SandboxesHot     int       `json:"sandboxes_hot"`
	SandboxesWarm    int       `json:"sandboxes_warm"`
	SandboxesCold    int       `json:"sandboxes_cold"`
	UsersTotal       int       `json:"users_total"`
	UsersActive      int       `json:"users_active"`
	WebsocketsActive int       `json:"websockets_active"`
	HostLoad1m       float64   `json:"host_load_1m"`
	HostMemTotalMB   int       `json:"host_mem_total_mb"`
	HostMemAvailMB   int       `json:"host_mem_avail_mb"`
}

// InsertEvents inserts a batch of events in a single transaction.
func (s *Store) InsertEvents(events []Event) error {
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback()

	stmt, err := tx.Prepare(`INSERT INTO events (type, user_id, sandbox_id, meta) VALUES (?, ?, ?, ?)`)
	if err != nil {
		return fmt.Errorf("prepare: %w", err)
	}
	defer stmt.Close()

	for _, e := range events {
		metaJSON, err := json.Marshal(e.Meta)
		if err != nil {
			metaJSON = []byte(`{"error":"marshal_failed"}`)
		}
		if _, err := stmt.Exec(e.Type, e.UserID, e.SandboxID, string(metaJSON)); err != nil {
			return fmt.Errorf("insert event %s: %w", e.Type, err)
		}
	}

	return tx.Commit()
}

// InsertMetricsSnapshot inserts a single metrics snapshot row.
func (s *Store) InsertMetricsSnapshot(m MetricsSnapshot) error {
	_, err := s.db.Exec(`INSERT INTO metrics_snapshots (
		api_requests, api_errors, api_auth_failures, api_rate_limited,
		proxy_requests, proxy_errors, proxy_cold_wakes, proxy_rate_limited,
		events_dropped,
		sandboxes_total, sandboxes_hot, sandboxes_warm, sandboxes_cold,
		users_total, users_active, websockets_active,
		host_load_1m, host_mem_total_mb, host_mem_avail_mb
	) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		m.APIRequests, m.APIErrors, m.APIAuthFailures, m.APIRateLimited,
		m.ProxyRequests, m.ProxyErrors, m.ProxyColdWakes, m.ProxyRateLimited,
		m.EventsDropped,
		m.SandboxesTotal, m.SandboxesHot, m.SandboxesWarm, m.SandboxesCold,
		m.UsersTotal, m.UsersActive, m.WebsocketsActive,
		m.HostLoad1m, m.HostMemTotalMB, m.HostMemAvailMB,
	)
	return err
}

// EventFilter specifies query criteria for listing events.
type EventFilter struct {
	Type      string // exact match or prefix (e.g. "thermal" matches "thermal.*")
	UserID    string
	SandboxID string
	Since     time.Time
	Limit     int
	CountOnly bool
}

// QueryEvents returns events matching the filter, ordered by timestamp descending.
func (s *Store) QueryEvents(f EventFilter) ([]Event, error) {
	var conditions []string
	var args []any

	if f.Type != "" {
		// Support prefix matching: "thermal" matches "thermal.pause", "thermal.wake", etc.
		if strings.ContainsRune(f.Type, '.') {
			conditions = append(conditions, "type = ?")
			args = append(args, f.Type)
		} else {
			conditions = append(conditions, "(type = ? OR type LIKE ?)")
			args = append(args, f.Type, f.Type+".%")
		}
	}
	if f.UserID != "" {
		conditions = append(conditions, "user_id = ?")
		args = append(args, f.UserID)
	}
	if f.SandboxID != "" {
		conditions = append(conditions, "sandbox_id = ?")
		args = append(args, f.SandboxID)
	}
	if !f.Since.IsZero() {
		conditions = append(conditions, "ts > ?")
		args = append(args, f.Since.UTC().Format("2006-01-02T15:04:05.000Z"))
	}

	where := ""
	if len(conditions) > 0 {
		where = "WHERE " + strings.Join(conditions, " AND ")
	}

	if f.CountOnly {
		var count int
		err := s.db.QueryRow("SELECT COUNT(*) FROM events "+where, args...).Scan(&count)
		if err != nil {
			return nil, err
		}
		return []Event{{Meta: map[string]any{"count": count}}}, nil
	}

	limit := 50
	if f.Limit > 0 {
		limit = f.Limit
	}

	query := fmt.Sprintf("SELECT id, ts, type, user_id, sandbox_id, meta FROM events %s ORDER BY ts DESC LIMIT ?", where)
	args = append(args, limit)

	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var events []Event
	for rows.Next() {
		var e Event
		var tsStr, metaStr string
		if err := rows.Scan(&e.ID, &tsStr, &e.Type, &e.UserID, &e.SandboxID, &metaStr); err != nil {
			return nil, err
		}
		e.Timestamp, _ = time.Parse("2006-01-02T15:04:05.000Z", tsStr)
		if e.Timestamp.IsZero() {
			e.Timestamp, _ = time.Parse("2006-01-02 15:04:05", tsStr)
		}
		json.Unmarshal([]byte(metaStr), &e.Meta)
		events = append(events, e)
	}
	return events, rows.Err()
}

// CountEvents returns the count of events matching the filter.
func (s *Store) CountEvents(f EventFilter) (int, error) {
	f.CountOnly = true
	events, err := s.QueryEvents(f)
	if err != nil {
		return 0, err
	}
	if len(events) > 0 {
		if c, ok := events[0].Meta["count"].(int); ok {
			return c, nil
		}
	}
	return 0, nil
}

// QueryMetricsSnapshots returns metrics snapshots since the given time.
func (s *Store) QueryMetricsSnapshots(since time.Time, limit int) ([]MetricsSnapshot, error) {
	query := `SELECT ts,
		api_requests, api_errors, api_auth_failures, api_rate_limited,
		proxy_requests, proxy_errors, proxy_cold_wakes, proxy_rate_limited,
		events_dropped,
		sandboxes_total, sandboxes_hot, sandboxes_warm, sandboxes_cold,
		users_total, users_active, websockets_active,
		host_load_1m, host_mem_total_mb, host_mem_avail_mb
		FROM metrics_snapshots WHERE ts > ? ORDER BY ts ASC`
	if limit > 0 {
		query += fmt.Sprintf(" LIMIT %d", limit)
	}

	rows, err := s.db.Query(query, since.UTC().Format("2006-01-02T15:04:05.000Z"))
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var snapshots []MetricsSnapshot
	for rows.Next() {
		var m MetricsSnapshot
		var tsStr string
		if err := rows.Scan(&tsStr,
			&m.APIRequests, &m.APIErrors, &m.APIAuthFailures, &m.APIRateLimited,
			&m.ProxyRequests, &m.ProxyErrors, &m.ProxyColdWakes, &m.ProxyRateLimited,
			&m.EventsDropped,
			&m.SandboxesTotal, &m.SandboxesHot, &m.SandboxesWarm, &m.SandboxesCold,
			&m.UsersTotal, &m.UsersActive, &m.WebsocketsActive,
			&m.HostLoad1m, &m.HostMemTotalMB, &m.HostMemAvailMB,
		); err != nil {
			return nil, err
		}
		m.Timestamp, _ = time.Parse("2006-01-02T15:04:05.000Z", tsStr)
		if m.Timestamp.IsZero() {
			m.Timestamp, _ = time.Parse("2006-01-02 15:04:05", tsStr)
		}
		snapshots = append(snapshots, m)
	}
	return snapshots, rows.Err()
}

// LatestMetricsSnapshot returns the most recent snapshot, or a zero value if none exist.
func (s *Store) LatestMetricsSnapshot() (MetricsSnapshot, error) {
	var m MetricsSnapshot
	var tsStr string
	err := s.db.QueryRow(`SELECT ts,
		api_requests, api_errors, api_auth_failures, api_rate_limited,
		proxy_requests, proxy_errors, proxy_cold_wakes, proxy_rate_limited,
		events_dropped,
		sandboxes_total, sandboxes_hot, sandboxes_warm, sandboxes_cold,
		users_total, users_active, websockets_active,
		host_load_1m, host_mem_total_mb, host_mem_avail_mb
		FROM metrics_snapshots ORDER BY ts DESC LIMIT 1`).Scan(&tsStr,
		&m.APIRequests, &m.APIErrors, &m.APIAuthFailures, &m.APIRateLimited,
		&m.ProxyRequests, &m.ProxyErrors, &m.ProxyColdWakes, &m.ProxyRateLimited,
		&m.EventsDropped,
		&m.SandboxesTotal, &m.SandboxesHot, &m.SandboxesWarm, &m.SandboxesCold,
		&m.UsersTotal, &m.UsersActive, &m.WebsocketsActive,
		&m.HostLoad1m, &m.HostMemTotalMB, &m.HostMemAvailMB,
	)
	if err != nil {
		return MetricsSnapshot{}, nil // no rows is not an error for callers
	}
	m.Timestamp, _ = time.Parse("2006-01-02T15:04:05.000Z", tsStr)
	return m, nil
}

// SumMetricsSnapshots returns column sums across all snapshots since the given time.
// Used by `bhatti admin status` to compute totals like "84,521 requests".
func (s *Store) SumMetricsSnapshots(since time.Time) (MetricsSnapshot, error) {
	var m MetricsSnapshot
	err := s.db.QueryRow(`SELECT
		COALESCE(SUM(api_requests), 0), COALESCE(SUM(api_errors), 0),
		COALESCE(SUM(api_auth_failures), 0), COALESCE(SUM(api_rate_limited), 0),
		COALESCE(SUM(proxy_requests), 0), COALESCE(SUM(proxy_errors), 0),
		COALESCE(SUM(proxy_cold_wakes), 0), COALESCE(SUM(proxy_rate_limited), 0),
		COALESCE(SUM(events_dropped), 0)
		FROM metrics_snapshots WHERE ts > ?`,
		since.UTC().Format("2006-01-02T15:04:05.000Z"),
	).Scan(
		&m.APIRequests, &m.APIErrors, &m.APIAuthFailures, &m.APIRateLimited,
		&m.ProxyRequests, &m.ProxyErrors, &m.ProxyColdWakes, &m.ProxyRateLimited,
		&m.EventsDropped,
	)
	return m, err
}

// PurgeOldEvents deletes events older than the given duration.
func (s *Store) PurgeOldEvents(retention time.Duration) (int64, error) {
	cutoff := time.Now().Add(-retention).UTC().Format("2006-01-02T15:04:05.000Z")
	res, err := s.db.Exec("DELETE FROM events WHERE ts < ?", cutoff)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// PurgeOldMetricsSnapshots deletes snapshots older than the given duration.
func (s *Store) PurgeOldMetricsSnapshots(retention time.Duration) (int64, error) {
	cutoff := time.Now().Add(-retention).UTC().Format("2006-01-02T15:04:05.000Z")
	res, err := s.db.Exec("DELETE FROM metrics_snapshots WHERE ts < ?", cutoff)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}
