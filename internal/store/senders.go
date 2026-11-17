package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Senders managed in the database (D62, D63). The rules about names, keys
// and policies are internal/senders'; the store keeps the rows and writes
// each change together with its event.

// Event kinds of managed senders. Actor is who made the change, Sender the
// sender changed; the text names the key or summarizes the policy.
const (
	KindSenderCreated       = "sender.created"
	KindSenderRotated       = "sender.rotated"
	KindSenderRevoked       = "sender.revoked"
	KindSenderPolicyChanged = "sender.policy_changed"
)

// SenderPolicy is a policy as stored; internal/senders.Policy checks it.
type SenderPolicy struct {
	AllowedFrom    []string
	MaxPerMinute   int
	MaxRcptPerHour int
}

// SenderKey is one key of a managed sender. Digest is the SHA-256 of the
// key's secret part.
type SenderKey struct {
	ID        string
	Sender    string
	Digest    []byte
	CreatedAt time.Time
	CreatedBy string
	ExpiresAt time.Time // zero: none
	RevokedAt time.Time // zero: active
	RevokedBy string
}

// SenderRow is a managed sender with all its keys, revoked ones included.
type SenderRow struct {
	Name      string
	Policy    SenderPolicy
	CreatedAt time.Time
	CreatedBy string
	UpdatedAt time.Time
	UpdatedBy string
	Keys      []SenderKey
}

// NewSenderKey is a key to add.
type NewSenderKey struct {
	ID        string
	Digest    []byte
	ExpiresAt time.Time // zero: none
}

// Errors of the sender changes.
var (
	ErrSenderExists = errors.New("store: sender exists")
	ErrNoSender     = errors.New("store: no such sender")
	ErrKeyExists    = errors.New("store: key id exists")
	ErrNoSenderKey  = errors.New("store: no such key")
	ErrKeyRevoked   = errors.New("store: key already revoked")
	ErrTooManyKeys  = errors.New("store: too many active keys")
)

// Senders returns every managed sender with its keys, by name.
func (s *Store) Senders(ctx context.Context) ([]SenderRow, error) {
	var out []SenderRow
	err := s.tx(ctx, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, `SELECT name, allowed_from, max_per_minute, max_rcpt_per_hour,
			created_at, created_by, updated_at, updated_by FROM senders ORDER BY name`)
		if err != nil {
			return err
		}
		idx := map[string]int{}
		for rows.Next() {
			var r SenderRow
			var from string
			var created, updated int64
			if err := rows.Scan(&r.Name, &from, &r.Policy.MaxPerMinute, &r.Policy.MaxRcptPerHour,
				&created, &r.CreatedBy, &updated, &r.UpdatedBy); err != nil {
				rows.Close()
				return err
			}
			if from != "" {
				r.Policy.AllowedFrom = strings.Split(from, ",")
			}
			r.CreatedAt, r.UpdatedAt = time.UnixMilli(created), time.UnixMilli(updated)
			idx[r.Name] = len(out)
			out = append(out, r)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		rows, err = tx.QueryContext(ctx, `SELECT id, sender, digest, created_at, created_by, expires_at, revoked_at, revoked_by
			FROM sender_keys ORDER BY created_at DESC, id`)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var k SenderKey
			var created int64
			var expires, revoked sql.NullInt64
			var revokedBy sql.NullString
			if err := rows.Scan(&k.ID, &k.Sender, &k.Digest, &created, &k.CreatedBy, &expires, &revoked, &revokedBy); err != nil {
				return err
			}
			k.CreatedAt, k.RevokedBy = time.UnixMilli(created), revokedBy.String
			if expires.Valid {
				k.ExpiresAt = time.UnixMilli(expires.Int64)
			}
			if revoked.Valid {
				k.RevokedAt = time.UnixMilli(revoked.Int64)
			}
			if i, ok := idx[k.Sender]; ok {
				out[i].Keys = append(out[i].Keys, k)
			}
		}
		return rows.Err()
	})
	if err != nil {
		return nil, fmt.Errorf("store: list senders: %w", err)
	}
	return out, nil
}

// CreateSender adds a sender, its first key and a sender.created event in
// one transaction.
func (s *Store) CreateSender(ctx context.Context, name string, p SenderPolicy, k NewSenderKey, actor string, at time.Time) error {
	err := s.eventTx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `INSERT INTO senders (name, allowed_from, max_per_minute, max_rcpt_per_hour,
			created_at, created_by, updated_at, updated_by) VALUES (?, ?, ?, ?, ?, ?, ?, ?) ON CONFLICT (name) DO NOTHING`,
			name, strings.Join(p.AllowedFrom, ","), p.MaxPerMinute, p.MaxRcptPerHour, ms(at), actor, ms(at), actor)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return ErrSenderExists
		}
		if err := insertSenderKey(ctx, tx, name, k, actor, at); err != nil {
			return err
		}
		return insertEvent(ctx, tx, Event{Time: at, Kind: KindSenderCreated, Level: Info, Sender: name, Actor: actor,
			Text: "key=" + k.ID})
	})
	if err != nil {
		return fmt.Errorf("store: create sender: %w", err)
	}
	return nil
}

func insertSenderKey(ctx context.Context, tx *sql.Tx, sender string, k NewSenderKey, actor string, at time.Time) error {
	var expires any
	if !k.ExpiresAt.IsZero() {
		expires = ms(k.ExpiresAt)
	}
	res, err := tx.ExecContext(ctx, `INSERT INTO sender_keys (id, sender, digest, created_at, created_by, expires_at)
		VALUES (?, ?, ?, ?, ?, ?) ON CONFLICT (id) DO NOTHING`, k.ID, sender, k.Digest, ms(at), actor, expires)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrKeyExists
	}
	return nil
}

// AddSenderKey adds a key to a sender (a rotation) and writes
// sender.rotated. It refuses with ErrTooManyKeys while the sender has
// maxActive keys that are neither revoked nor expired at at.
func (s *Store) AddSenderKey(ctx context.Context, name string, k NewSenderKey, maxActive int, actor string, at time.Time) error {
	err := s.eventTx(ctx, func(tx *sql.Tx) error {
		var exists bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM senders WHERE name = ?)`, name).Scan(&exists); err != nil {
			return err
		}
		if !exists {
			return ErrNoSender
		}
		var active int
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM sender_keys WHERE sender = ? AND revoked_at IS NULL
			AND (expires_at IS NULL OR expires_at > ?)`, name, ms(at)).Scan(&active); err != nil {
			return err
		}
		if active >= maxActive {
			return ErrTooManyKeys
		}
		if err := insertSenderKey(ctx, tx, name, k, actor, at); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE senders SET updated_at = ?, updated_by = ? WHERE name = ?`,
			ms(at), actor, name); err != nil {
			return err
		}
		return insertEvent(ctx, tx, Event{Time: at, Kind: KindSenderRotated, Level: Info, Sender: name, Actor: actor,
			Text: "key=" + k.ID})
	})
	if err != nil {
		return fmt.Errorf("store: add sender key: %w", err)
	}
	return nil
}

// RevokeSenderKey revokes one key and writes sender.revoked; it returns the
// key's sender.
func (s *Store) RevokeSenderKey(ctx context.Context, id, actor string, at time.Time) (string, error) {
	var sender string
	err := s.eventTx(ctx, func(tx *sql.Tx) error {
		var revoked sql.NullInt64
		err := tx.QueryRowContext(ctx, `SELECT sender, revoked_at FROM sender_keys WHERE id = ?`, id).Scan(&sender, &revoked)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNoSenderKey
		}
		if err != nil {
			return err
		}
		if revoked.Valid {
			return ErrKeyRevoked
		}
		if _, err := tx.ExecContext(ctx, `UPDATE sender_keys SET revoked_at = ?, revoked_by = ? WHERE id = ?`,
			ms(at), actor, id); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE senders SET updated_at = ?, updated_by = ? WHERE name = ?`,
			ms(at), actor, sender); err != nil {
			return err
		}
		return insertEvent(ctx, tx, Event{Time: at, Kind: KindSenderRevoked, Level: Info, Sender: sender, Actor: actor,
			Text: "key=" + id})
	})
	if err != nil {
		return "", fmt.Errorf("store: revoke sender key: %w", err)
	}
	return sender, nil
}

// SetSenderPolicy replaces a sender's policy and writes
// sender.policy_changed with summary as text (the patterns themselves look
// like addresses and are not written into events).
func (s *Store) SetSenderPolicy(ctx context.Context, name string, p SenderPolicy, summary, actor string, at time.Time) error {
	err := s.eventTx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `UPDATE senders SET allowed_from = ?, max_per_minute = ?, max_rcpt_per_hour = ?,
			updated_at = ?, updated_by = ? WHERE name = ?`,
			strings.Join(p.AllowedFrom, ","), p.MaxPerMinute, p.MaxRcptPerHour, ms(at), actor, name)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return ErrNoSender
		}
		return insertEvent(ctx, tx, Event{Time: at, Kind: KindSenderPolicyChanged, Level: Info, Sender: name, Actor: actor,
			Text: summary})
	})
	if err != nil {
		return fmt.Errorf("store: set sender policy: %w", err)
	}
	return nil
}

// SenderUse is when one credential last logged in. KeyID is empty for a
// password line.
type SenderUse struct {
	Source string // "file" or "db"
	Sender string
	KeyID  string
	At     time.Time
}

// TouchSenderUsage records logins in one transaction; a time only ever
// moves forward.
func (s *Store) TouchSenderUsage(ctx context.Context, uses []SenderUse) error {
	if len(uses) == 0 {
		return nil
	}
	err := s.tx(ctx, func(tx *sql.Tx) error {
		for _, u := range uses {
			if _, err := tx.ExecContext(ctx, `INSERT INTO sender_usage (source, sender, key_id, last_used_at)
				VALUES (?, ?, ?, ?) ON CONFLICT (source, sender, key_id)
				DO UPDATE SET last_used_at = max(last_used_at, excluded.last_used_at)`,
				u.Source, u.Sender, u.KeyID, ms(u.At)); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("store: sender usage: %w", err)
	}
	return nil
}

// SenderUsage returns every recorded login time.
func (s *Store) SenderUsage(ctx context.Context) ([]SenderUse, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT source, sender, key_id, last_used_at FROM sender_usage
		ORDER BY sender, source, key_id`)
	if err != nil {
		return nil, fmt.Errorf("store: sender usage: %w", err)
	}
	defer rows.Close()
	var out []SenderUse
	for rows.Next() {
		var u SenderUse
		var at int64
		if err := rows.Scan(&u.Source, &u.Sender, &u.KeyID, &at); err != nil {
			return nil, fmt.Errorf("store: sender usage: %w", err)
		}
		u.At = time.UnixMilli(at)
		out = append(out, u)
	}
	return out, rows.Err()
}

// MailsPerSender counts the mails received in [from, to) per sender.
func (s *Store) MailsPerSender(ctx context.Context, from, to time.Time) (map[string]int, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT sender, count(*) FROM mails WHERE received_at >= ? AND received_at < ?
		GROUP BY sender`, ms(from), ms(to))
	if err != nil {
		return nil, fmt.Errorf("store: mails per sender: %w", err)
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var name string
		var n int
		if err := rows.Scan(&name, &n); err != nil {
			return nil, fmt.Errorf("store: mails per sender: %w", err)
		}
		out[name] = n
	}
	return out, rows.Err()
}

// SenderActivity is what one sender did since a time.
type SenderActivity struct {
	// Mails received, and their recipients that are failed (at any
	// target).
	Mails            int
	FailedRecipients int
	// Counts from the folded ingress events (D51): refused logins naming
	// the sender, refused commands, and commands refused by a rate limit.
	AuthFailures int
	Rejected     int
	RateLimited  int
}

// countRe reads the count of a folded event's text: "failures=3",
// "reason=too_large rejected=2", "limit=max_per_minute limited=5".
var countRe = regexp.MustCompile(`(?:failures|rejected|limited)=([0-9]+)`)

// Event kinds whose counts SenderActivitySince adds up. They are ingress's
// (internal/ingress); the strings are part of the event contract.
const (
	kindIngressAuthFailed  = "ingress.auth_failed"
	kindIngressRejected    = "ingress.rejected"
	kindIngressRateLimited = "ingress.rate_limited"
)

// SenderActivitySince returns the activity of every sender that has some
// since the time, by name.
func (s *Store) SenderActivitySince(ctx context.Context, since time.Time) (map[string]*SenderActivity, error) {
	out := map[string]*SenderActivity{}
	get := func(name string) *SenderActivity {
		if out[name] == nil {
			out[name] = &SenderActivity{}
		}
		return out[name]
	}
	err := s.tx(ctx, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, `SELECT m.sender, count(*),
				(SELECT count(*) FROM delivery_recipients dr JOIN mails x ON x.id = dr.mail_id
				 WHERE x.sender = m.sender AND x.received_at >= ? AND dr.status = 'failed')
			FROM mails m WHERE m.received_at >= ? GROUP BY m.sender`, ms(since), ms(since))
		if err != nil {
			return err
		}
		for rows.Next() {
			var name string
			var mails, failed int
			if err := rows.Scan(&name, &mails, &failed); err != nil {
				rows.Close()
				return err
			}
			a := get(name)
			a.Mails, a.FailedRecipients = mails, failed
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		rows, err = tx.QueryContext(ctx, `SELECT sender, kind, text FROM events
			WHERE time >= ? AND kind IN (?, ?, ?) AND sender IS NOT NULL AND sender != ''`,
			ms(since), kindIngressAuthFailed, kindIngressRejected, kindIngressRateLimited)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var name, kind string
			var text sql.NullString
			if err := rows.Scan(&name, &kind, &text); err != nil {
				return err
			}
			n := 1
			if m := countRe.FindStringSubmatch(text.String); m != nil {
				n, _ = strconv.Atoi(m[1])
			}
			a := get(name)
			switch kind {
			case kindIngressAuthFailed:
				a.AuthFailures += n
			case kindIngressRejected:
				a.Rejected += n
			case kindIngressRateLimited:
				a.RateLimited += n
			}
		}
		return rows.Err()
	})
	if err != nil {
		return nil, fmt.Errorf("store: sender activity: %w", err)
	}
	return out, nil
}

// LastBySender returns, per sender, the time of the newest event of kind
// since the time. The volume signal uses it to pick up its state after a
// restart.
func (s *Store) LastBySender(ctx context.Context, kind string, since time.Time) (map[string]time.Time, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT sender, max(time) FROM events
		WHERE kind = ? AND time >= ? AND sender IS NOT NULL GROUP BY sender`, kind, ms(since))
	if err != nil {
		return nil, fmt.Errorf("store: last by sender: %w", err)
	}
	defer rows.Close()
	out := map[string]time.Time{}
	for rows.Next() {
		var name string
		var at int64
		if err := rows.Scan(&name, &at); err != nil {
			return nil, fmt.Errorf("store: last by sender: %w", err)
		}
		out[name] = time.UnixMilli(at)
	}
	return out, rows.Err()
}
