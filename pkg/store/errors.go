package store

import (
	"database/sql"
	"errors"
	"fmt"

	"modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"
)

// The store's error vocabulary. Callers branch on these with errors.Is and
// never on message text: the driver's wording is not a contract, and a name a
// user chose can contain any word a substring match looks for (a volume named
// "attachment" once turned a "not found" into a 409).
var (
	// ErrNotFound: no such row. Lookups are scoped to the owner, so another
	// user's row and an absent one are the same answer, and must stay so.
	ErrNotFound = errors.New("not found")

	// ErrConflict: a unique constraint refused the write because the name (or
	// key) is already taken.
	ErrConflict = errors.New("already exists")

	// ErrInUse: a delete refused because other rows still depend on the one
	// named (a sandbox using a legacy volume, a user who still owns sandboxes
	// or secrets).
	ErrInUse = errors.New("in use")

	// ErrVolumeAttached: a persistent volume's attachments forbid the
	// operation: it can't be deleted while attached, and an attachment that
	// would break its single-writer rule is refused.
	ErrVolumeAttached = errors.New("volume is attached")
)

// kindError reads as msg and matches kind under errors.Is. Messages are
// worded for the person who will read them and kept apart from kind so that
// rewording one never changes which branch a caller takes.
type kindError struct {
	kind  error
	msg   string // empty: cause's text, or kind's when there is no cause
	cause error  // the driver error this came from, for errors.As and logs
}

func (e *kindError) Error() string {
	switch {
	case e.msg != "":
		return e.msg
	case e.cause != nil:
		return e.cause.Error()
	}
	return e.kind.Error()
}

func (e *kindError) Unwrap() []error {
	if e.cause == nil {
		return []error{e.kind}
	}
	return []error{e.kind, e.cause}
}

// wrap is an error that reads as the formatted text and matches kind.
func wrap(kind error, format string, args ...any) error {
	return &kindError{kind: kind, msg: fmt.Sprintf(format, args...)}
}

// classify translates what the driver returns into the store's vocabulary,
// and is the only place that reads a driver error: SQLITE_CONSTRAINT_UNIQUE
// and SQLITE_CONSTRAINT_PRIMARYKEY (both mean the key is taken) become
// ErrConflict, and the empty result of a single-row query becomes ErrNotFound.
// Anything else comes back unchanged. The driver error stays in the chain, so
// errors.As still finds the *sqlite.Error behind an ErrConflict.
//
// Extended result codes are on for every connection the driver opens, so the
// codes below are the specific ones.
func classify(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, sql.ErrNoRows) {
		return &kindError{kind: ErrNotFound}
	}
	var se *sqlite.Error
	if errors.As(err, &se) {
		switch se.Code() {
		case sqlite3.SQLITE_CONSTRAINT_UNIQUE, sqlite3.SQLITE_CONSTRAINT_PRIMARYKEY:
			return &kindError{kind: ErrConflict, cause: err}
		}
	}
	return err
}

// noRows is err, with the empty result of a single-row query turned into
// ErrNotFound worded as the caller says; any other error passes through, so a
// failed query is never reported as an absent row.
func noRows(err error, format string, args ...any) error {
	if errors.Is(err, sql.ErrNoRows) {
		return wrap(ErrNotFound, format, args...)
	}
	return err
}
