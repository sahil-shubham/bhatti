package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func userVersion(t *testing.T, db *sql.DB) int {
	t.Helper()
	var v int
	if err := db.QueryRowContext(t.Context(), `PRAGMA user_version`).Scan(&v); err != nil {
		t.Fatal(err)
	}
	return v
}

func setUserVersion(t *testing.T, db *sql.DB, v int) {
	t.Helper()
	if _, err := db.ExecContext(t.Context(), fmt.Sprintf(`PRAGMA user_version = %d`, v)); err != nil {
		t.Fatal(err)
	}
}

// baseline is the steps written to run over any shape a database from before
// the ladder can have, which the steps after it are not (see migrations).
var baseline = migrations[:3]

// reopenFromBeforeTheLadder runs the baseline again over the database behind
// s, as a database from before the ladder gets it: rewound to version 0, as
// every older release left it, and opened with the baseline alone, because
// the later steps expect a schema without what they add and this one has it.
// It closes s, and returns the reopened store with the version s had, since
// the steps after the baseline did run on this database.
func reopenFromBeforeTheLadder(t *testing.T, s *Store, path string) *Store {
	t.Helper()
	was := userVersion(t, s.db)
	setUserVersion(t, s.db, 0)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := open(t.Context(), path, baseline)
	if err != nil {
		t.Fatal(err)
	}
	setUserVersion(t, reopened.db, was)
	return reopened
}

// rawOpen opens the database file at path without going through New, for
// reading what New left or failed to leave.
func rawOpen(t *testing.T, path string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func tableNames(t *testing.T, db *sql.DB) []string {
	t.Helper()
	rows, err := db.QueryContext(t.Context(), `SELECT name FROM sqlite_master WHERE type = 'table' AND name NOT LIKE 'sqlite\_%' ESCAPE '\'`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var names []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatal(err)
		}
		names = append(names, name)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return names
}

func rowCounts(t *testing.T, db *sql.DB) map[string]int {
	t.Helper()
	counts := map[string]int{}
	for _, table := range tableNames(t, db) {
		var n int
		if err := db.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM "`+table+`"`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		counts[table] = n
	}
	return counts
}

// schemaOf describes the tables and indexes of db as lines that don't depend
// on the order its columns came in (ALTER TABLE appends one where a CREATE
// puts it in the middle) or on the names SQLite gives the indexes behind
// UNIQUE and PRIMARY KEY constraints.
func schemaOf(t *testing.T, db *sql.DB) []string {
	t.Helper()
	var lines []string
	for _, table := range tableNames(t, db) {
		lines = append(lines, "table "+table)

		cols, err := db.QueryContext(t.Context(), `SELECT name, type, "notnull", dflt_value, pk FROM pragma_table_info(?)`, table)
		if err != nil {
			t.Fatal(err)
		}
		for cols.Next() {
			var name, typ string
			var notNull, pk int
			var dflt sql.NullString
			if err := cols.Scan(&name, &typ, &notNull, &dflt, &pk); err != nil {
				t.Fatal(err)
			}
			def := "none"
			if dflt.Valid {
				def = dflt.String
			}
			lines = append(lines, fmt.Sprintf("%s: column %s %s notnull=%d default=%s pk=%d", table, name, typ, notNull, def, pk))
		}
		if err := cols.Err(); err != nil {
			t.Fatal(err)
		}
		cols.Close()

		type index struct {
			name, origin    string
			unique, partial int
		}
		var indexes []index
		list, err := db.QueryContext(t.Context(), `SELECT name, "unique", origin, partial FROM pragma_index_list(?)`, table)
		if err != nil {
			t.Fatal(err)
		}
		for list.Next() {
			var ix index
			if err := list.Scan(&ix.name, &ix.unique, &ix.origin, &ix.partial); err != nil {
				t.Fatal(err)
			}
			indexes = append(indexes, ix)
		}
		if err := list.Err(); err != nil {
			t.Fatal(err)
		}
		list.Close()

		for _, ix := range indexes {
			var keys []string
			info, err := db.QueryContext(t.Context(), `SELECT COALESCE(name, '<expr>'), "desc" FROM pragma_index_xinfo(?) WHERE key = 1 ORDER BY seqno`, ix.name)
			if err != nil {
				t.Fatal(err)
			}
			for info.Next() {
				var name string
				var desc int
				if err := info.Scan(&name, &desc); err != nil {
					t.Fatal(err)
				}
				if desc == 1 {
					name += " DESC"
				}
				keys = append(keys, name)
			}
			if err := info.Err(); err != nil {
				t.Fatal(err)
			}
			info.Close()

			line := fmt.Sprintf("%s: index unique=%d origin=%s partial=%d on (%s)", table, ix.unique, ix.origin, ix.partial, strings.Join(keys, ", "))
			if ix.origin == "c" {
				line += " name=" + ix.name
			}
			if ix.partial == 1 {
				var stmt string
				if err := db.QueryRowContext(t.Context(), `SELECT sql FROM sqlite_master WHERE name = ?`, ix.name).Scan(&stmt); err != nil {
					t.Fatal(err)
				}
				_, where, _ := strings.Cut(strings.Join(strings.Fields(stmt), " "), " WHERE ")
				line += " where " + where
			}
			lines = append(lines, line)
		}
	}
	slices.Sort(lines)
	return lines
}

// diffLines lists what is in only one of want and got.
func diffLines(want, got []string) string {
	var b strings.Builder
	for _, l := range want {
		if !slices.Contains(got, l) {
			fmt.Fprintf(&b, "- %s\n", l)
		}
	}
	for _, l := range got {
		if !slices.Contains(want, l) {
			fmt.Fprintf(&b, "+ %s\n", l)
		}
	}
	return b.String()
}

// replayDump writes the database that dump describes to a new file, at
// version 0 as every release before the ladder left it, and returns its path
// and the rows it holds per table.
func replayDump(t *testing.T, dump string) (string, map[string]int) {
	t.Helper()
	text, err := os.ReadFile(dump)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "state.db")
	db := rawOpen(t, path)
	if _, err := db.ExecContext(t.Context(), string(text)); err != nil {
		t.Fatalf("replay %s: %v", dump, err)
	}
	counts := rowCounts(t, db)
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	return path, counts
}

// TestDatabasesFromEveryReleaseConverge: New on a database written by any
// earlier release leaves the schema a new database has, at the ladder's last
// version, with every row it had.
//
// The databases in testdata/olddb were each written by one release's own
// store.New, seeded with a few rows per table, and dumped as SQL. Every
// release before the ladder left user_version at 0. A dump stands for each
// distinct schema they left:
//
//	v1.0.0       v1.0.0-rc1 to v1.2.1
//	v1.3.0       v1.3.0
//	v1.4.0       v1.4.0 to v1.9.1
//	v1.11.12     v1.10.0 to v1.11.12
//	v2.0.0-rc.4  v2.0.0-rc.4 to v2.3.2
//	v2.4.0       v2.4.0-rc.1 to v2.5.0
//	v2.5.1       v2.5.1 to v2.5.6
//	v2.5.7       v2.5.7 and v2.5.8: the last schema before the ladder
//
// and two commits from before v1.0.0, whose secrets table still had its first
// key: pre-v1-secrets-3b26367 (no user_id, and no users table) and
// pre-v1-secrets-f404593 (a user_id outside the key).
func TestDatabasesFromEveryReleaseConverge(t *testing.T) {
	fresh := testStore(t)
	want := schemaOf(t, fresh.db)
	if v := userVersion(t, fresh.db); v != len(migrations) {
		t.Fatalf("a new database is at version %d, want %d", v, len(migrations))
	}

	dumps, err := filepath.Glob("testdata/olddb/*.sql")
	if err != nil {
		t.Fatal(err)
	}
	if len(dumps) == 0 {
		t.Fatal("no databases in testdata/olddb to upgrade")
	}
	for _, dump := range dumps {
		t.Run(strings.TrimSuffix(filepath.Base(dump), ".sql"), func(t *testing.T) {
			path, before := replayDump(t, dump)
			var rows int
			for _, n := range before {
				rows += n
			}
			if rows == 0 {
				t.Fatal("the dump holds no rows, so nothing shows they survive")
			}

			s, err := New(t.Context(), path)
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()

			if v := userVersion(t, s.db); v != len(migrations) {
				t.Errorf("version after New = %d, want %d", v, len(migrations))
			}
			if diff := diffLines(want, schemaOf(t, s.db)); diff != "" {
				t.Errorf("schema differs from a new database's (- only in a new one, + only in this one):\n%s", diff)
			}
			after := rowCounts(t, s.db)
			for table, n := range before {
				if after[table] != n {
					t.Errorf("table %s: %d rows before New, %d after", table, n, after[table])
				}
			}
		})
	}
}

// TestAnyMissingColumnIsAdded: an old database lacks whichever columns the
// releases after it added, and a release may have added any of them to a
// database that has the rest. Each column the baseline adds, missing on its
// own from a database that has everything else, comes back as a new database
// has it.
func TestAnyMissingColumnIsAdded(t *testing.T) {
	want := schemaOf(t, testStore(t).db)
	path := filepath.Join(t.TempDir(), "state.db")
	for _, c := range slices.Concat(originalColumns, laterColumns) {
		// The secrets table is rebuilt by the next step, and the rebuild
		// defines its columns; a v2 table without one is not a shape any
		// release left.
		if c.table == "secrets" {
			continue
		}
		t.Run(c.table+"."+c.column, func(t *testing.T) {
			s, err := New(t.Context(), path)
			if err != nil {
				t.Fatal(err)
			}
			// An index on the column blocks DROP COLUMN; the baseline makes it again.
			rows, err := s.db.QueryContext(t.Context(),
				`SELECT name FROM sqlite_master WHERE type = 'index' AND tbl_name = ? AND sql LIKE '%' || ? || '%'`, c.table, c.column)
			if err != nil {
				t.Fatal(err)
			}
			var indexes []string
			for rows.Next() {
				var name string
				if err := rows.Scan(&name); err != nil {
					t.Fatal(err)
				}
				indexes = append(indexes, name)
			}
			rows.Close()
			for _, name := range indexes {
				if _, err := s.db.Exec(`DROP INDEX ` + name); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := s.db.Exec(`ALTER TABLE ` + c.table + ` DROP COLUMN ` + c.column); err != nil {
				t.Fatal(err)
			}
			s = reopenFromBeforeTheLadder(t, s, path)
			defer s.Close()
			if diff := diffLines(want, schemaOf(t, s.db)); diff != "" {
				t.Errorf("schema differs from a new database's (- only in a new one, + only in this one):\n%s", diff)
			}
		})
	}
}

// ddlStep is a migration that creates one table, then returns fail, so that
// what the rollback must undo is already in the transaction.
func ddlStep(name, table string, ran *[]string, fail error) migration {
	return migration{name, func(ctx context.Context, tx *sql.Tx) error {
		*ran = append(*ran, name)
		if _, err := tx.ExecContext(ctx, `CREATE TABLE `+table+` (x INTEGER)`); err != nil {
			return err
		}
		return fail
	}}
}

// TestFailedMigrationNamesItsStep: a step that fails fails the open, and the
// error says which step (by number and name) and why. The database keeps the
// version of the last step that succeeded, nothing of the failed one, and the
// next open resumes there, running no step twice.
func TestFailedMigrationNamesItsStep(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	boom := errors.New("boom")
	var ran []string
	ladder := []migration{
		ddlStep("make a", "a", &ran, nil),
		ddlStep("make b", "b", &ran, nil),
		ddlStep("make c", "c", &ran, boom),
		ddlStep("make d", "d", &ran, nil),
	}

	s, err := open(t.Context(), path, ladder)
	if err == nil {
		s.Close()
		t.Fatal("opened a database whose third migration failed")
	}
	if !errors.Is(err, boom) {
		t.Errorf("the step's own error is not in the chain: %v", err)
	}
	if msg := err.Error(); !strings.Contains(msg, "migration 3") || !strings.Contains(msg, "make c") {
		t.Errorf("error does not name migration 3 (make c): %v", err)
	}
	if want := []string{"make a", "make b", "make c"}; !slices.Equal(ran, want) {
		t.Errorf("steps run = %v, want %v: a failure must stop the ladder", ran, want)
	}

	raw := rawOpen(t, path)
	if v := userVersion(t, raw); v != 2 {
		t.Errorf("version after the failure = %d, want 2, the last step that succeeded", v)
	}
	if got, want := tableNames(t, raw), []string{"a", "b"}; !slices.Equal(got, want) {
		t.Errorf("tables after the failure = %v, want %v: the failed step must leave nothing behind", got, want)
	}

	ran = nil
	ladder[2] = ddlStep("make c", "c", &ran, nil)
	s, err = open(t.Context(), path, ladder)
	if err != nil {
		t.Fatalf("reopen with the step fixed: %v", err)
	}
	defer s.Close()
	if want := []string{"make c", "make d"}; !slices.Equal(ran, want) {
		t.Errorf("steps run on resuming = %v, want %v", ran, want)
	}
	if v := userVersion(t, s.db); v != 4 {
		t.Errorf("version after resuming = %d, want 4", v)
	}
}

// TestNewRefusesADatabaseFromANewerBuild: a database a newer build has
// migrated past this one's last step was changed in ways this build can't see,
// so it is refused untouched rather than run on blind.
func TestNewRefusesADatabaseFromANewerBuild(t *testing.T) {
	t.Run("New", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "state.db")
		s, err := New(t.Context(), path)
		if err != nil {
			t.Fatal(err)
		}
		newer := len(migrations) + 1
		setUserVersion(t, s.db, newer)
		if err := s.Close(); err != nil {
			t.Fatal(err)
		}

		opened, err := New(t.Context(), path)
		if err == nil {
			opened.Close()
			t.Fatal("opened a database from a newer build")
		}
		if !errors.Is(err, errSchemaNewer) {
			t.Errorf("refused for another reason: %v", err)
		}
		if v := userVersion(t, rawOpen(t, path)); v != newer {
			t.Errorf("version after the refusal = %d, want it left at %d", v, newer)
		}
	})

	t.Run("no step runs", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "state.db")
		var ran []string
		ladder := []migration{
			ddlStep("make a", "a", &ran, nil),
			ddlStep("make b", "b", &ran, nil),
		}
		s, err := open(t.Context(), path, ladder)
		if err != nil {
			t.Fatal(err)
		}
		setUserVersion(t, s.db, 3) // a newer build's third step
		if err := s.Close(); err != nil {
			t.Fatal(err)
		}

		ran = nil
		opened, err := open(t.Context(), path, ladder)
		if err == nil {
			opened.Close()
			t.Fatal("opened a database from a newer build")
		}
		if !errors.Is(err, errSchemaNewer) {
			t.Errorf("refused for another reason: %v", err)
		}
		if len(ran) != 0 {
			t.Errorf("steps run on a database from a newer build: %v", ran)
		}
	})

	t.Run("negative", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "state.db")
		s, err := New(t.Context(), path)
		if err != nil {
			t.Fatal(err)
		}
		setUserVersion(t, s.db, -1)
		if err := s.Close(); err != nil {
			t.Fatal(err)
		}
		opened, err := New(t.Context(), path)
		if err == nil {
			opened.Close()
			t.Fatal("opened a database whose version is negative")
		}
	})
}

// TestConcurrentOpenersRunEachStepOnce: two processes can open one new
// database at once (the daemon booting while an admin command runs). The one
// that waits for the other's write lock finds its steps already run, because a
// step may assume it runs exactly once, on the schema the step before it left.
func TestConcurrentOpenersRunEachStepOnce(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	// Put the database in WAL mode first: switching a brand-new file takes
	// a lock that busy_timeout, set after it, does not wait for.
	first, err := open(t.Context(), path, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}

	var runs [3]atomic.Int32
	// A plain CREATE TABLE, so a step run twice fails loudly as well as counting.
	step := func(i int, delay time.Duration) migration {
		return migration{fmt.Sprintf("step %d", i), func(ctx context.Context, tx *sql.Tx) error {
			runs[i].Add(1)
			time.Sleep(delay) // holds the write lock while the other opener queues for it
			_, err := tx.ExecContext(ctx, fmt.Sprintf(`CREATE TABLE t%d (x INTEGER)`, i))
			return err
		}}
	}
	ladder := []migration{step(0, 200*time.Millisecond), step(1, 0), step(2, 0)}

	const openers = 3
	errs := make([]error, openers)
	var wg sync.WaitGroup
	for i := range openers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s, err := open(t.Context(), path, ladder)
			if err == nil {
				err = s.Close()
			}
			errs[i] = err
		}()
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Errorf("opener %d: %v", i, err)
		}
	}
	for i := range runs {
		if n := runs[i].Load(); n != 1 {
			t.Errorf("step %d ran %d times, want once", i, n)
		}
	}
	if v := userVersion(t, rawOpen(t, path)); v != len(ladder) {
		t.Errorf("version = %d, want %d", v, len(ladder))
	}
}

// TestUpToDateDatabaseOpensBesideAWriter: an admin command opens the database
// while the daemon is mid-write. A database already at the last version has no
// step to run, so opening it must not queue for the write lock; BEGIN
// IMMEDIATE would wait out busy_timeout and then fail the command.
func TestUpToDateDatabaseOpensBesideAWriter(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	s, err := New(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	writer, err := rawOpen(t, path).Conn(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	if _, err := writer.ExecContext(t.Context(), `BEGIN IMMEDIATE`); err != nil {
		t.Fatal(err)
	}
	defer writer.ExecContext(t.Context(), `ROLLBACK`)

	opened, err := New(t.Context(), path)
	if err != nil {
		t.Fatalf("opening beside a writer: %v", err)
	}
	opened.Close()
}

// TestEventsCursorIndex: a follower reads one user's events after a cursor,
// which wants an index on (user_id, id) to seek by. A new database has it,
// and so does one that last ran only the baseline: the step after the
// baseline runs on that database alone.
func TestEventsCursorIndex(t *testing.T) {
	const want = "events: index unique=0 origin=c partial=0 on (user_id, id) name=idx_events_user_id"

	t.Run("new database", func(t *testing.T) {
		if !slices.Contains(schemaOf(t, testStore(t).db), want) {
			t.Errorf("no index on events (user_id, id)")
		}
	})

	t.Run("database at the baseline", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "state.db")
		s, err := open(t.Context(), path, baseline)
		if err != nil {
			t.Fatal(err)
		}
		if v := userVersion(t, s.db); v != len(baseline) {
			t.Fatalf("version after the baseline = %d, want %d", v, len(baseline))
		}
		if slices.Contains(schemaOf(t, s.db), want) {
			t.Fatal("the baseline already has the index, so the step after it has nothing to show")
		}
		if err := s.Close(); err != nil {
			t.Fatal(err)
		}

		s, err = New(t.Context(), path)
		if err != nil {
			t.Fatal(err)
		}
		defer s.Close()
		if v := userVersion(t, s.db); v != len(migrations) {
			t.Errorf("version = %d, want %d", v, len(migrations))
		}
		if !slices.Contains(schemaOf(t, s.db), want) {
			t.Errorf("no index on events (user_id, id) after the step that makes it")
		}
	})
}
