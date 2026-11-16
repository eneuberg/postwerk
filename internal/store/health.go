package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"modernc.org/sqlite"
)

// SQLite result codes (primary codes, the low byte of an extended code)
// that mean the database cannot be written: the disk is full, an I/O error
// (a file system remounted read-only after errors gives these too), a
// read-only file, a file that cannot be opened.
const (
	sqliteReadOnly  = 8
	sqliteIOErr     = 10
	sqliteFull      = 13
	sqliteCantOpen  = 14
	sqliteCodeBytes = 0xff
)

// storageError reports whether err says that the database cannot be
// written, as opposed to a constraint, a busy connection or a cancelled
// context.
func storageError(err error) bool {
	var se *sqlite.Error
	if !errors.As(err, &se) {
		return false
	}
	switch se.Code() & sqliteCodeBytes {
	case sqliteReadOnly, sqliteIOErr, sqliteFull, sqliteCantOpen:
		return true
	}
	return false
}

// noteWrite records the outcome of a write: a storage error is kept until a
// later write succeeds. It returns err unchanged.
func (s *Store) noteWrite(err error) error {
	switch {
	case err == nil:
		s.writeErr.Store(nil)
	case storageError(err):
		s.writeErr.Store(&err)
	}
	return err
}

// Writable returns the last write error that said the database cannot be
// written (disk full, I/O error, read-only), or nil when the last write
// that ended went through. /readyz uses it (D50).
func (s *Store) Writable() error {
	if p := s.writeErr.Load(); p != nil {
		return fmt.Errorf("store: last write failed: %w", *p)
	}
	return nil
}

// Probe writes one page and commits it, to find out whether the database
// can be written again after a storage error: it sets user_version to its
// own value. It updates Writable.
func (s *Store) Probe(ctx context.Context) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		var v int
		if err := tx.QueryRowContext(ctx, "PRAGMA user_version").Scan(&v); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, fmt.Sprintf("PRAGMA user_version = %d", v))
		return err
	})
}

// QueueStat is the queue of one target: the mails with a recipient queued or
// sending there, and when the oldest of them was received.
type QueueStat struct {
	Mails  int
	Oldest time.Time
}

// Queues returns the queue of every target that has one.
func (s *Store) Queues(ctx context.Context) (map[string]QueueStat, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT dr.target, COUNT(DISTINCT dr.mail_id), MIN(m.received_at) FROM delivery_recipients dr
		JOIN mails m ON m.id = dr.mail_id
		WHERE dr.status IN ('queued', 'sending') GROUP BY dr.target`)
	if err != nil {
		return nil, fmt.Errorf("store: queues: %w", err)
	}
	defer rows.Close()
	out := make(map[string]QueueStat)
	for rows.Next() {
		var target string
		var n int
		var oldest int64
		if err := rows.Scan(&target, &n, &oldest); err != nil {
			return nil, fmt.Errorf("store: queues: %w", err)
		}
		out[target] = QueueStat{Mails: n, Oldest: time.UnixMilli(oldest)}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: queues: %w", err)
	}
	return out, nil
}

// LastOf returns the newest event of one of kinds for target (empty: events
// without a target), and false if there is none. The signal checks use it
// to pick up their state after a restart, so a restart neither repeats nor
// loses a state change.
func (s *Store) LastOf(ctx context.Context, target string, kinds ...string) (Event, bool, error) {
	if len(kinds) == 0 {
		return Event{}, false, errors.New("store: last of: no kinds")
	}
	args := []any{nullStr(target)}
	for _, k := range kinds {
		args = append(args, k)
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT `+eventColumns+` FROM events WHERE target IS ? AND kind IN (`+placeholders(len(kinds))+`)
		ORDER BY seq DESC LIMIT 1`, args...)
	if err != nil {
		return Event{}, false, fmt.Errorf("store: last of: %w", err)
	}
	evs, err := scanEvents(rows)
	if err != nil {
		return Event{}, false, fmt.Errorf("store: last of: %w", err)
	}
	if len(evs) == 0 {
		return Event{}, false, nil
	}
	return evs[0], true, nil
}
