package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"time"
)

// Resolution is how an admin closed a recipient (D53). It is stored next to
// the status, which it implies: Confirmed recipients are handed_off,
// GivenUp recipients are failed. The derived summaries read the status only.
type Resolution string

// Resolutions.
const (
	// Confirmed: the admin checked at the target that it has the mail.
	Confirmed Resolution = "confirmed"
	// GivenUp: nobody will send it any more.
	GivenUp Resolution = "given_up"
)

// Event kinds of the admin actions. Actor names the account or API token.
const (
	// KindDeliveryClosed: an admin closed this recipient; the text holds
	// the resolution, the status before and where it came from (ui, api),
	// and the admin's note, redacted.
	KindDeliveryClosed = "delivery.closed"
	// KindDeliveryResend: an admin queued this recipient again; the text
	// holds the status before and where it came from.
	KindDeliveryResend = "delivery.resend"
)

// Why a close or resend is not possible. They are checked in the same
// transaction as the change, so two admins, or an admin and a worker,
// cannot both act on one recipient.
var (
	ErrNoDelivery = errors.New("store: no such delivery")
	// ErrInProgress: a worker is sending this delivery right now.
	ErrInProgress = errors.New("store: the delivery is being sent")
	// ErrNothingToClose: no recipient is queued, unknown or failed
	// without a resolution.
	ErrNothingToClose = errors.New("store: nothing to close")
	// ErrConfirmQueued: a queued recipient was never handed to the target,
	// so it cannot be confirmed; it can only be given up.
	ErrConfirmQueued = errors.New("store: a queued recipient cannot be confirmed")
	// ErrNothingToResend: no recipient is failed (and not closed) or
	// unknown.
	ErrNothingToResend = errors.New("store: nothing to resend")
	// ErrRawGone: the stored message was removed (hand-off, retention or
	// a close), so nothing can be sent again.
	ErrRawGone = errors.New("store: the stored message was removed")
	// ErrDuplicateRisk: an unknown recipient may already have the mail;
	// resending it needs the admin's explicit consent.
	ErrDuplicateRisk = errors.New("store: resending an unknown recipient may produce a duplicate")
)

// CloseCheck returns the positions a close with resolution res would change
// in a delivery with these recipients, or why it is not possible. An empty
// res checks the close as such (as GivenUp, which every open recipient
// allows).
func CloseCheck(rs []RecipientState, res Resolution) ([]int, error) {
	var out []int
	queued := false
	for _, r := range rs {
		switch {
		case r.Status == Sending:
			return nil, ErrInProgress
		case r.Status == Queued:
			queued = true
			out = append(out, r.Position)
		case r.Status == Unknown, r.Status == Failed && r.Resolution == "":
			out = append(out, r.Position)
		}
	}
	switch {
	case len(out) == 0:
		return nil, ErrNothingToClose
	case res == Confirmed && queued:
		return nil, ErrConfirmQueued
	}
	return out, nil
}

// ResendCheck returns the positions a resend would queue again, whether one
// of them is unknown (a resend may then produce a duplicate), or why it is
// not possible. hasRaw tells whether the stored message still exists.
func ResendCheck(rs []RecipientState, hasRaw bool) (positions []int, unknown bool, err error) {
	for _, r := range rs {
		switch {
		case r.Status == Sending:
			return nil, false, ErrInProgress
		case r.Status == Unknown:
			unknown = true
			positions = append(positions, r.Position)
		case r.Status == Failed && r.Resolution == "":
			positions = append(positions, r.Position)
		}
	}
	switch {
	case len(positions) == 0:
		return nil, false, ErrNothingToResend
	case !hasRaw:
		return nil, false, ErrRawGone
	}
	return positions, unknown, nil
}

// Close is an admin's close of one delivery.
type Close struct {
	MailID, Target string
	Resolution     Resolution
	// Note is the admin's short note; it is redacted like every text.
	Note string
	// Actor is the account or token name; Via is "ui" or "api".
	Actor, Via string
	At         time.Time
}

// CloseDelivery closes every open recipient of one delivery: queued,
// unknown, or failed without a resolution (D53). Confirmed makes them
// handed_off, GivenUp failed, each with the resolution and a
// delivery.closed event, in one transaction. An unknown recipient closed
// this way is no longer touched by retention's raw expiry. The same
// transaction removes the stored message if no recipient of the mail may
// still need it (queued, sending, unknown, or failed without a
// resolution). It returns the positions closed.
func (s *Store) CloseDelivery(ctx context.Context, c Close) ([]int, error) {
	switch c.Resolution {
	case Confirmed, GivenUp:
	default:
		return nil, fmt.Errorf("store: close: invalid resolution %q", c.Resolution)
	}
	var positions []int
	err := s.eventTx(ctx, func(tx *sql.Tx) error {
		rs, err := recipientStates(ctx, tx, c.MailID, c.Target)
		if err != nil {
			return err
		}
		positions, err = CloseCheck(rs, c.Resolution)
		if err != nil {
			return err
		}
		status := HandedOff
		if c.Resolution == GivenUp {
			status = Failed
		}
		for _, r := range rs {
			if !slices.Contains(positions, r.Position) {
				continue
			}
			if _, err := tx.ExecContext(ctx, `
				UPDATE delivery_recipients SET status = ?, resolution = ?, updated_at = ?
				WHERE mail_id = ? AND target = ? AND position = ?`,
				string(status), string(c.Resolution), ms(c.At), c.MailID, c.Target, r.Position); err != nil {
				return fmt.Errorf("close recipient %d: %w", r.Position, err)
			}
			text := "resolution=" + string(c.Resolution) + " was=" + string(r.Status) + " via=" + c.Via
			if c.Note != "" {
				text += ": " + c.Note
			}
			if err := insertEvent(ctx, tx, Event{
				Time: c.At, MailID: c.MailID, Target: c.Target, Recipient: Position(r.Position),
				Kind: KindDeliveryClosed, Level: Info, Text: text, Actor: c.Actor,
			}); err != nil {
				return err
			}
		}
		// Every queued recipient is closed now: nothing is due.
		if _, err := tx.ExecContext(ctx, `
			UPDATE deliveries SET next_attempt_at = NULL, updated_at = ? WHERE mail_id = ? AND target = ?`,
			ms(c.At), c.MailID, c.Target); err != nil {
			return fmt.Errorf("unschedule: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `
			UPDATE mails SET raw = NULL, raw_purged_at = ?
			WHERE id = ? AND raw IS NOT NULL AND NOT EXISTS (
				SELECT 1 FROM delivery_recipients WHERE mail_id = ? AND (status IN ('queued', 'sending', 'unknown')
					OR (status = 'failed' AND resolution IS NULL)))`,
			ms(c.At), c.MailID, c.MailID); err != nil {
			return fmt.Errorf("purge raw: %w", err)
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("store: close %s/%s: %w", c.MailID, c.Target, err)
	}
	return positions, nil
}

// Resend is an admin's resend of one delivery.
type Resend struct {
	MailID, Target string
	// AcceptDuplicate is the admin's consent that an unknown recipient may
	// get the mail twice.
	AcceptDuplicate bool
	Actor, Via      string
	At              time.Time
}

// ResendDelivery queues every failed (not closed) and unknown recipient of
// one delivery again, due at once, with a delivery.resend event each, in one
// transaction. It needs the stored message: retention is never extended for
// a resend, and once the message is gone, ErrRawGone. While a worker sends
// the delivery, ErrInProgress, so a recipient is never in two attempts at
// once; a second resend finds the recipients queued and gets
// ErrNothingToResend. The retry window stays the one counted from the
// mail's arrival. It returns the positions queued.
func (s *Store) ResendDelivery(ctx context.Context, r Resend) ([]int, error) {
	var positions []int
	err := s.eventTx(ctx, func(tx *sql.Tx) error {
		rs, err := recipientStates(ctx, tx, r.MailID, r.Target)
		if err != nil {
			return err
		}
		var hasRaw bool
		if err := tx.QueryRowContext(ctx, `SELECT raw IS NOT NULL FROM mails WHERE id = ?`, r.MailID).
			Scan(&hasRaw); err != nil {
			return fmt.Errorf("load mail: %w", err)
		}
		var unknown bool
		positions, unknown, err = ResendCheck(rs, hasRaw)
		if err != nil {
			return err
		}
		if unknown && !r.AcceptDuplicate {
			return ErrDuplicateRisk
		}
		for _, rc := range rs {
			if !slices.Contains(positions, rc.Position) {
				continue
			}
			if _, err := tx.ExecContext(ctx, `
				UPDATE delivery_recipients SET status = 'queued', updated_at = ?
				WHERE mail_id = ? AND target = ? AND position = ?`,
				ms(r.At), r.MailID, r.Target, rc.Position); err != nil {
				return fmt.Errorf("queue recipient %d: %w", rc.Position, err)
			}
			if err := insertEvent(ctx, tx, Event{
				Time: r.At, MailID: r.MailID, Target: r.Target, Recipient: Position(rc.Position),
				Kind: KindDeliveryResend, Level: Info, Actor: r.Actor,
				Text: "was=" + string(rc.Status) + " via=" + r.Via,
			}); err != nil {
				return err
			}
		}
		if _, err := tx.ExecContext(ctx, `
			UPDATE deliveries SET next_attempt_at = ?, updated_at = ? WHERE mail_id = ? AND target = ?`,
			ms(r.At), ms(r.At), r.MailID, r.Target); err != nil {
			return fmt.Errorf("schedule: %w", err)
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("store: resend %s/%s: %w", r.MailID, r.Target, err)
	}
	return positions, nil
}

// recipientStates reads the recipients of one delivery, or ErrNoDelivery.
func recipientStates(ctx context.Context, tx *sql.Tx, mailID, target string) ([]RecipientState, error) {
	rows, err := tx.QueryContext(ctx, `
		SELECT position, status, resolution FROM delivery_recipients
		WHERE mail_id = ? AND target = ? ORDER BY position`, mailID, target)
	if err != nil {
		return nil, fmt.Errorf("load recipients: %w", err)
	}
	defer rows.Close()
	var out []RecipientState
	for rows.Next() {
		var r RecipientState
		var status string
		var res sql.NullString
		if err := rows.Scan(&r.Position, &status, &res); err != nil {
			return nil, fmt.Errorf("load recipients: %w", err)
		}
		r.Status, r.Resolution = Status(status), Resolution(res.String)
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("load recipients: %w", err)
	}
	if len(out) == 0 {
		return nil, ErrNoDelivery
	}
	return out, nil
}
