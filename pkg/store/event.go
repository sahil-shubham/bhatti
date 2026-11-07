package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
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
//
// A producer that knows a sandbox's owner must capture it before buffering:
// deletion can remove the sandbox before this transaction runs. The lookup is
// only a fallback for callers that know the sandbox ID but not its owner.
func (s *Store) InsertEvents(ctx context.Context, events []Event) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback()

	stmt, err := tx.PrepareContext(ctx, `INSERT INTO events (type, user_id, sandbox_id, meta) VALUES (?1,
		CASE WHEN ?2 != '' OR ?3 = '' THEN ?2
		ELSE COALESCE((SELECT created_by FROM sandboxes WHERE id = ?3), '') END,
		?3, ?4)`)
	if err != nil {
		return fmt.Errorf("prepare: %w", err)
	}
	defer stmt.Close()

	for _, e := range events {
		metaJSON, err := json.Marshal(e.Meta)
		if err != nil {
			metaJSON = []byte(`{"error":"marshal_failed"}`)
		}
		if _, err := stmt.ExecContext(ctx, e.Type, e.UserID, e.SandboxID, string(metaJSON)); err != nil {
			return fmt.Errorf("insert event %s: %w", e.Type, err)
		}
	}

	return tx.Commit()
}

// InsertMetricsSnapshot inserts a single metrics snapshot row.
func (s *Store) InsertMetricsSnapshot(ctx context.Context, m MetricsSnapshot) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO metrics_snapshots (
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
func (s *Store) QueryEvents(ctx context.Context, f EventFilter) ([]Event, error) {
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
		err := s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM events "+where, args...).Scan(&count)
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

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var events []Event
	for rows.Next() {
		e, err := scanEvent(rows)
		if err != nil {
			return nil, err
		}
		events = append(events, e)
	}
	return events, rows.Err()
}

// scanEvent reads the six columns every events query selects. A meta that is
// not valid JSON leaves Meta empty instead of failing the read: InsertEvents
// writes valid JSON, and one damaged row must not stop a reader from getting
// the rows after it.
func scanEvent(rows *sql.Rows) (Event, error) {
	var e Event
	var tsStr, metaStr string
	if err := rows.Scan(&e.ID, &tsStr, &e.Type, &e.UserID, &e.SandboxID, &metaStr); err != nil {
		return Event{}, err
	}
	e.Timestamp, _ = time.Parse("2006-01-02T15:04:05.000Z", tsStr)
	if e.Timestamp.IsZero() {
		e.Timestamp, _ = time.Parse("2006-01-02 15:04:05", tsStr)
	}
	json.Unmarshal([]byte(metaStr), &e.Meta)
	return e, nil
}

// EventQuery selects one user's events past a position in the events table.
type EventQuery struct {
	UserID     string // whose events; required, so events with no user can never be asked for through here
	After      int64  // only events with a greater id
	TypePrefix string // when set, only events whose type starts with it
	SandboxID  string // when set, only this sandbox's events
	Limit      int    // at most this many events; must be positive
}

// EventPage couples matching events with the retention boundary from the same
// read snapshot. ScannedThrough is safe to resume after even when filters
// rejected every event; a full page stops at its last returned event instead.
type EventPage struct {
	Events         []Event
	Oldest         int64
	ScannedThrough int64
}

// EventsAfter returns one user's matching events in increasing ID order.
// The read transaction ends before returning, so a slow consumer holds neither
// a SQLite snapshot nor its WAL history while writing the page to the network.
func (s *Store) EventsAfter(ctx context.Context, q EventQuery) (EventPage, error) {
	if q.UserID == "" {
		return EventPage{}, errors.New("store: events are read for a user")
	}
	if q.Limit <= 0 {
		return EventPage{}, fmt.Errorf("store: events limit %d, want positive", q.Limit)
	}
	// ReadOnly bypasses the store's immediate-write transaction mode. Bounds
	// and rows must share a snapshot without excluding concurrent writers.
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return EventPage{}, err
	}
	defer tx.Rollback()
	page := EventPage{ScannedThrough: q.After}
	var newest int64
	if err := tx.QueryRowContext(ctx, eventIDBoundsSQL).Scan(&page.Oldest, &newest); err != nil {
		return EventPage{}, err
	}
	query := `SELECT id, ts, type, user_id, sandbox_id, meta FROM events WHERE user_id = ? AND id > ?`
	args := []any{q.UserID, q.After}
	if q.TypePrefix != "" {
		// instr, not LIKE: the prefix is the caller's text, and "_" and "%" in it
		// must stay literal ("restart_" is a prefix, not a pattern).
		query += ` AND instr(type, ?) = 1`
		args = append(args, q.TypePrefix)
	}
	if q.SandboxID != "" {
		query += ` AND sandbox_id = ?`
		args = append(args, q.SandboxID)
	}
	query += ` ORDER BY id LIMIT ?`
	args = append(args, q.Limit)

	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return EventPage{}, err
	}
	defer rows.Close()
	for rows.Next() {
		e, err := scanEvent(rows)
		if err != nil {
			return EventPage{}, err
		}
		page.Events = append(page.Events, e)
	}
	if err := rows.Err(); err != nil {
		return EventPage{}, err
	}
	if len(page.Events) == q.Limit {
		page.ScannedThrough = page.Events[len(page.Events)-1].ID
	} else {
		// All matches through newest were exhausted, including any rejected
		// tail. Never move a caller's cursor backwards when it is ahead of us.
		page.ScannedThrough = max(q.After, newest)
	}
	return page, nil
}

// Separate min/max subqueries each seek the primary key; a combined aggregate
// makes SQLite scan the entire event table.
const eventIDBoundsSQL = `SELECT COALESCE((SELECT MIN(id) FROM events), 0), COALESCE((SELECT MAX(id) FROM events), 0)`

// EventIDBounds returns the smallest and largest event id stored, both 0 when
// there is none. Ids below oldest are gone: retention deletes from the old end,
// and the ids of deleted rows are not handed out again.
func (s *Store) EventIDBounds(ctx context.Context) (oldest, newest int64, err error) {
	err = s.db.QueryRowContext(ctx, eventIDBoundsSQL).Scan(&oldest, &newest)
	return oldest, newest, err
}

// NewestEventID checks the live edge with one primary-key seek, independent of
// the history size. It also observes commits made through other Store handles.
func (s *Store) NewestEventID(ctx context.Context) (int64, error) {
	var newest int64
	err := s.db.QueryRowContext(ctx, `SELECT COALESCE(MAX(id), 0) FROM events`).Scan(&newest)
	return newest, err
}

// CountEvents returns the count of events matching the filter.
func (s *Store) CountEvents(ctx context.Context, f EventFilter) (int, error) {
	f.CountOnly = true
	events, err := s.QueryEvents(ctx, f)
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
func (s *Store) QueryMetricsSnapshots(ctx context.Context, since time.Time, limit int) ([]MetricsSnapshot, error) {
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

	rows, err := s.db.QueryContext(ctx, query, since.UTC().Format("2006-01-02T15:04:05.000Z"))
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
func (s *Store) LatestMetricsSnapshot(ctx context.Context) (MetricsSnapshot, error) {
	var m MetricsSnapshot
	var tsStr string
	err := s.db.QueryRowContext(ctx, `SELECT ts,
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
func (s *Store) SumMetricsSnapshots(ctx context.Context, since time.Time) (MetricsSnapshot, error) {
	var m MetricsSnapshot
	err := s.db.QueryRowContext(ctx, `SELECT
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
func (s *Store) PurgeOldEvents(ctx context.Context, retention time.Duration) (int64, error) {
	cutoff := time.Now().Add(-retention).UTC().Format("2006-01-02T15:04:05.000Z")
	res, err := s.db.ExecContext(ctx, "DELETE FROM events WHERE ts < ?", cutoff)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// PurgeOldMetricsSnapshots deletes snapshots older than the given duration.
func (s *Store) PurgeOldMetricsSnapshots(ctx context.Context, retention time.Duration) (int64, error) {
	cutoff := time.Now().Add(-retention).UTC().Format("2006-01-02T15:04:05.000Z")
	res, err := s.db.ExecContext(ctx, "DELETE FROM metrics_snapshots WHERE ts < ?", cutoff)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}
