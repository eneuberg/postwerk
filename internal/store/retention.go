package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// batch bounds the rows one retention transaction touches, so ingress and
// workers never wait long for the single connection.
const batch = 200

// RetainPolicy says what one retention run removes (S6–S10). Durations are
// measured back from the run's time.
type RetainPolicy struct {
	// RawKeepAfterHandoff: keep the stored message this long after the last
	// recipient became handed_off or skipped, when all recipients of all
	// deliveries are.
	RawKeepAfterHandoff time.Duration
	// RawKeepAfterFailure: keep it this long after the last recipient became
	// final, when one of them is failed or unknown.
	RawKeepAfterFailure time.Duration
	// RawMaxAge: never keep it longer than this after the mail was received.
	RawMaxAge time.Duration
	// Pseudonymize, if not nil, replaces each address by its keyed hash
	// PseudonymizeAfter after the mail was received. Mails with a recipient
	// that may still be sent (queued, sending, unknown) or a stored message
	// are left alone until that is over.
	Pseudonymize      func(addr string) string
	PseudonymizeAfter time.Duration
	// RecordRetention deletes mails, recipients and deliveries. Mails with a
	// recipient that may still be sent are kept.
	RecordRetention time.Duration
	// EventRetention deletes events older than this.
	EventRetention time.Duration
	// Optional names the optional targets: their retention.raw_expired
	// events are warnings, not errors.
	Optional map[string]bool
}

// RetainCounts is what one retention run did.
type RetainCounts struct {
	RawPurged     int // stored messages removed
	RawExpired    int // recipients failed because their message was removed
	Pseudonymized int // mails whose addresses were replaced by hashes
	Records       int // mails deleted with their recipients and deliveries
	Events        int // events deleted
}

func (c RetainCounts) any() bool {
	return c.RawPurged+c.RawExpired+c.Pseudonymized+c.Records+c.Events > 0
}

func (c RetainCounts) String() string {
	return fmt.Sprintf("raw_purged=%d raw_expired=%d pseudonymized=%d records_deleted=%d events_deleted=%d",
		c.RawPurged, c.RawExpired, c.Pseudonymized, c.Records, c.Events)
}

// Retain runs every retention step once, writes one retention.purged event
// with the counts if anything changed, and checkpoints the WAL so that old
// page copies do not linger there (S10). It stops at the first error; the
// steps done before stay done.
func (s *Store) Retain(ctx context.Context, p RetainPolicy, now time.Time) (RetainCounts, error) {
	var c RetainCounts
	steps := []func() error{
		func() error { return s.purgeHandedOff(ctx, p, now, &c) },
		func() error { return s.purgeAfterFailure(ctx, p, now, &c) },
		func() error { return s.purgeMaxAge(ctx, p, now, &c) },
		func() error { return s.pseudonymize(ctx, p, now, &c) },
		func() error { return s.deleteRecords(ctx, p, now, &c) },
		func() error { return s.deleteEvents(ctx, p, now, &c) },
	}
	var err error
	for _, step := range steps {
		if err = step(); err != nil {
			break
		}
	}
	if c.any() {
		if e := s.eventTx(ctx, func(tx *sql.Tx) error {
			return insertEvent(ctx, tx, Event{Time: now, Kind: KindRetentionPurged, Level: Info, Text: c.String()})
		}); e != nil && err == nil {
			err = e
		}
	}
	if e := s.Checkpoint(ctx); e != nil && err == nil {
		err = e
	}
	if err != nil {
		return c, fmt.Errorf("store: retention: %w", err)
	}
	return c, nil
}

// Checkpoint copies the WAL into the database file and truncates it.
func (s *Store) Checkpoint(ctx context.Context) error {
	var busy, logFrames, done int
	if err := s.db.QueryRowContext(ctx, "PRAGMA wal_checkpoint(TRUNCATE)").Scan(&busy, &logFrames, &done); err != nil {
		return fmt.Errorf("checkpoint: %w", err)
	}
	if busy != 0 {
		return errors.New("checkpoint: database busy, WAL not truncated")
	}
	return nil
}

// repeat runs fn in its own transaction until it reports fewer than batch
// rows.
func (s *Store) repeat(ctx context.Context, fn func(tx *sql.Tx) (int, error)) error {
	for {
		var n int
		err := s.eventTx(ctx, func(tx *sql.Tx) error {
			var err error
			n, err = fn(tx)
			return err
		})
		if err != nil {
			return err
		}
		if n < batch {
			return nil
		}
	}
}

// purgeHandedOff removes the message of mails whose recipients are all
// handed_off or skipped at every target, RawKeepAfterHandoff after the last
// of them.
func (s *Store) purgeHandedOff(ctx context.Context, p RetainPolicy, now time.Time, c *RetainCounts) error {
	cutoff := ms(now.Add(-p.RawKeepAfterHandoff))
	return s.repeat(ctx, func(tx *sql.Tx) (int, error) {
		res, err := tx.ExecContext(ctx, `
			UPDATE mails SET raw = NULL, raw_purged_at = ?
			WHERE id IN (
				SELECT m.id FROM mails m
				WHERE m.raw IS NOT NULL
				  AND EXISTS (SELECT 1 FROM delivery_recipients d WHERE d.mail_id = m.id)
				  AND NOT EXISTS (SELECT 1 FROM delivery_recipients d WHERE d.mail_id = m.id
				                  AND d.status NOT IN ('handed_off', 'skipped'))
				  AND (SELECT MAX(d.updated_at) FROM delivery_recipients d WHERE d.mail_id = m.id) <= ?
				LIMIT ?)`, ms(now), cutoff, batch)
		if err != nil {
			return 0, fmt.Errorf("purge handed off: %w", err)
		}
		n, err := res.RowsAffected()
		c.RawPurged += int(n)
		return int(n), err
	})
}

// purgeAfterFailure removes the message of mails whose recipients are all
// final (none queued or sending) with at least one failed or unknown,
// RawKeepAfterFailure after the last change. Unknown recipients become
// failed: without the message they can no longer be resent.
func (s *Store) purgeAfterFailure(ctx context.Context, p RetainPolicy, now time.Time, c *RetainCounts) error {
	cutoff := ms(now.Add(-p.RawKeepAfterFailure))
	return s.purgeEach(ctx, p, now, c, `
		SELECT m.id FROM mails m
		WHERE m.raw IS NOT NULL
		  AND EXISTS (SELECT 1 FROM delivery_recipients d WHERE d.mail_id = m.id AND d.status IN ('failed', 'unknown'))
		  AND NOT EXISTS (SELECT 1 FROM delivery_recipients d WHERE d.mail_id = m.id AND d.status IN ('queued', 'sending'))
		  AND (SELECT MAX(d.updated_at) FROM delivery_recipients d WHERE d.mail_id = m.id) <= ?
		ORDER BY m.id LIMIT ?`, cutoff,
		"stored message removed "+p.RawKeepAfterFailure.String()+" after the failure (POSTWERK_RAW_KEEP_AFTER_FAILURE); resend is no longer possible")
}

// purgeMaxAge removes the message of every mail older than RawMaxAge, in any
// state except while a recipient is being sent (the next run takes it).
// Queued and unknown recipients become failed.
func (s *Store) purgeMaxAge(ctx context.Context, p RetainPolicy, now time.Time, c *RetainCounts) error {
	cutoff := ms(now.Add(-p.RawMaxAge))
	return s.purgeEach(ctx, p, now, c, `
		SELECT m.id FROM mails m
		WHERE m.raw IS NOT NULL AND m.received_at <= ?
		  AND NOT EXISTS (SELECT 1 FROM delivery_recipients d WHERE d.mail_id = m.id AND d.status = 'sending')
		ORDER BY m.id LIMIT ?`, cutoff,
		"stored message removed at its maximum age (POSTWERK_RAW_MAX_AGE); the delivery can no longer be sent")
}

// purgeEach removes the message of each mail that query selects, and fails
// its queued and unknown recipients, each with a retention.raw_expired event.
func (s *Store) purgeEach(ctx context.Context, p RetainPolicy, now time.Time, c *RetainCounts,
	query string, cutoff int64, text string) error {
	return s.repeat(ctx, func(tx *sql.Tx) (int, error) {
		ids, err := queryStrings(ctx, tx, query, cutoff, batch)
		if err != nil {
			return 0, fmt.Errorf("purge: %w", err)
		}
		type rcpt struct {
			target string
			pos    int
		}
		for _, id := range ids {
			rows, err := tx.QueryContext(ctx, `
				SELECT target, position FROM delivery_recipients WHERE mail_id = ? AND status IN ('queued', 'unknown')
				ORDER BY target, position`, id)
			if err != nil {
				return 0, fmt.Errorf("purge %s: %w", id, err)
			}
			var expired []rcpt
			for rows.Next() {
				var r rcpt
				if err := rows.Scan(&r.target, &r.pos); err != nil {
					rows.Close()
					return 0, fmt.Errorf("purge %s: %w", id, err)
				}
				expired = append(expired, r)
			}
			rows.Close()
			if err := rows.Err(); err != nil {
				return 0, fmt.Errorf("purge %s: %w", id, err)
			}
			for _, r := range expired {
				if _, err := tx.ExecContext(ctx, `
					UPDATE delivery_recipients SET status = 'failed', updated_at = ?
					WHERE mail_id = ? AND target = ? AND position = ?`, ms(now), id, r.target, r.pos); err != nil {
					return 0, fmt.Errorf("purge %s: fail recipient: %w", id, err)
				}
				level := Error
				if p.Optional[r.target] {
					level = Warn
				}
				if err := insertEvent(ctx, tx, Event{
					Time: now, MailID: id, Target: r.target, Recipient: Position(r.pos), Kind: KindRawExpired,
					Level: level, Text: text,
				}); err != nil {
					return 0, err
				}
				c.RawExpired++
			}
			if _, err := tx.ExecContext(ctx,
				`UPDATE deliveries SET next_attempt_at = NULL, updated_at = ? WHERE mail_id = ? AND next_attempt_at IS NOT NULL`,
				ms(now), id); err != nil {
				return 0, fmt.Errorf("purge %s: unschedule: %w", id, err)
			}
			if _, err := tx.ExecContext(ctx,
				`UPDATE mails SET raw = NULL, raw_purged_at = ? WHERE id = ?`, ms(now), id); err != nil {
				return 0, fmt.Errorf("purge %s: %w", id, err)
			}
			c.RawPurged++
		}
		return len(ids), nil
	})
}

// pseudonymize replaces the recipient addresses and the envelope sender of
// old mails by their keyed hashes (S7). The null sender stays empty.
func (s *Store) pseudonymize(ctx context.Context, p RetainPolicy, now time.Time, c *RetainCounts) error {
	if p.Pseudonymize == nil || p.PseudonymizeAfter <= 0 {
		return nil
	}
	cutoff := ms(now.Add(-p.PseudonymizeAfter))
	return s.repeat(ctx, func(tx *sql.Tx) (int, error) {
		ids, err := queryStrings(ctx, tx, `
			SELECT m.id FROM mails m
			WHERE m.pseudonymized_at IS NULL AND m.received_at <= ? AND m.raw IS NULL
			  AND NOT EXISTS (SELECT 1 FROM delivery_recipients d WHERE d.mail_id = m.id
			                  AND d.status IN ('queued', 'sending', 'unknown'))
			ORDER BY m.id LIMIT ?`, cutoff, batch)
		if err != nil {
			return 0, fmt.Errorf("pseudonymize: %w", err)
		}
		for _, id := range ids {
			if err := pseudonymizeMail(ctx, tx, id, p.Pseudonymize, now); err != nil {
				return 0, fmt.Errorf("pseudonymize %s: %w", id, err)
			}
			c.Pseudonymized++
		}
		return len(ids), nil
	})
}

func pseudonymizeMail(ctx context.Context, tx *sql.Tx, id string, hash func(string) string, now time.Time) error {
	rows, err := tx.QueryContext(ctx, `SELECT position, address FROM recipients WHERE mail_id = ?`, id)
	if err != nil {
		return err
	}
	type rcpt struct {
		pos  int
		addr string
	}
	var rs []rcpt
	for rows.Next() {
		var r rcpt
		if err := rows.Scan(&r.pos, &r.addr); err != nil {
			rows.Close()
			return err
		}
		rs = append(rs, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, r := range rs {
		if _, err := tx.ExecContext(ctx, `UPDATE recipients SET address = ? WHERE mail_id = ? AND position = ?`,
			hash(r.addr), id, r.pos); err != nil {
			return err
		}
	}
	var from string
	if err := tx.QueryRowContext(ctx, `SELECT envelope_from FROM mails WHERE id = ?`, id).Scan(&from); err != nil {
		return err
	}
	if from != "" {
		from = hash(from)
	}
	_, err = tx.ExecContext(ctx, `UPDATE mails SET envelope_from = ?, pseudonymized_at = ? WHERE id = ?`, from, ms(now), id)
	return err
}

// deleteRecords deletes old mails; recipients and deliveries go with them
// (ON DELETE CASCADE). Events are kept until their own retention.
func (s *Store) deleteRecords(ctx context.Context, p RetainPolicy, now time.Time, c *RetainCounts) error {
	cutoff := ms(now.Add(-p.RecordRetention))
	return s.repeat(ctx, func(tx *sql.Tx) (int, error) {
		res, err := tx.ExecContext(ctx, `
			DELETE FROM mails WHERE id IN (
				SELECT m.id FROM mails m
				WHERE m.received_at < ?
				  AND NOT EXISTS (SELECT 1 FROM delivery_recipients d WHERE d.mail_id = m.id
				                  AND d.status IN ('queued', 'sending', 'unknown'))
				LIMIT ?)`, cutoff, batch)
		if err != nil {
			return 0, fmt.Errorf("delete records: %w", err)
		}
		n, err := res.RowsAffected()
		c.Records += int(n)
		return int(n), err
	})
}

// deleteEvents raises the event cutoff (it never goes back) and deletes
// events older than it. The trigger on events refuses anything newer (S3).
func (s *Store) deleteEvents(ctx context.Context, p RetainPolicy, now time.Time, c *RetainCounts) error {
	cutoff := ms(now.Add(-p.EventRetention))
	if err := s.tx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `UPDATE event_retention SET cutoff = MAX(cutoff, ?) WHERE id = 1`, cutoff)
		return err
	}); err != nil {
		return fmt.Errorf("raise event cutoff: %w", err)
	}
	return s.repeat(ctx, func(tx *sql.Tx) (int, error) {
		res, err := tx.ExecContext(ctx, `
			DELETE FROM events WHERE seq IN (
				SELECT seq FROM events
				WHERE time < (SELECT cutoff FROM event_retention WHERE id = 1)
				ORDER BY seq LIMIT ?)`, batch)
		if err != nil {
			return 0, fmt.Errorf("delete events: %w", err)
		}
		n, err := res.RowsAffected()
		c.Events += int(n)
		return int(n), err
	})
}

// EventCutoff returns the current event retention cutoff.
func (s *Store) EventCutoff(ctx context.Context) (time.Time, error) {
	var cutoff int64
	if err := s.db.QueryRowContext(ctx, `SELECT cutoff FROM event_retention WHERE id = 1`).Scan(&cutoff); err != nil {
		return time.Time{}, fmt.Errorf("store: event cutoff: %w", err)
	}
	return time.UnixMilli(cutoff), nil
}

// MailIDsByAddress returns the ids of mails that have addr as a recipient or
// as envelope sender, newest first, at most limit. normalized is the query
// in its normalized form (addrhash.Normalize), matched against addresses not
// pseudonymized yet; hashes are the query hashed with every hash key
// (addrhash.Hasher.Lookup), matched against pseudonymized ones.
func (s *Store) MailIDsByAddress(ctx context.Context, normalized string, hashes []string, limit int) ([]string, error) {
	if normalized == "" {
		return nil, nil
	}
	in := strings.TrimSuffix(strings.Repeat("?,", len(hashes)), ",")
	args := []any{normalized}
	for _, h := range hashes {
		args = append(args, h)
	}
	match := `lower(trim(%[1]s)) = ?`
	if len(hashes) > 0 {
		match += ` OR %[1]s IN (` + in + `)`
	}
	q := `SELECT id FROM (
			SELECT mail_id AS id FROM recipients WHERE ` + fmt.Sprintf(match, "address") + `
			UNION
			SELECT id FROM mails WHERE ` + fmt.Sprintf(match, "envelope_from") + `)
		ORDER BY id DESC LIMIT ?`
	args = append(args, args...)
	args = append(args, limit)
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("store: lookup by address: %w", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("store: lookup by address: %w", err)
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// MailRecord is what the store keeps about a mail besides its deliveries.
type MailRecord struct {
	ID              string
	ReceivedAt      time.Time
	Sender          string
	EnvelopeFrom    string // plain, or a keyed hash after pseudonymization
	Recipients      []string
	MessageID       string
	Ref             string
	Size            int
	Params          Params
	HasRaw          bool
	RawPurgedAt     time.Time // zero while the message is stored
	PseudonymizedAt time.Time // zero while addresses are plain
	SealedRawBytes  int       // length of the stored blob
	// Audit is where the mail came from (D65); empty for mails stored
	// before version 9.
	Audit MailAudit
}

// ErrNotFound is returned for a mail id that does not exist.
var ErrNotFound = errors.New("store: not found")

// Mail returns the record of one mail.
func (s *Store) Mail(ctx context.Context, id string) (MailRecord, error) {
	var m MailRecord
	var received int64
	var messageID, ref sql.NullString
	var purged, pseudo sql.NullInt64
	var rawLen, declared sql.NullInt64
	var body sql.NullString
	var keyID, keySource, client, tlsVersion, tlsCipher, ehlo, mech sql.NullString
	err := s.db.QueryRowContext(ctx, `
		SELECT id, received_at, sender, envelope_from, message_id, ref, size, length(raw), raw_purged_at, pseudonymized_at,
			body, smtputf8, declared_size, key_id, key_source, client_ip, tls_version, tls_cipher, ehlo, auth_mech
		FROM mails WHERE id = ?`, id).
		Scan(&m.ID, &received, &m.Sender, &m.EnvelopeFrom, &messageID, &ref, &m.Size, &rawLen, &purged, &pseudo,
			&body, &m.Params.SMTPUTF8, &declared, &keyID, &keySource, &client, &tlsVersion, &tlsCipher, &ehlo, &mech)
	if errors.Is(err, sql.ErrNoRows) {
		return m, ErrNotFound
	}
	if err != nil {
		return m, fmt.Errorf("store: mail %s: %w", id, err)
	}
	m.ReceivedAt = time.UnixMilli(received)
	m.MessageID, m.Ref = messageID.String, ref.String
	m.Params.Body, m.Params.DeclaredSize = body.String, declared.Int64
	m.Audit = MailAudit{KeyID: keyID.String, KeySource: keySource.String, ClientIP: client.String,
		TLSVersion: tlsVersion.String, TLSCipher: tlsCipher.String, EHLO: ehlo.String, AuthMech: mech.String}
	m.HasRaw, m.SealedRawBytes = rawLen.Valid, int(rawLen.Int64)
	if purged.Valid {
		m.RawPurgedAt = time.UnixMilli(purged.Int64)
	}
	if pseudo.Valid {
		m.PseudonymizedAt = time.UnixMilli(pseudo.Int64)
	}
	m.Recipients, err = queryStrings(ctx, s.db, `SELECT address FROM recipients WHERE mail_id = ? ORDER BY position`, id)
	if err != nil {
		return m, fmt.Errorf("store: mail %s: %w", id, err)
	}
	return m, nil
}

type querier interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

func queryStrings(ctx context.Context, q querier, query string, args ...any) ([]string, error) {
	rows, err := q.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var v string
		if err := rows.Scan(&v); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}
