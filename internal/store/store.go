// Package store keeps mails, deliveries and events in one SQLite file
// (S1–S11). It is the only package that speaks SQL.
//
// All access goes through one connection, so every read and write is
// serialized and SQLITE_BUSY cannot happen inside the process. Every state
// change is written together with its event in one transaction (S4). Free
// texts are passed through the redact package before they are stored (S5).
//
// The store does not encrypt or hash anything itself: raw messages arrive
// sealed (internal/seal), and the retention job passes in the function that
// hashes addresses (internal/addrhash). So the store needs no keys.
package store

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"slices"
	"strconv"
	"strings"
	"time"

	_ "modernc.org/sqlite" // registers the "sqlite" driver

	"github.com/eneuberg/postwerk/internal/redact"
)

//go:embed migrations/*.sql
var migrationFS embed.FS

// Status of a delivery.
type Status string

// Delivery statuses.
const (
	Queued    Status = "queued"
	Sending   Status = "sending"
	HandedOff Status = "handed_off"
	Failed    Status = "failed"
	Unknown   Status = "unknown"
	Skipped   Status = "skipped"
)

// Level of an event.
type Level string

// Event levels.
const (
	Info  Level = "info"
	Warn  Level = "warn"
	Error Level = "error"
)

// Event kinds.
const (
	KindMailAccepted       = "mail.accepted"
	KindDeliveryAttempt    = "delivery.attempt"
	KindDeliveryRetry      = "delivery.retry"
	KindDeliveryHandedOff  = "delivery.handed_off"
	KindDeliveryFailed     = "delivery.failed"
	KindDeliveryUnknown    = "delivery.unknown"
	KindDeliverySkipped    = "delivery.skipped"
	KindDeliveryNotAllowed = "delivery.not_allowed"
	// KindDeliveryUnsupported: the mail needs an SMTP extension
	// (SMTPUTF8, 8BITMIME) that the target does not offer; nothing was sent.
	KindDeliveryUnsupported = "delivery.unsupported"
	KindTargetAuthFailed    = "target.auth_failed"
	KindTargetUnreachable   = "target.unreachable"
	// KindRawExpired: the stored message was removed by retention while the
	// delivery still needed it; the delivery is failed and cannot be resent.
	KindRawExpired = "retention.raw_expired"
	// KindRetentionPurged: one retention run removed or replaced data; the
	// text holds counts only.
	KindRetentionPurged = "retention.purged"
	// KindDecryptFailed: the stored message could not be decrypted; the
	// delivery is failed and nothing was sent.
	KindDecryptFailed = "store.decrypt_failed"
)

// ErrNotSending is returned when a delivery that should be in "sending" is
// not, which means two writers disagree about it.
var ErrNotSending = errors.New("store: delivery is not in sending state")

// Store is an open Postwerk database.
type Store struct {
	db   *sql.DB
	opts Options
}

// Options for Open.
type Options struct {
	// OnEvent, if set, is called after every commit that added events. It
	// must not block.
	OnEvent func()
	// PurgeRawOnHandoff removes the stored message in the same transaction
	// that makes the last delivery of a mail handed_off or skipped
	// (POSTWERK_RAW_KEEP_AFTER_HANDOFF=0).
	PurgeRawOnHandoff bool
	// InsecureDelete turns SQLite's secure_delete off
	// (POSTWERK_SECURE_DELETE=off). By default deleted content is
	// overwritten with zeros.
	InsecureDelete bool
}

// Open opens or creates the database at path, applies the pragmas, checks
// that they took effect and runs pending migrations.
func Open(ctx context.Context, path string, opts Options) (*Store, error) {
	if path == "" || strings.ContainsAny(path, "?#") || strings.HasPrefix(path, "file:") {
		return nil, errors.New("store: invalid database path (no '?', '#' or file: prefix)")
	}
	// Create the file with owner-only permissions; SQLite gives its WAL
	// and shared-memory files the same mode.
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, fmt.Errorf("store: create database file: %w", err)
	}
	_ = f.Close()

	secureDelete := "1"
	if opts.InsecureDelete {
		secureDelete = "0"
	}
	dsn := path + "?_pragma=journal_mode(WAL)" +
		"&_pragma=synchronous(FULL)" +
		"&_pragma=foreign_keys(1)" +
		"&_pragma=busy_timeout(5000)" +
		"&_pragma=secure_delete(" + secureDelete + ")" +
		"&_pragma=temp_store(MEMORY)" +
		"&_txlock=immediate"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("store: open: %w", err)
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	db.SetConnMaxLifetime(0)
	db.SetConnMaxIdleTime(0)

	s := &Store{db: db, opts: opts}
	if err := s.checkPragmas(ctx); err != nil {
		db.Close()
		return nil, err
	}
	if err := s.migrate(ctx); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

// Close closes the database.
func (s *Store) Close() error { return s.db.Close() }

// Ping checks that the database answers.
func (s *Store) Ping(ctx context.Context) error {
	var one int
	if err := s.db.QueryRowContext(ctx, "SELECT 1").Scan(&one); err != nil {
		return fmt.Errorf("store: ping: %w", err)
	}
	return nil
}

func (s *Store) checkPragmas(ctx context.Context) error {
	secureDelete := "1"
	if s.opts.InsecureDelete {
		secureDelete = "0"
	}
	want := map[string]string{
		"journal_mode":  "wal",
		"synchronous":   "2", // FULL
		"foreign_keys":  "1",
		"secure_delete": secureDelete,
	}
	for name, w := range want {
		var got string
		if err := s.db.QueryRowContext(ctx, "PRAGMA "+name).Scan(&got); err != nil {
			return fmt.Errorf("store: read pragma %s: %w", name, err)
		}
		if strings.ToLower(got) != w {
			return fmt.Errorf("store: pragma %s is %q, want %q (is the file on a local disk?)", name, got, w)
		}
	}
	return nil
}

// migrate applies every embedded migration newer than the database's
// user_version, each in its own transaction.
func (s *Store) migrate(ctx context.Context) error {
	files, err := fs.Glob(migrationFS, "migrations/*.sql")
	if err != nil {
		return fmt.Errorf("store: list migrations: %w", err)
	}
	var current int
	if err := s.db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&current); err != nil {
		return fmt.Errorf("store: read schema version: %w", err)
	}
	if current > len(files) {
		return fmt.Errorf("store: database schema version %d is newer than this Postwerk (knows %d)", current, len(files))
	}
	for i, name := range files {
		version := i + 1
		if n, err := strconv.Atoi(strings.SplitN(path.Base(name), "_", 2)[0]); err != nil || n != version {
			return fmt.Errorf("store: migration %s is out of sequence", name)
		}
		if version <= current {
			continue
		}
		body, err := migrationFS.ReadFile(name)
		if err != nil {
			return fmt.Errorf("store: read migration %s: %w", name, err)
		}
		err = s.tx(ctx, func(tx *sql.Tx) error {
			if _, err := tx.ExecContext(ctx, string(body)); err != nil {
				return err
			}
			_, err := tx.ExecContext(ctx, fmt.Sprintf("PRAGMA user_version = %d", version))
			return err
		})
		if err != nil {
			return fmt.Errorf("store: migration %s: %w", name, err)
		}
	}
	return nil
}

// tx runs fn in a transaction and commits it if fn returns nil.
func (s *Store) tx(ctx context.Context, fn func(*sql.Tx) error) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		_ = tx.Rollback()
		return err
	}
	return tx.Commit()
}

// eventTx runs fn in a transaction and signals OnEvent after the commit.
func (s *Store) eventTx(ctx context.Context, fn func(*sql.Tx) error) error {
	if err := s.tx(ctx, fn); err != nil {
		return err
	}
	if s.opts.OnEvent != nil {
		s.opts.OnEvent()
	}
	return nil
}

// Event is one row of the event log.
type Event struct {
	Seq     int64
	Time    time.Time
	MailID  string
	Target  string
	Kind    string
	Level   Level
	Attempt int
	Code    int
	Text    string
	Sender  string
}

// insertEvent appends e. The sender is taken from the mail row; the text is
// redacted.
func insertEvent(ctx context.Context, tx *sql.Tx, e Event) error {
	_, err := tx.ExecContext(ctx, `
		INSERT INTO events (time, mail_id, target, kind, level, attempt, code, text, sender)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, (SELECT sender FROM mails WHERE id = ?))`,
		ms(e.Time), nullStr(e.MailID), nullStr(e.Target), e.Kind, string(e.Level),
		nullInt(e.Attempt), nullInt(e.Code), nullStr(redact.Text(e.Text)), e.MailID)
	if err != nil {
		return fmt.Errorf("insert event %s: %w", e.Kind, err)
	}
	return nil
}

// NewMail is an accepted mail with its deliveries.
type NewMail struct {
	ID           string
	ReceivedAt   time.Time
	Sender       string
	EnvelopeFrom string
	Recipients   []string
	MessageID    string
	Ref          string
	// Raw is the sealed message (internal/seal); Size is the length of the
	// message before sealing.
	Raw  []byte
	Size int
	// Params are the MAIL parameters the sender declared.
	Params     Params
	Deliveries []NewDelivery
}

// Body values of the MAIL BODY= parameter that Postwerk accepts.
const (
	Body7Bit     = "7BIT"
	Body8BitMIME = "8BITMIME"
)

// Params are the MAIL parameters a sender declared (RFC 6152, RFC 6531,
// RFC 1870). They are stored as given, so delivery can pass them on.
type Params struct {
	// Body is Body7Bit, Body8BitMIME or empty when BODY= was not given.
	Body string
	// SMTPUTF8 is set when the sender gave the SMTPUTF8 parameter.
	SMTPUTF8 bool
	// DeclaredSize is the SIZE= value, 0 when not given.
	DeclaredSize int64
}

// NewDelivery is the initial state of one delivery.
type NewDelivery struct {
	Target string
	// Status is Queued or Skipped.
	Status Status
	// For Skipped: the event kind, level and text that explain why.
	Kind  string
	Level Level
	Text  string
}

// Accept stores a mail, its recipients, its deliveries and their events in one
// transaction (I6, S4). Queued deliveries are due at once. If no delivery is
// queued and PurgeRawOnHandoff is set, the message is not stored at all.
func (s *Store) Accept(ctx context.Context, m NewMail) error {
	at := ms(m.ReceivedAt)
	raw, purgedAt := any(m.Raw), any(nil)
	if s.opts.PurgeRawOnHandoff && !slices.ContainsFunc(m.Deliveries, func(d NewDelivery) bool { return d.Status == Queued }) {
		raw, purgedAt = nil, at
	}
	switch m.Params.Body {
	case "", Body7Bit, Body8BitMIME:
	default:
		return fmt.Errorf("store: accept %s: invalid BODY value", m.ID)
	}
	err := s.eventTx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `
			INSERT INTO mails (id, received_at, sender, envelope_from, message_id, ref, size, raw, raw_purged_at,
				body, smtputf8, declared_size)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			m.ID, at, m.Sender, m.EnvelopeFrom, nullStr(m.MessageID), nullStr(m.Ref), m.Size, raw, purgedAt,
			nullStr(m.Params.Body), m.Params.SMTPUTF8, nullInt64(m.Params.DeclaredSize))
		if err != nil {
			return fmt.Errorf("insert mail: %w", err)
		}
		for i, r := range m.Recipients {
			if _, err := tx.ExecContext(ctx,
				`INSERT INTO recipients (mail_id, position, address) VALUES (?, ?, ?)`, m.ID, i, r); err != nil {
				return fmt.Errorf("insert recipient: %w", err)
			}
		}
		if err := insertEvent(ctx, tx, Event{Time: m.ReceivedAt, MailID: m.ID, Kind: KindMailAccepted, Level: Info}); err != nil {
			return err
		}
		for _, d := range m.Deliveries {
			var next any
			switch d.Status {
			case Queued:
				next = at
			case Skipped:
			default:
				return fmt.Errorf("new delivery for %s: invalid status %q", d.Target, d.Status)
			}
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO deliveries (mail_id, target, status, next_attempt_at, updated_at)
				VALUES (?, ?, ?, ?, ?)`, m.ID, d.Target, string(d.Status), next, at); err != nil {
				return fmt.Errorf("insert delivery: %w", err)
			}
			if d.Status == Skipped {
				if err := insertEvent(ctx, tx, Event{
					Time: m.ReceivedAt, MailID: m.ID, Target: d.Target, Kind: d.Kind, Level: d.Level, Text: d.Text,
				}); err != nil {
					return err
				}
			}
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("store: accept %s: %w", m.ID, err)
	}
	return nil
}

// Claimed is a delivery that a worker has taken for one attempt.
type Claimed struct {
	MailID       string
	Target       string
	Attempt      int
	ReceivedAt   time.Time
	EnvelopeFrom string
	Recipients   []string
	Params       Params
	// Raw is the sealed message. It is nil if retention has removed it; the
	// worker must then fail the delivery (KindRawExpired) without sending.
	Raw []byte
}

// Claim takes the next due queued delivery of target: it moves it to
// sending, counts the attempt and writes a delivery.attempt event, all in one
// transaction. It returns nil if nothing is due.
func (s *Store) Claim(ctx context.Context, target string, now time.Time) (*Claimed, error) {
	var c *Claimed
	err := s.eventTx(ctx, func(tx *sql.Tx) error {
		var mailID string
		var attempts int
		err := tx.QueryRowContext(ctx, `
			SELECT mail_id, attempts FROM deliveries
			WHERE target = ? AND status = 'queued' AND next_attempt_at <= ?
			ORDER BY next_attempt_at, mail_id LIMIT 1`, target, ms(now)).Scan(&mailID, &attempts)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("find due: %w", err)
		}
		claimed := &Claimed{MailID: mailID, Target: target, Attempt: attempts + 1}
		if _, err := tx.ExecContext(ctx, `
			UPDATE deliveries SET status = 'sending', attempts = ?, next_attempt_at = NULL,
				data_sent_at = NULL, updated_at = ?
			WHERE mail_id = ? AND target = ?`, claimed.Attempt, ms(now), mailID, target); err != nil {
			return fmt.Errorf("mark sending: %w", err)
		}
		var received int64
		var raw []byte
		var body sql.NullString
		var declared sql.NullInt64
		if err := tx.QueryRowContext(ctx,
			`SELECT received_at, envelope_from, raw, body, smtputf8, declared_size FROM mails WHERE id = ?`, mailID).
			Scan(&received, &claimed.EnvelopeFrom, &raw, &body, &claimed.Params.SMTPUTF8, &declared); err != nil {
			return fmt.Errorf("load mail: %w", err)
		}
		claimed.Raw = raw
		claimed.Params.Body, claimed.Params.DeclaredSize = body.String, declared.Int64
		claimed.ReceivedAt = time.UnixMilli(received)
		rows, err := tx.QueryContext(ctx,
			`SELECT address FROM recipients WHERE mail_id = ? ORDER BY position`, mailID)
		if err != nil {
			return fmt.Errorf("load recipients: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var a string
			if err := rows.Scan(&a); err != nil {
				return fmt.Errorf("load recipients: %w", err)
			}
			claimed.Recipients = append(claimed.Recipients, a)
		}
		if err := rows.Err(); err != nil {
			return fmt.Errorf("load recipients: %w", err)
		}
		if err := insertEvent(ctx, tx, Event{
			Time: now, MailID: mailID, Target: target, Kind: KindDeliveryAttempt, Level: Info, Attempt: claimed.Attempt,
		}); err != nil {
			return err
		}
		c = claimed
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("store: claim for %s: %w", target, err)
	}
	return c, nil
}

// MarkDataSent records that the message body is about to be sent (D5). From
// here on, a broken connection is an unclear outcome.
func (s *Store) MarkDataSent(ctx context.Context, mailID, target string, at time.Time) error {
	err := s.tx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `
			UPDATE deliveries SET data_sent_at = ?, updated_at = ?
			WHERE mail_id = ? AND target = ? AND status = 'sending'`, ms(at), ms(at), mailID, target)
		if err != nil {
			return err
		}
		return oneRow(res)
	})
	if err != nil {
		return fmt.Errorf("store: mark data sent %s/%s: %w", mailID, target, err)
	}
	return nil
}

// Result is the outcome of one attempt, or of the recovery of a delivery left
// in sending.
type Result struct {
	MailID  string
	Target  string
	At      time.Time
	Attempt int
	// Status is Queued (retry at NextAttempt), HandedOff, Failed or Unknown.
	Status      Status
	NextAttempt time.Time
	// Code and Text are the target's reply or an error description. Text is
	// redacted before it is stored.
	Code  int
	Text  string
	Kind  string
	Level Level
}

// Finish moves a delivery out of sending and writes its event, in one
// transaction. With PurgeRawOnHandoff, the same transaction removes the
// stored message once every delivery of the mail is handed_off or skipped.
func (s *Store) Finish(ctx context.Context, r Result) error {
	var next any
	switch r.Status {
	case Queued:
		next = ms(r.NextAttempt)
	case HandedOff, Failed, Unknown:
	default:
		return fmt.Errorf("store: finish %s/%s: invalid status %q", r.MailID, r.Target, r.Status)
	}
	text := redact.Text(r.Text)
	err := s.eventTx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `
			UPDATE deliveries SET status = ?, next_attempt_at = ?, last_code = ?, last_text = ?, updated_at = ?
			WHERE mail_id = ? AND target = ? AND status = 'sending'`,
			string(r.Status), next, nullInt(r.Code), nullStr(text), ms(r.At), r.MailID, r.Target)
		if err != nil {
			return fmt.Errorf("update delivery: %w", err)
		}
		if err := oneRow(res); err != nil {
			return err
		}
		if r.Status == HandedOff && s.opts.PurgeRawOnHandoff {
			if _, err := tx.ExecContext(ctx, `
				UPDATE mails SET raw = NULL, raw_purged_at = ?
				WHERE id = ? AND raw IS NOT NULL AND NOT EXISTS (
					SELECT 1 FROM deliveries WHERE mail_id = ? AND status NOT IN ('handed_off', 'skipped'))`,
				ms(r.At), r.MailID, r.MailID); err != nil {
				return fmt.Errorf("purge raw: %w", err)
			}
		}
		return insertEvent(ctx, tx, Event{
			Time: r.At, MailID: r.MailID, Target: r.Target, Kind: r.Kind, Level: r.Level,
			Attempt: r.Attempt, Code: r.Code, Text: text,
		})
	})
	if err != nil {
		return fmt.Errorf("store: finish %s/%s: %w", r.MailID, r.Target, err)
	}
	return nil
}

// Stuck is a delivery found in sending at start-up.
type Stuck struct {
	MailID     string
	Attempt    int
	DataSent   bool
	ReceivedAt time.Time
}

// Sending lists the deliveries of target that are in sending. At start-up
// these are left over from a crash (D6).
func (s *Store) Sending(ctx context.Context, target string) ([]Stuck, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT d.mail_id, d.attempts, d.data_sent_at IS NOT NULL, m.received_at
		FROM deliveries d JOIN mails m ON m.id = d.mail_id
		WHERE d.target = ? AND d.status = 'sending' ORDER BY d.mail_id`, target)
	if err != nil {
		return nil, fmt.Errorf("store: list sending: %w", err)
	}
	defer rows.Close()
	var out []Stuck
	for rows.Next() {
		var st Stuck
		var received int64
		if err := rows.Scan(&st.MailID, &st.Attempt, &st.DataSent, &received); err != nil {
			return nil, fmt.Errorf("store: list sending: %w", err)
		}
		st.ReceivedAt = time.UnixMilli(received)
		out = append(out, st)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list sending: %w", err)
	}
	return out, nil
}

// NextDue returns the earliest next attempt of a queued delivery of target.
func (s *Store) NextDue(ctx context.Context, target string) (time.Time, bool, error) {
	var next sql.NullInt64
	err := s.db.QueryRowContext(ctx, `
		SELECT MIN(next_attempt_at) FROM deliveries WHERE target = ? AND status = 'queued'`, target).Scan(&next)
	if err != nil {
		return time.Time{}, false, fmt.Errorf("store: next due: %w", err)
	}
	if !next.Valid {
		return time.Time{}, false, nil
	}
	return time.UnixMilli(next.Int64), true, nil
}

// PendingByTarget counts queued and sending deliveries per target name.
func (s *Store) PendingByTarget(ctx context.Context) (map[string]int, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT target, COUNT(*) FROM deliveries WHERE status IN ('queued', 'sending') GROUP BY target`)
	if err != nil {
		return nil, fmt.Errorf("store: pending: %w", err)
	}
	defer rows.Close()
	out := make(map[string]int)
	for rows.Next() {
		var t string
		var n int
		if err := rows.Scan(&t, &n); err != nil {
			return nil, fmt.Errorf("store: pending: %w", err)
		}
		out[t] = n
	}
	return out, rows.Err()
}

// Delivery is the current state of one delivery.
type Delivery struct {
	MailID      string
	Target      string
	Status      Status
	Attempts    int
	NextAttempt time.Time // zero unless queued
	DataSent    time.Time // zero if not recorded
	LastCode    int
	LastText    string
}

// Deliveries returns the deliveries of a mail, ordered by target.
func (s *Store) Deliveries(ctx context.Context, mailID string) ([]Delivery, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT mail_id, target, status, attempts, next_attempt_at, data_sent_at, last_code, last_text
		FROM deliveries WHERE mail_id = ? ORDER BY target`, mailID)
	if err != nil {
		return nil, fmt.Errorf("store: deliveries: %w", err)
	}
	defer rows.Close()
	var out []Delivery
	for rows.Next() {
		var d Delivery
		var status string
		var next, sent, code sql.NullInt64
		var text sql.NullString
		if err := rows.Scan(&d.MailID, &d.Target, &status, &d.Attempts, &next, &sent, &code, &text); err != nil {
			return nil, fmt.Errorf("store: deliveries: %w", err)
		}
		d.Status = Status(status)
		if next.Valid {
			d.NextAttempt = time.UnixMilli(next.Int64)
		}
		if sent.Valid {
			d.DataSent = time.UnixMilli(sent.Int64)
		}
		d.LastCode = int(code.Int64)
		d.LastText = text.String
		out = append(out, d)
	}
	return out, rows.Err()
}

// EventsAfter returns up to limit events with seq greater than after, in
// order.
func (s *Store) EventsAfter(ctx context.Context, after int64, limit int) ([]Event, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT seq, time, mail_id, target, kind, level, attempt, code, text, sender
		FROM events WHERE seq > ? ORDER BY seq LIMIT ?`, after, limit)
	if err != nil {
		return nil, fmt.Errorf("store: events: %w", err)
	}
	defer rows.Close()
	var out []Event
	for rows.Next() {
		var e Event
		var t int64
		var level string
		var mailID, target, text, sender sql.NullString
		var attempt, code sql.NullInt64
		if err := rows.Scan(&e.Seq, &t, &mailID, &target, &e.Kind, &level, &attempt, &code, &text, &sender); err != nil {
			return nil, fmt.Errorf("store: events: %w", err)
		}
		e.Time = time.UnixMilli(t).UTC()
		e.Level = Level(level)
		e.MailID, e.Target, e.Text, e.Sender = mailID.String, target.String, text.String, sender.String
		e.Attempt, e.Code = int(attempt.Int64), int(code.Int64)
		out = append(out, e)
	}
	return out, rows.Err()
}

// Cursor returns the last exported seq of sink, or 0.
func (s *Store) Cursor(ctx context.Context, sink string) (int64, error) {
	var seq int64
	err := s.db.QueryRowContext(ctx, `SELECT seq FROM cursors WHERE sink = ?`, sink).Scan(&seq)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("store: cursor %s: %w", sink, err)
	}
	return seq, nil
}

// SetCursor stores the last exported seq of sink.
func (s *Store) SetCursor(ctx context.Context, sink string, seq int64) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO cursors (sink, seq) VALUES (?, ?)
		ON CONFLICT (sink) DO UPDATE SET seq = excluded.seq`, sink, seq)
	if err != nil {
		return fmt.Errorf("store: set cursor %s: %w", sink, err)
	}
	return nil
}

func oneRow(res sql.Result) error {
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return ErrNotSending
	}
	return nil
}

func ms(t time.Time) int64 { return t.UnixMilli() }

func nullStr(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func nullInt64(n int64) any {
	if n == 0 {
		return nil
	}
	return n
}

func nullInt(n int) any {
	if n == 0 {
		return nil
	}
	return n
}
