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
	"sync/atomic"
	"time"

	_ "modernc.org/sqlite" // registers the "sqlite" driver

	"github.com/eneuberg/postwerk/internal/redact"
)

//go:embed migrations/*.sql
var migrationFS embed.FS

// Status of one recipient of one delivery: the unit of tracking is the
// triple (mail, target, recipient), and its status is the single source of
// truth. The status of a delivery or a mail is derived (Summarize).
type Status string

// Recipient statuses, stored per (mail, target, recipient).
const (
	Queued    Status = "queued"
	Sending   Status = "sending"
	HandedOff Status = "handed_off"
	Failed    Status = "failed"
	Unknown   Status = "unknown"
	Skipped   Status = "skipped"
)

// Partial is a derived summary only, never stored: some recipients were
// handed off and the others failed.
const Partial Status = "partial"

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
	// KindDeliveryTooLarge: the target offers a SIZE smaller than the
	// message; nothing was sent.
	KindDeliveryTooLarge = "delivery.too_large"
	// KindDeliveryRejected: the target refused this recipient's address
	// for good (D46); a matter for the sender, not the operator.
	KindDeliveryRejected = "delivery.rejected"
	// KindTargetDown: a rejected login (535) or a target that cannot be
	// reached stopped all sending to it (D45). One per outage, no mail id.
	KindTargetDown = "target.down"
	// KindTargetRecovered: a probe succeeded and sending resumes; the text
	// holds how long the target was down.
	KindTargetRecovered = "target.recovered"
	// KindRawExpired: the stored message was removed by retention while the
	// delivery still needed it; the delivery is failed and cannot be resent.
	KindRawExpired = "retention.raw_expired"
	// KindRetentionPurged: one retention run removed or replaced data; the
	// text holds counts only.
	KindRetentionPurged = "retention.purged"
	// KindDecryptFailed: the stored message could not be decrypted; the
	// delivery is failed and nothing was sent.
	KindDecryptFailed = "store.decrypt_failed"

	// Events of people using the UI (U13). They name the account in Actor
	// and never carry a search query, an address or a password.
	KindUILogin       = "ui.login"
	KindUILoginFailed = "ui.login_failed"
	KindUISearch      = "ui.search"
	// KindUILoginThrottled: failed logins for one name or from one source
	// started a wait (U22); the text is "account" or "source".
	KindUILoginThrottled = "ui.login_throttled"
	// KindUIView: someone opened the detail page of the event's mail (U24).
	KindUIView = "ui.view"
	// Account changes in the admin panel (U17–U21); the text is the name
	// of the account changed, Actor the account that changed it.
	KindUIAccountCreated = "ui.account_created"
	KindUIAccountDeleted = "ui.account_deleted"
	KindUIPasswordSet    = "ui.password_set"
)

// ErrNotSending is returned when a recipient that should be in "sending" is
// not, which means two writers disagree about it.
var ErrNotSending = errors.New("store: not in sending state")

// Store is an open Postwerk database.
type Store struct {
	db   *sql.DB
	opts Options
	// lock is the open lock file; its flock is held until Close (D61).
	lock *os.File
	// writeErr holds the last write that failed for lack of space or
	// storage, until a write succeeds (Writable).
	writeErr atomic.Pointer[error]
	// schema is the schema version after Open; migrated the migrations
	// Open applied, by file name.
	schema   int
	migrated []string
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
	// LockWait is how long Open waits for another process to let go of
	// the data lock (D61) before it returns ErrLocked; 0 does not wait.
	LockWait time.Duration
	// OnLockWait, if set, is called while Open waits for the data lock:
	// at once and then every few seconds, with the time waited so far and
	// what the holder wrote into the lock file.
	OnLockWait func(waited time.Duration, holder string)
}

// Open opens or creates the database at path, applies the pragmas, checks
// that they took effect and runs pending migrations.
//
// Before it touches the database, Open takes an exclusive lock on the file
// LockPath(path) (D61), waiting up to opts.LockWait while another process
// holds it. Close releases it; so does the end of the process.
func Open(ctx context.Context, path string, opts Options) (s *Store, err error) {
	if path == "" || strings.ContainsAny(path, "?#") || strings.HasPrefix(path, "file:") {
		return nil, errors.New("store: invalid database path (no '?', '#' or file: prefix)")
	}
	lock, err := acquireLock(ctx, path, opts.LockWait, opts.OnLockWait)
	if err != nil {
		return nil, err
	}
	defer func() {
		if err != nil {
			lock.Close()
		}
	}()
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

	s = &Store{db: db, opts: opts, lock: lock}
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

// Close closes the database, then releases the data lock.
func (s *Store) Close() error {
	err := s.db.Close()
	if s.lock != nil {
		s.lock.Close()
		s.lock = nil
	}
	return err
}

// Schema returns the schema version of the database and the migrations
// Open applied to reach it (empty if it was up to date), for the start log.
func (s *Store) Schema() (version int, applied []string) {
	return s.schema, slices.Clone(s.migrated)
}

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
	s.schema = current
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
		s.schema = version
		s.migrated = append(s.migrated, strings.TrimSuffix(path.Base(name), ".sql"))
	}
	return nil
}

// tx runs fn in a transaction and commits it if fn returns nil.
func (s *Store) tx(ctx context.Context, fn func(*sql.Tx) error) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return s.noteWrite(err)
	}
	if err := fn(tx); err != nil {
		_ = tx.Rollback()
		return s.noteWrite(err)
	}
	return s.noteWrite(tx.Commit())
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
	// Recipient is the position of the recipient the event is about, nil
	// for events about the mail or the whole attempt. Events never carry
	// the address itself.
	Recipient *int
	// Actor names the person behind an event caused through the UI or the
	// query API: the token name. Empty for events of the relay itself.
	Actor string
}

// Position returns a pointer to p, for Event.Recipient.
func Position(p int) *int { return &p }

// insertEvent appends e. The sender is taken from the mail row, or from
// e.Sender for an event without a mail (a refused login names the sender
// it tried); the text is redacted.
func insertEvent(ctx context.Context, tx *sql.Tx, e Event) error {
	var rcpt any
	if e.Recipient != nil {
		rcpt = *e.Recipient
	}
	_, err := tx.ExecContext(ctx, `
		INSERT INTO events (time, mail_id, target, recipient, kind, level, attempt, code, text, sender, actor)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, COALESCE((SELECT sender FROM mails WHERE id = ?), ?), ?)`,
		ms(e.Time), nullStr(e.MailID), nullStr(e.Target), rcpt, e.Kind, string(e.Level),
		nullInt(e.Attempt), nullInt(e.Code), nullStr(redact.Text(e.Text)), e.MailID, nullStr(e.Sender), nullStr(e.Actor))
	if err != nil {
		return fmt.Errorf("insert event %s: %w", e.Kind, err)
	}
	return nil
}

// Append writes one event that belongs to no state change, such as a login
// to the UI (U13). Its text is redacted like every other.
func (s *Store) Append(ctx context.Context, e Event) error {
	switch e.Level {
	case Info, Warn, Error:
	default:
		return fmt.Errorf("store: append %s: invalid level %q", e.Kind, e.Level)
	}
	if e.Kind == "" {
		return errors.New("store: append: event without kind")
	}
	if err := s.eventTx(ctx, func(tx *sql.Tx) error { return insertEvent(ctx, tx, e) }); err != nil {
		return fmt.Errorf("store: append %s: %w", e.Kind, err)
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

// Accept stores a mail, its recipients, its deliveries with one status per
// recipient, and their events in one transaction (I6, S4). Queued deliveries
// are due at once. If no delivery is queued and PurgeRawOnHandoff is set, the
// message is not stored at all.
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
				INSERT INTO deliveries (mail_id, target, next_attempt_at, updated_at)
				VALUES (?, ?, ?, ?)`, m.ID, d.Target, next, at); err != nil {
				return fmt.Errorf("insert delivery: %w", err)
			}
			for i := range m.Recipients {
				if _, err := tx.ExecContext(ctx, `
					INSERT INTO delivery_recipients (mail_id, target, position, status, updated_at)
					VALUES (?, ?, ?, ?, ?)`, m.ID, d.Target, i, string(d.Status), at); err != nil {
					return fmt.Errorf("insert delivery recipient: %w", err)
				}
				if d.Status == Skipped {
					if err := insertEvent(ctx, tx, Event{
						Time: m.ReceivedAt, MailID: m.ID, Target: d.Target, Recipient: Position(i),
						Kind: d.Kind, Level: d.Level, Text: d.Text,
					}); err != nil {
						return err
					}
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

// Recipient is one envelope recipient of a claimed delivery.
type Recipient struct {
	Position int
	Address  string
}

// Claimed is a delivery that a worker has taken for one attempt.
type Claimed struct {
	MailID       string
	Target       string
	Attempt      int
	ReceivedAt   time.Time
	EnvelopeFrom string
	// Recipients are the recipients of this attempt: those that were
	// queued for the target. They are now in sending.
	Recipients []Recipient
	Params     Params
	// Raw is the sealed message. It is nil if retention has removed it; the
	// worker must then fail the delivery (KindRawExpired) without sending.
	Raw []byte
}

// Addresses returns the addresses of the claimed recipients.
func (c *Claimed) Addresses() []string {
	out := make([]string, len(c.Recipients))
	for i, r := range c.Recipients {
		out[i] = r.Address
	}
	return out
}

// Claim takes the next due delivery of target: it moves its queued
// recipients to sending, counts the attempt and writes a delivery.attempt
// event, all in one transaction. It returns nil if nothing is due.
func (s *Store) Claim(ctx context.Context, target string, now time.Time) (*Claimed, error) {
	var c *Claimed
	err := s.eventTx(ctx, func(tx *sql.Tx) error {
		var mailID string
		var attempts int
		err := tx.QueryRowContext(ctx, `
			SELECT mail_id, attempts FROM deliveries
			WHERE target = ? AND next_attempt_at IS NOT NULL AND next_attempt_at <= ?
			ORDER BY next_attempt_at, mail_id LIMIT 1`, target, ms(now)).Scan(&mailID, &attempts)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("find due: %w", err)
		}
		claimed := &Claimed{MailID: mailID, Target: target, Attempt: attempts + 1}
		if _, err := tx.ExecContext(ctx, `
			UPDATE deliveries SET attempts = ?, next_attempt_at = NULL, data_sent_at = NULL, updated_at = ?
			WHERE mail_id = ? AND target = ?`, claimed.Attempt, ms(now), mailID, target); err != nil {
			return fmt.Errorf("mark sending: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `
			UPDATE delivery_recipients SET status = 'sending', updated_at = ?
			WHERE mail_id = ? AND target = ? AND status = 'queued'`, ms(now), mailID, target); err != nil {
			return fmt.Errorf("mark recipients sending: %w", err)
		}
		rows, err := tx.QueryContext(ctx, `
			SELECT r.position, r.address FROM recipients r
			JOIN delivery_recipients dr ON dr.mail_id = r.mail_id AND dr.position = r.position
			WHERE dr.mail_id = ? AND dr.target = ? AND dr.status = 'sending'
			ORDER BY r.position`, mailID, target)
		if err != nil {
			return fmt.Errorf("load recipients: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var r Recipient
			if err := rows.Scan(&r.Position, &r.Address); err != nil {
				return fmt.Errorf("load recipients: %w", err)
			}
			claimed.Recipients = append(claimed.Recipients, r)
		}
		if err := rows.Err(); err != nil {
			return fmt.Errorf("load recipients: %w", err)
		}
		if len(claimed.Recipients) == 0 {
			// Due without a queued recipient cannot happen; the cleared
			// next attempt keeps it from being found again.
			return nil
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

// RecipientResult is the outcome of one attempt for one recipient.
type RecipientResult struct {
	Position int
	// Status is Queued (retry at the delivery's next attempt), HandedOff,
	// Failed or Unknown.
	Status Status
	// Code and Text are the reply or an error description; Text is
	// redacted before it is stored.
	Code  int
	Text  string
	Kind  string
	Level Level
}

// MarkDataSent records that the message body is about to be sent (D5). From
// here on, a broken connection is an unclear outcome for the recipients that
// are still in sending. rejected holds the recipients the target refused at
// RCPT in this attempt; they are written now, so that a crash during the
// message cannot turn their clear answer into an unclear one.
func (s *Store) MarkDataSent(ctx context.Context, mailID, target string, at time.Time, attempt int,
	rejected []RecipientResult) error {
	err := s.eventTx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `
			UPDATE deliveries SET data_sent_at = ?, updated_at = ?
			WHERE mail_id = ? AND target = ? AND EXISTS (
				SELECT 1 FROM delivery_recipients WHERE mail_id = ? AND target = ? AND status = 'sending')`,
			ms(at), ms(at), mailID, target, mailID, target)
		if err != nil {
			return err
		}
		if err := oneRow(res); err != nil {
			return err
		}
		return applyRecipients(ctx, tx, mailID, target, at, attempt, rejected)
	})
	if err != nil {
		return fmt.Errorf("store: mark data sent %s/%s: %w", mailID, target, err)
	}
	return nil
}

// applyRecipients moves recipients out of sending and writes one event for
// each.
func applyRecipients(ctx context.Context, tx *sql.Tx, mailID, target string, at time.Time, attempt int,
	results []RecipientResult) error {
	for _, r := range results {
		switch r.Status {
		case Queued, HandedOff, Failed, Unknown:
		default:
			return fmt.Errorf("recipient %d: invalid status %q", r.Position, r.Status)
		}
		text := redact.Text(r.Text)
		res, err := tx.ExecContext(ctx, `
			UPDATE delivery_recipients SET status = ?, last_code = ?, last_text = ?, updated_at = ?
			WHERE mail_id = ? AND target = ? AND position = ? AND status = 'sending'`,
			string(r.Status), nullInt(r.Code), nullStr(text), ms(at), mailID, target, r.Position)
		if err != nil {
			return fmt.Errorf("update recipient %d: %w", r.Position, err)
		}
		if err := oneRow(res); err != nil {
			return fmt.Errorf("recipient %d: %w", r.Position, err)
		}
		if err := insertEvent(ctx, tx, Event{
			Time: at, MailID: mailID, Target: target, Recipient: Position(r.Position), Kind: r.Kind, Level: r.Level,
			Attempt: attempt, Code: r.Code, Text: text,
		}); err != nil {
			return err
		}
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
	// Recipients holds one result for every recipient still in sending.
	Recipients []RecipientResult
	// NextAttempt is when the recipients left queued are tried again. It is
	// required if any recipient of the delivery is queued afterwards.
	NextAttempt time.Time
	// Code and Text describe the attempt as a whole (the last reply), for
	// the delivery row. Text is redacted before it is stored.
	Code int
	Text string
}

// Finish moves the recipients of an attempt out of sending, writes one event
// per recipient and schedules the next attempt if a recipient is queued, in
// one transaction. With PurgeRawOnHandoff, the same transaction removes the
// stored message once every recipient of every delivery of the mail is
// handed_off, skipped or closed by an admin.
func (s *Store) Finish(ctx context.Context, r Result) error {
	text := redact.Text(r.Text)
	err := s.eventTx(ctx, func(tx *sql.Tx) error {
		if err := applyRecipients(ctx, tx, r.MailID, r.Target, r.At, r.Attempt, r.Recipients); err != nil {
			return err
		}
		var sending, queued int
		if err := tx.QueryRowContext(ctx, `
			SELECT COALESCE(SUM(status = 'sending'), 0), COALESCE(SUM(status = 'queued'), 0)
			FROM delivery_recipients WHERE mail_id = ? AND target = ?`, r.MailID, r.Target).
			Scan(&sending, &queued); err != nil {
			return fmt.Errorf("count recipients: %w", err)
		}
		if sending > 0 {
			return fmt.Errorf("%d recipients left in sending", sending)
		}
		var next any
		if queued > 0 {
			if r.NextAttempt.IsZero() {
				return errors.New("queued recipients without a next attempt")
			}
			next = ms(r.NextAttempt)
		}
		res, err := tx.ExecContext(ctx, `
			UPDATE deliveries SET next_attempt_at = ?, last_code = ?, last_text = ?, updated_at = ?
			WHERE mail_id = ? AND target = ?`,
			next, nullInt(r.Code), nullStr(text), ms(r.At), r.MailID, r.Target)
		if err != nil {
			return fmt.Errorf("update delivery: %w", err)
		}
		if err := oneRow(res); err != nil {
			return err
		}
		if s.opts.PurgeRawOnHandoff {
			if _, err := tx.ExecContext(ctx, `
				UPDATE mails SET raw = NULL, raw_purged_at = ?
				WHERE id = ? AND raw IS NOT NULL AND NOT EXISTS (
					SELECT 1 FROM delivery_recipients WHERE mail_id = ? AND status NOT IN ('handed_off', 'skipped')
						AND resolution IS NULL)`,
				ms(r.At), r.MailID, r.MailID); err != nil {
				return fmt.Errorf("purge raw: %w", err)
			}
		}
		return nil
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
	// Positions are the recipients still in sending.
	Positions []int
}

// Sending lists the deliveries of target that have recipients in sending.
// At start-up these are left over from a crash (D6).
func (s *Store) Sending(ctx context.Context, target string) ([]Stuck, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT d.mail_id, d.attempts, d.data_sent_at IS NOT NULL, m.received_at, dr.position
		FROM deliveries d
		JOIN mails m ON m.id = d.mail_id
		JOIN delivery_recipients dr ON dr.mail_id = d.mail_id AND dr.target = d.target
		WHERE d.target = ? AND dr.status = 'sending' ORDER BY d.mail_id, dr.position`, target)
	if err != nil {
		return nil, fmt.Errorf("store: list sending: %w", err)
	}
	defer rows.Close()
	var out []Stuck
	for rows.Next() {
		var st Stuck
		var received int64
		var pos int
		if err := rows.Scan(&st.MailID, &st.Attempt, &st.DataSent, &received, &pos); err != nil {
			return nil, fmt.Errorf("store: list sending: %w", err)
		}
		if n := len(out); n > 0 && out[n-1].MailID == st.MailID {
			out[n-1].Positions = append(out[n-1].Positions, pos)
			continue
		}
		st.ReceivedAt = time.UnixMilli(received)
		st.Positions = []int{pos}
		out = append(out, st)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list sending: %w", err)
	}
	return out, nil
}

// NextDue returns the earliest next attempt of a delivery of target.
func (s *Store) NextDue(ctx context.Context, target string) (time.Time, bool, error) {
	var next sql.NullInt64
	err := s.db.QueryRowContext(ctx, `
		SELECT MIN(next_attempt_at) FROM deliveries WHERE target = ? AND next_attempt_at IS NOT NULL`, target).Scan(&next)
	if err != nil {
		return time.Time{}, false, fmt.Errorf("store: next due: %w", err)
	}
	if !next.Valid {
		return time.Time{}, false, nil
	}
	return time.UnixMilli(next.Int64), true, nil
}

// PendingByTarget counts, per target name, the deliveries with a recipient
// still queued or sending.
func (s *Store) PendingByTarget(ctx context.Context) (map[string]int, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT target, COUNT(DISTINCT mail_id) FROM delivery_recipients
		WHERE status IN ('queued', 'sending') GROUP BY target`)
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

// RecipientState is the stored state of one recipient of one delivery: the
// single source of truth for what happened to that (mail, target,
// recipient).
type RecipientState struct {
	Position int
	Status   Status
	LastCode int
	LastText string
	// Resolution is set when an admin closed the recipient (D53).
	Resolution Resolution
}

// Delivery is the current state of one delivery.
type Delivery struct {
	MailID string
	Target string
	// Status is derived from the recipients' statuses (Summarize); it is
	// not stored.
	Status      Status
	Attempts    int
	NextAttempt time.Time // zero unless a recipient is queued
	DataSent    time.Time // zero if not recorded
	LastCode    int
	LastText    string
	Recipients  []RecipientState
}

// Deliveries returns the deliveries of a mail, ordered by target, each with
// its recipients in order.
func (s *Store) Deliveries(ctx context.Context, mailID string) ([]Delivery, error) {
	var out []Delivery
	// One transaction, so the deliveries and their recipients are read in
	// the same state.
	err := s.tx(ctx, func(tx *sql.Tx) error {
		var err error
		out, err = deliveries(ctx, tx, mailID)
		return err
	})
	if err != nil {
		return nil, fmt.Errorf("store: deliveries: %w", err)
	}
	return out, nil
}

func deliveries(ctx context.Context, tx *sql.Tx, mailID string) ([]Delivery, error) {
	rows, err := tx.QueryContext(ctx, `
		SELECT mail_id, target, attempts, next_attempt_at, data_sent_at, last_code, last_text
		FROM deliveries WHERE mail_id = ? ORDER BY target`, mailID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Delivery
	for rows.Next() {
		var d Delivery
		var next, sent, code sql.NullInt64
		var text sql.NullString
		if err := rows.Scan(&d.MailID, &d.Target, &d.Attempts, &next, &sent, &code, &text); err != nil {
			return nil, err
		}
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
	if err := rows.Err(); err != nil {
		return nil, err
	}
	rows.Close()

	rrows, err := tx.QueryContext(ctx, `
		SELECT target, position, status, last_code, last_text, resolution FROM delivery_recipients
		WHERE mail_id = ? ORDER BY target, position`, mailID)
	if err != nil {
		return nil, err
	}
	defer rrows.Close()
	byTarget := make(map[string]*Delivery, len(out))
	for i := range out {
		byTarget[out[i].Target] = &out[i]
	}
	for rrows.Next() {
		var target, status string
		var r RecipientState
		var code sql.NullInt64
		var text, res sql.NullString
		if err := rrows.Scan(&target, &r.Position, &status, &code, &text, &res); err != nil {
			return nil, err
		}
		r.Status, r.LastCode, r.LastText = Status(status), int(code.Int64), text.String
		r.Resolution = Resolution(res.String)
		if d := byTarget[target]; d != nil {
			d.Recipients = append(d.Recipients, r)
		}
	}
	if err := rrows.Err(); err != nil {
		return nil, err
	}
	for i := range out {
		statuses := make([]Status, len(out[i].Recipients))
		for j, r := range out[i].Recipients {
			statuses[j] = r.Status
		}
		out[i].Status = Summarize(statuses)
	}
	return out, nil
}

// Summarize derives the status of a delivery (or of a mail, over all its
// deliveries' recipients) from the statuses of its recipients. Nothing
// derived is stored. In order: sending while any recipient is being sent,
// queued while any is queued, unknown while any is unknown, skipped when all
// are skipped; then handed_off when every recipient that was not skipped
// was handed off, failed when none was, and partial otherwise.
func Summarize(statuses []Status) Status {
	n := map[Status]int{}
	for _, s := range statuses {
		n[s]++
	}
	return summarizeCounts(n, len(statuses))
}

// MailStatus derives the state of a mail from its deliveries: over the
// recipients of the required targets only, because a failed capture copy
// does not make a mail a problem (design.md section 4). A mail whose
// deliveries all go to optional targets is derived from all of them.
// optional reports whether a target is optional; nil means none is.
// ListMails applies the same rule in SQL (MailFilter.Optional).
func MailStatus(ds []Delivery, optional func(target string) bool) Status {
	var required, all []Status
	for _, d := range ds {
		for _, r := range d.Recipients {
			all = append(all, r.Status)
			if optional == nil || !optional(d.Target) {
				required = append(required, r.Status)
			}
		}
	}
	if len(required) == 0 {
		return Summarize(all)
	}
	return Summarize(required)
}

// summarizeCounts is Summarize over counts per status; total is the number
// of recipients. stateSQL (read.go) is the same rule in SQL.
func summarizeCounts(n map[Status]int, total int) Status {
	switch {
	case total == 0:
		return ""
	case n[Sending] > 0:
		return Sending
	case n[Queued] > 0:
		return Queued
	case n[Unknown] > 0:
		return Unknown
	case n[Skipped] == total:
		return Skipped
	case n[Failed] == 0:
		return HandedOff
	case n[HandedOff] == 0:
		return Failed
	}
	return Partial
}

// EventsAfter returns up to limit events with seq greater than after, in
// order.
func (s *Store) EventsAfter(ctx context.Context, after int64, limit int) ([]Event, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT `+eventColumns+` FROM events WHERE seq > ? ORDER BY seq LIMIT ?`, after, limit)
	if err != nil {
		return nil, fmt.Errorf("store: events: %w", err)
	}
	out, err := scanEvents(rows)
	if err != nil {
		return nil, fmt.Errorf("store: events: %w", err)
	}
	return out, nil
}

const eventColumns = `seq, time, mail_id, target, recipient, kind, level, attempt, code, text, sender, actor`

// scanEvents reads rows of eventColumns and closes them.
func scanEvents(rows *sql.Rows) ([]Event, error) {
	defer rows.Close()
	var out []Event
	for rows.Next() {
		var e Event
		var t int64
		var level string
		var mailID, target, text, sender, actor sql.NullString
		var attempt, code, rcpt sql.NullInt64
		if err := rows.Scan(&e.Seq, &t, &mailID, &target, &rcpt, &e.Kind, &level, &attempt, &code, &text, &sender,
			&actor); err != nil {
			return nil, err
		}
		e.Time = time.UnixMilli(t).UTC()
		e.Level = Level(level)
		e.MailID, e.Target, e.Text, e.Sender, e.Actor = mailID.String, target.String, text.String, sender.String, actor.String
		e.Attempt, e.Code = int(attempt.Int64), int(code.Int64)
		if rcpt.Valid {
			e.Recipient = Position(int(rcpt.Int64))
		}
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
	if err := s.noteWrite(err); err != nil {
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
