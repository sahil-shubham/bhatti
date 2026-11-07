package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strings"
	"sync"

	_ "modernc.org/sqlite"
)

// Store wraps SQLite operations.
type Store struct {
	db *sql.DB

	volumeGateMu sync.Mutex
	volumeGates  map[string]*volumeAttachmentGate
}

type volumeAttachmentGate struct {
	mu   sync.Mutex
	refs int
}

// New opens (or creates) the SQLite database and brings its schema up to date
// by running the migrations it lacks. A database that a newer build has
// migrated further than this one knows is refused.
func New(ctx context.Context, dbPath string) (*Store, error) {
	return open(ctx, dbPath, migrations)
}

// open is New with the ladder to climb passed in, so a test can climb another.
func open(ctx context.Context, dbPath string, ladder []migration) (*Store, error) {
	if err := restrictDBFiles(dbPath); err != nil {
		return nil, err
	}
	// _txlock=immediate takes the write lock at BEGIN, which applyNext relies
	// on to keep two openers from running one migration twice.
	db, err := sql.Open("sqlite", dbPath+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_txlock=immediate")
	if err != nil {
		return nil, fmt.Errorf("store: open db: %w", err)
	}
	// Four connections bound concurrent readers while allowing writes when
	// callers keep query rows open; one connection would block nested queries.
	db.SetMaxOpenConns(4)
	ok := false
	defer func() {
		if !ok {
			db.Close()
		}
	}()
	var journalMode string
	if err := db.QueryRowContext(ctx, `PRAGMA journal_mode`).Scan(&journalMode); err != nil {
		return nil, fmt.Errorf("store: read journal mode: %w", err)
	}
	if !strings.EqualFold(journalMode, "wal") {
		return nil, fmt.Errorf("store: journal mode %q, want wal", journalMode)
	}
	if err := migrate(ctx, db, ladder); err != nil {
		return nil, err
	}
	ok = true
	return &Store{db: db}, nil
}

// restrictDBFiles makes the database owner-only before SQLite opens it. It
// holds API key hashes, sealed secrets and guest agent tokens. SQLite creates
// the -wal and -shm files with the database file's mode, so creating it 0600
// covers them; files left by an older version are tightened here.
func restrictDBFiles(dbPath string) error {
	f, err := os.OpenFile(dbPath, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return fmt.Errorf("store: open db: %w", err)
	}
	f.Close()
	for _, p := range []string{dbPath, dbPath + "-wal", dbPath + "-shm"} {
		if err := os.Chmod(p, 0o600); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("store: restrict %s: %w", p, err)
		}
	}
	return nil
}

// Close closes the database.
func (s *Store) Close() error { return s.db.Close() }

// --- Users ---

// CreateUser creates a new API user.

// SetSecret creates or updates an encrypted secret for a user.

// CreateImage inserts a new image record.

// CreateSnapshot inserts a new snapshot record.
// CreateTask inserts a new task record.
// CreateVolumeBackup records a new backup.
