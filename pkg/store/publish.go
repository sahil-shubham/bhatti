package store

import (
	"errors"
	"time"
)

type PublishRule struct {
	ID        string    `json:"id"`
	SandboxID string    `json:"sandbox_id"`
	UserID    string    `json:"user_id"`
	Port      int       `json:"port"`
	Alias     string    `json:"alias"`
	CreatedAt time.Time `json:"created_at"`
}

// CreatePublishRule inserts a new publish rule. Two unique constraints can
// refuse it and both are ErrConflict: the alias is taken, or the sandbox
// already publishes the port. The message says which.
func (s *Store) CreatePublishRule(rule PublishRule) error {
	_, err := s.db.Exec(
		`INSERT INTO publish_rules (id, sandbox_id, user_id, port, alias)
		 VALUES (?, ?, ?, ?, ?)`,
		rule.ID, rule.SandboxID, rule.UserID, rule.Port, rule.Alias,
	)
	err = classify(err)
	if !errors.Is(err, ErrConflict) {
		return err
	}
	// The driver names the violated constraint only in its message text, so
	// ask the table which one holds.
	var aliasTaken, portTaken bool
	s.db.QueryRow(`SELECT EXISTS(SELECT 1 FROM publish_rules WHERE alias = ?),
		EXISTS(SELECT 1 FROM publish_rules WHERE sandbox_id = ? AND port = ?)`,
		rule.Alias, rule.SandboxID, rule.Port).Scan(&aliasTaken, &portTaken)
	switch {
	case aliasTaken:
		return wrap(ErrConflict, "alias %q is already taken", rule.Alias)
	case portTaken:
		return wrap(ErrConflict, "port %d is already published for this sandbox", rule.Port)
	}
	return err
}

// GetPublishRuleByAlias looks up a publish rule by its public alias.
func (s *Store) GetPublishRuleByAlias(alias string) (*PublishRule, error) {
	var r PublishRule
	err := s.db.QueryRow(
		`SELECT id, sandbox_id, user_id, port, alias, created_at
		 FROM publish_rules WHERE alias = ?`, alias,
	).Scan(&r.ID, &r.SandboxID, &r.UserID, &r.Port, &r.Alias, &r.CreatedAt)
	if err != nil {
		return nil, noRows(err, "no publish rule for alias %q", alias)
	}
	return &r, nil
}

// ListPublishRules returns all publish rules for a sandbox.
func (s *Store) ListPublishRules(sandboxID string) ([]PublishRule, error) {
	rows, err := s.db.Query(
		`SELECT id, sandbox_id, user_id, port, alias, created_at
		 FROM publish_rules WHERE sandbox_id = ?`, sandboxID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var rules []PublishRule
	for rows.Next() {
		var r PublishRule
		if err := rows.Scan(&r.ID, &r.SandboxID, &r.UserID, &r.Port, &r.Alias, &r.CreatedAt); err != nil {
			return nil, err
		}
		rules = append(rules, r)
	}
	return rules, rows.Err()
}

// ListUserPublishRules returns all publish rules for sandboxes owned by a user.
func (s *Store) ListUserPublishRules(userID string) ([]PublishRule, error) {
	rows, err := s.db.Query(
		`SELECT id, sandbox_id, user_id, port, alias, created_at
		 FROM publish_rules WHERE user_id = ?`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var rules []PublishRule
	for rows.Next() {
		var r PublishRule
		if err := rows.Scan(&r.ID, &r.SandboxID, &r.UserID, &r.Port, &r.Alias, &r.CreatedAt); err != nil {
			return nil, err
		}
		rules = append(rules, r)
	}
	return rules, rows.Err()
}

// DeletePublishRule removes a publish rule scoped to user + sandbox + port.
func (s *Store) DeletePublishRule(userID, sandboxID string, port int) error {
	res, err := s.db.Exec(
		`DELETE FROM publish_rules WHERE user_id = ? AND sandbox_id = ? AND port = ?`,
		userID, sandboxID, port,
	)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return wrap(ErrNotFound, "no publish rule for port %d on this sandbox", port)
	}
	return nil
}

// DeletePublishRulesForSandbox removes all publish rules for a sandbox.
func (s *Store) DeletePublishRulesForSandbox(sandboxID string) (int64, error) {
	res, err := s.db.Exec(`DELETE FROM publish_rules WHERE sandbox_id = ?`, sandboxID)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// CleanupOrphanedPublishRules removes rules for destroyed/unknown/missing sandboxes.
func (s *Store) CleanupOrphanedPublishRules() (int64, error) {
	res, err := s.db.Exec(`
		DELETE FROM publish_rules WHERE sandbox_id NOT IN (
			SELECT id FROM sandboxes WHERE status NOT IN ('destroyed', 'unknown')
		)`)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// --- Volume Backups ---
