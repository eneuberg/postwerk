package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/eneuberg/postwerk/internal/redact"
)

// ExpireBatch bounds the deliveries one ExpireQueued call fails.
const ExpireBatch = 200

// Expiry selects the queued recipients that ExpireQueued fails.
type Expiry struct {
	Target string
	// ReceivedBefore: mails received at or before it have used up their
	// retry window.
	ReceivedBefore time.Time
	At             time.Time
	// Kind, Level and Text of the event written per recipient.
	Kind  string
	Level Level
	Text  string
}

// ExpireQueued fails the queued recipients of up to ExpireBatch deliveries
// of the target whose mail was received at or before e.ReceivedBefore, one
// event per recipient, in one transaction. A worker whose target is down
// claims nothing, so this is how the retry window still ends for its mails
// (D45). It returns the number of deliveries changed.
func (s *Store) ExpireQueued(ctx context.Context, e Expiry) (int, error) {
	text := redact.Text(e.Text)
	n := 0
	err := s.tx(ctx, func(tx *sql.Tx) error {
		type due struct {
			mailID   string
			attempts int
		}
		var list []due
		rows, err := tx.QueryContext(ctx, `
			SELECT d.mail_id, d.attempts FROM deliveries d JOIN mails m ON m.id = d.mail_id
			WHERE d.target = ? AND d.next_attempt_at IS NOT NULL AND m.received_at <= ?
			ORDER BY m.received_at, d.mail_id LIMIT ?`, e.Target, ms(e.ReceivedBefore), ExpireBatch)
		if err != nil {
			return err
		}
		for rows.Next() {
			var d due
			if err := rows.Scan(&d.mailID, &d.attempts); err != nil {
				rows.Close()
				return err
			}
			list = append(list, d)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		for _, d := range list {
			positions, err := queryInts(ctx, tx, `SELECT position FROM delivery_recipients
				WHERE mail_id = ? AND target = ? AND status = 'queued' ORDER BY position`, d.mailID, e.Target)
			if err != nil {
				return err
			}
			for _, p := range positions {
				if _, err := tx.ExecContext(ctx, `
					UPDATE delivery_recipients SET status = 'failed', last_code = NULL, last_text = ?, updated_at = ?
					WHERE mail_id = ? AND target = ? AND position = ? AND status = 'queued'`,
					nullStr(text), ms(e.At), d.mailID, e.Target, p); err != nil {
					return err
				}
				if err := insertEvent(ctx, tx, Event{Time: e.At, MailID: d.mailID, Target: e.Target,
					Recipient: Position(p), Kind: e.Kind, Level: e.Level, Attempt: d.attempts, Text: text}); err != nil {
					return err
				}
			}
			if _, err := tx.ExecContext(ctx, `
				UPDATE deliveries SET next_attempt_at = NULL, last_code = NULL, last_text = ?, updated_at = ?
				WHERE mail_id = ? AND target = ?`, nullStr(text), ms(e.At), d.mailID, e.Target); err != nil {
				return err
			}
			n++
		}
		return nil
	})
	if err != nil {
		return 0, fmt.Errorf("store: expire queued for %s: %w", e.Target, err)
	}
	if n > 0 && s.opts.OnEvent != nil {
		s.opts.OnEvent()
	}
	return n, nil
}

func queryInts(ctx context.Context, tx *sql.Tx, q string, args ...any) ([]int, error) {
	rows, err := tx.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []int
	for rows.Next() {
		var v int
		if err := rows.Scan(&v); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}
