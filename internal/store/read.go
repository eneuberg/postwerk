package store

import (
	"context"
	"database/sql"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"
)

// Reads for the UI and the query API (U10, U11, U15). The read logic on top
// of them lives in internal/query; the SQL stays here.

// stateSQL is Summarize as a condition over the counts per status of one
// mail's recipients (s sending, q queued, u unknown, k skipped, f failed,
// h handed_off, n all). It must stay equal to summarizeCounts.
var stateSQL = map[Status]string{
	Sending:   "s > 0",
	Queued:    "s = 0 AND q > 0",
	Unknown:   "s = 0 AND q = 0 AND u > 0",
	Skipped:   "n > 0 AND k = n",
	HandedOff: "s = 0 AND q = 0 AND u = 0 AND k < n AND f = 0",
	Failed:    "s = 0 AND q = 0 AND u = 0 AND f > 0 AND h = 0",
	Partial:   "s = 0 AND q = 0 AND u = 0 AND f > 0 AND h > 0",
}

// MailFilter selects mails for ListMails.
type MailFilter struct {
	// Target limits the derived state to the recipients of this target and
	// leaves out mails without a delivery to it. Empty: all targets.
	Target string
	// States are derived states (Summarize, including Partial); a mail
	// matches if its state is one of them. Empty: any state.
	States []Status
	// From and To bound the received time: From ≤ received < To. Zero
	// values leave that side open.
	From, To time.Time
	// Optional names the optional targets. Without Target, a mail's state
	// is derived from the recipients of its other (required) targets only;
	// a mail with no delivery to a required target uses all of them
	// (design.md section 4, MailStatus).
	Optional []string
	// IDs, if not nil, limits the list to these mails (search results).
	IDs []string
	// Limit (required, > 0) and Offset page through the list, newest first.
	Limit, Offset int
}

// TargetStatus is the derived status of one delivery.
type TargetStatus struct {
	Target string
	Status Status
}

// MailSummary is one row of a mail list.
type MailSummary struct {
	ID         string
	ReceivedAt time.Time
	Sender     string
	Size       int
	Recipients int
	// Status is derived over the recipients of the required targets
	// (MailStatus), or of the filter's target.
	Status Status
	// Targets holds the derived status of every delivery, by target name.
	Targets []TargetStatus
}

// ListMails returns the mails that match f, newest first.
func (s *Store) ListMails(ctx context.Context, f MailFilter) ([]MailSummary, error) {
	if f.Limit <= 0 {
		return nil, fmt.Errorf("store: list mails: limit required")
	}
	if f.IDs != nil && len(f.IDs) == 0 {
		return nil, nil
	}
	var where []string
	var args []any
	join := `LEFT JOIN delivery_recipients dr ON dr.mail_id = m.id`
	switch {
	case f.Target != "":
		join += ` AND dr.target = ?`
		args = append(args, f.Target)
	case len(f.Optional) > 0:
		// Required targets only, unless the mail has none.
		in := placeholders(len(f.Optional))
		join += ` AND (dr.target NOT IN (` + in + `) OR NOT EXISTS (
			SELECT 1 FROM delivery_recipients x WHERE x.mail_id = m.id AND x.target NOT IN (` + in + `)))`
		for range 2 {
			for _, t := range f.Optional {
				args = append(args, t)
			}
		}
	}
	if !f.From.IsZero() {
		where = append(where, `m.received_at >= ?`)
		args = append(args, ms(f.From))
	}
	if !f.To.IsZero() {
		where = append(where, `m.received_at < ?`)
		args = append(args, ms(f.To))
	}
	if f.IDs != nil {
		where = append(where, `m.id IN (`+placeholders(len(f.IDs))+`)`)
		for _, id := range f.IDs {
			args = append(args, id)
		}
	}
	whereSQL := ""
	if len(where) > 0 {
		whereSQL = "WHERE " + strings.Join(where, " AND ")
	}
	var outer []string
	if f.Target != "" {
		outer = append(outer, "n > 0")
	}
	if len(f.States) > 0 {
		var alts []string
		for _, st := range f.States {
			cond, ok := stateSQL[st]
			if !ok {
				return nil, fmt.Errorf("store: list mails: unknown state %q", st)
			}
			alts = append(alts, "("+cond+")")
		}
		outer = append(outer, "("+strings.Join(alts, " OR ")+")")
	}
	outerSQL := ""
	if len(outer) > 0 {
		outerSQL = "WHERE " + strings.Join(outer, " AND ")
	}
	q := `SELECT id, received_at, sender, size, rcpts, s, q, u, k, f, h, n FROM (
			SELECT m.id, m.received_at, m.sender, m.size,
				(SELECT COUNT(*) FROM recipients r WHERE r.mail_id = m.id) AS rcpts,
				COALESCE(SUM(dr.status = 'sending'), 0) AS s,
				COALESCE(SUM(dr.status = 'queued'), 0) AS q,
				COALESCE(SUM(dr.status = 'unknown'), 0) AS u,
				COALESCE(SUM(dr.status = 'skipped'), 0) AS k,
				COALESCE(SUM(dr.status = 'failed'), 0) AS f,
				COALESCE(SUM(dr.status = 'handed_off'), 0) AS h,
				COUNT(dr.mail_id) AS n
			FROM mails m ` + join + ` ` + whereSQL + `
			GROUP BY m.id) ` + outerSQL + `
		ORDER BY received_at DESC, id DESC LIMIT ? OFFSET ?`
	args = append(args, f.Limit, max(f.Offset, 0))

	var out []MailSummary
	// One transaction, so the rows and their per-target statuses are read
	// in the same state.
	err := s.tx(ctx, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, q, args...)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var m MailSummary
			var received int64
			var sending, queued, unknown, skipped, failed, handed, total int
			if err := rows.Scan(&m.ID, &received, &m.Sender, &m.Size, &m.Recipients,
				&sending, &queued, &unknown, &skipped, &failed, &handed, &total); err != nil {
				return err
			}
			m.ReceivedAt = time.UnixMilli(received)
			m.Status = summarizeCounts(map[Status]int{
				Sending: sending, Queued: queued, Unknown: unknown, Skipped: skipped, Failed: failed, HandedOff: handed,
			}, total)
			out = append(out, m)
		}
		if err := rows.Err(); err != nil {
			return err
		}
		rows.Close()
		return targetStatuses(ctx, tx, out)
	})
	if err != nil {
		return nil, fmt.Errorf("store: list mails: %w", err)
	}
	return out, nil
}

// targetStatuses fills in the derived status of every delivery of the mails.
func targetStatuses(ctx context.Context, tx *sql.Tx, mails []MailSummary) error {
	if len(mails) == 0 {
		return nil
	}
	byID := make(map[string]*MailSummary, len(mails))
	args := make([]any, len(mails))
	for i := range mails {
		byID[mails[i].ID] = &mails[i]
		args[i] = mails[i].ID
	}
	rows, err := tx.QueryContext(ctx, `
		SELECT mail_id, target, status, COUNT(*) FROM delivery_recipients
		WHERE mail_id IN (`+placeholders(len(mails))+`)
		GROUP BY mail_id, target, status ORDER BY mail_id, target`, args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	type key struct{ mail, target string }
	counts := map[key]map[Status]int{}
	var order []key
	for rows.Next() {
		var k key
		var status string
		var n int
		if err := rows.Scan(&k.mail, &k.target, &status, &n); err != nil {
			return err
		}
		if counts[k] == nil {
			counts[k] = map[Status]int{}
			order = append(order, k)
		}
		counts[k][Status(status)] += n
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for _, k := range order {
		total := 0
		for _, n := range counts[k] {
			total += n
		}
		m := byID[k.mail]
		m.Targets = append(m.Targets, TargetStatus{Target: k.target, Status: summarizeCounts(counts[k], total)})
	}
	return nil
}

// TargetStats is what the overview shows for one target.
type TargetStats struct {
	Target string
	// Recent counts the recipients of mails received since the given time,
	// by their stored status.
	Recent map[Status]int
	// QueuedMails counts the mails with a recipient queued or sending at
	// this target, of any age.
	QueuedMails int
	// OldestQueued is the received time of the oldest of them; zero if none.
	OldestQueued time.Time
}

// TargetStatsSince returns the statistics of every target that has a
// delivery in the store, ordered by name.
func (s *Store) TargetStatsSince(ctx context.Context, since time.Time) ([]TargetStats, error) {
	byTarget := map[string]*TargetStats{}
	get := func(t string) *TargetStats {
		if byTarget[t] == nil {
			byTarget[t] = &TargetStats{Target: t, Recent: map[Status]int{}}
		}
		return byTarget[t]
	}
	err := s.tx(ctx, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, `
			SELECT dr.target, dr.status, COUNT(*) FROM delivery_recipients dr
			JOIN mails m ON m.id = dr.mail_id
			WHERE m.received_at >= ? GROUP BY dr.target, dr.status`, ms(since))
		if err != nil {
			return err
		}
		for rows.Next() {
			var target, status string
			var n int
			if err := rows.Scan(&target, &status, &n); err != nil {
				rows.Close()
				return err
			}
			get(target).Recent[Status(status)] = n
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		rows, err = tx.QueryContext(ctx, `
			SELECT dr.target, COUNT(DISTINCT dr.mail_id), MIN(m.received_at) FROM delivery_recipients dr
			JOIN mails m ON m.id = dr.mail_id
			WHERE dr.status IN ('queued', 'sending') GROUP BY dr.target`)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var target string
			var n int
			var oldest int64
			if err := rows.Scan(&target, &n, &oldest); err != nil {
				return err
			}
			t := get(target)
			t.QueuedMails, t.OldestQueued = n, time.UnixMilli(oldest)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, fmt.Errorf("store: target stats: %w", err)
	}
	// The distinct targets in the store, also those with nothing recent.
	names, err := queryStrings(ctx, s.db, `SELECT DISTINCT target FROM deliveries ORDER BY target`)
	if err != nil {
		return nil, fmt.Errorf("store: target stats: %w", err)
	}
	for _, n := range names {
		get(n)
	}
	out := make([]TargetStats, 0, len(byTarget))
	for _, n := range slices.Sorted(maps.Keys(byTarget)) {
		out = append(out, *byTarget[n])
	}
	return out, nil
}

// MailIDsByRef returns the ids of mails with this X-Postwerk-Ref, newest
// first, at most limit.
func (s *Store) MailIDsByRef(ctx context.Context, ref string, limit int) ([]string, error) {
	if ref == "" {
		return nil, nil
	}
	ids, err := queryStrings(ctx, s.db,
		`SELECT id FROM mails WHERE ref = ? ORDER BY received_at DESC, id DESC LIMIT ?`, ref, limit)
	if err != nil {
		return nil, fmt.Errorf("store: lookup by ref: %w", err)
	}
	return ids, nil
}

// MailIDsByMessageID returns the ids of mails whose Message-ID is one of
// ids, newest first, at most limit.
func (s *Store) MailIDsByMessageID(ctx context.Context, messageIDs []string, limit int) ([]string, error) {
	if len(messageIDs) == 0 {
		return nil, nil
	}
	args := make([]any, 0, len(messageIDs)+1)
	for _, m := range messageIDs {
		args = append(args, m)
	}
	args = append(args, limit)
	ids, err := queryStrings(ctx, s.db, `SELECT id FROM mails WHERE message_id IN (`+placeholders(len(messageIDs))+`)
		ORDER BY received_at DESC, id DESC LIMIT ?`, args...)
	if err != nil {
		return nil, fmt.Errorf("store: lookup by message id: %w", err)
	}
	return ids, nil
}

// MailEvents returns the events of one mail's history in order, at most
// limit: the relay's events and an admin's close and resend, which change
// the mail's state. Other events caused by people (an Actor, such as
// ui.view) are left out: who looked at a mail is in the event log.
func (s *Store) MailEvents(ctx context.Context, mailID string, limit int) ([]Event, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+eventColumns+` FROM events
		WHERE mail_id = ? AND (actor IS NULL OR kind IN (?, ?)) ORDER BY seq LIMIT ?`,
		mailID, KindDeliveryClosed, KindDeliveryResend, limit)
	if err != nil {
		return nil, fmt.Errorf("store: events of %s: %w", mailID, err)
	}
	out, err := scanEvents(rows)
	if err != nil {
		return nil, fmt.Errorf("store: events of %s: %w", mailID, err)
	}
	return out, nil
}

func placeholders(n int) string {
	return strings.TrimSuffix(strings.Repeat("?,", n), ",")
}
