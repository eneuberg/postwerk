package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// UIAccount is an account of the UI managed in the admin panel (U17). Hash
// is the PBKDF2 value (internal/auth), never the password.
type UIAccount struct {
	Name      string
	Hash      string
	CreatedAt time.Time
	CreatedBy string
	UpdatedAt time.Time
	UpdatedBy string
}

// Errors of the account changes.
var (
	ErrNoAccount     = errors.New("store: no such account")
	ErrAccountExists = errors.New("store: account exists")
	// ErrLastAccount refuses to delete the only account left (U20).
	ErrLastAccount = errors.New("store: the last account cannot be deleted")
)

// UIAccounts returns every account, by name.
func (s *Store) UIAccounts(ctx context.Context) ([]UIAccount, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT name, hash, created_at, created_by, updated_at, updated_by
		FROM ui_accounts ORDER BY name`)
	if err != nil {
		return nil, fmt.Errorf("store: list accounts: %w", err)
	}
	defer rows.Close()
	var out []UIAccount
	for rows.Next() {
		a, err := scanAccount(rows)
		if err != nil {
			return nil, fmt.Errorf("store: list accounts: %w", err)
		}
		out = append(out, a)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list accounts: %w", err)
	}
	return out, nil
}

// UIAccount returns one account, or ErrNoAccount.
func (s *Store) UIAccount(ctx context.Context, name string) (UIAccount, error) {
	row := s.db.QueryRowContext(ctx, `SELECT name, hash, created_at, created_by, updated_at, updated_by
		FROM ui_accounts WHERE name = ?`, name)
	a, err := scanAccount(row)
	if errors.Is(err, sql.ErrNoRows) {
		return UIAccount{}, ErrNoAccount
	}
	if err != nil {
		return UIAccount{}, fmt.Errorf("store: read account: %w", err)
	}
	return a, nil
}

// CountUIAccounts returns the number of accounts.
func (s *Store) CountUIAccounts(ctx context.Context) (int, error) {
	var n int
	if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM ui_accounts`).Scan(&n); err != nil {
		return 0, fmt.Errorf("store: count accounts: %w", err)
	}
	return n, nil
}

// CreateUIAccount adds an account and its ui.account_created event in one
// transaction; actor is the account that made it.
func (s *Store) CreateUIAccount(ctx context.Context, name, hash, actor string, at time.Time) error {
	err := s.eventTx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `INSERT INTO ui_accounts (name, hash, created_at, created_by, updated_at, updated_by)
			VALUES (?, ?, ?, ?, ?, ?) ON CONFLICT (name) DO NOTHING`, name, hash, ms(at), actor, ms(at), actor)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return ErrAccountExists
		}
		return insertEvent(ctx, tx, Event{Time: at, Kind: KindUIAccountCreated, Level: Info, Actor: actor, Text: name})
	})
	if err != nil {
		return fmt.Errorf("store: create account: %w", err)
	}
	return nil
}

// SetUIPassword replaces the hash of an account and writes ui.password_set.
func (s *Store) SetUIPassword(ctx context.Context, name, hash, actor string, at time.Time) error {
	err := s.eventTx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `UPDATE ui_accounts SET hash = ?, updated_at = ?, updated_by = ? WHERE name = ?`,
			hash, ms(at), actor, name)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return ErrNoAccount
		}
		return insertEvent(ctx, tx, Event{Time: at, Kind: KindUIPasswordSet, Level: Info, Actor: actor, Text: name})
	})
	if err != nil {
		return fmt.Errorf("store: set password: %w", err)
	}
	return nil
}

// DeleteUIAccount removes an account and writes ui.account_deleted. The
// last account is never deleted (ErrLastAccount), so the panel cannot lock
// everyone out once the tokens file is gone.
func (s *Store) DeleteUIAccount(ctx context.Context, name, actor string, at time.Time) error {
	err := s.eventTx(ctx, func(tx *sql.Tx) error {
		var n int
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM ui_accounts`).Scan(&n); err != nil {
			return err
		}
		var exists bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM ui_accounts WHERE name = ?)`, name).
			Scan(&exists); err != nil {
			return err
		}
		switch {
		case !exists:
			return ErrNoAccount
		case n <= 1:
			return ErrLastAccount
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM ui_accounts WHERE name = ?`, name); err != nil {
			return err
		}
		return insertEvent(ctx, tx, Event{Time: at, Kind: KindUIAccountDeleted, Level: Info, Actor: actor, Text: name})
	})
	if err != nil {
		return fmt.Errorf("store: delete account: %w", err)
	}
	return nil
}

type scanner interface{ Scan(dest ...any) error }

func scanAccount(r scanner) (UIAccount, error) {
	var a UIAccount
	var created, updated int64
	if err := r.Scan(&a.Name, &a.Hash, &created, &a.CreatedBy, &updated, &a.UpdatedBy); err != nil {
		return UIAccount{}, err
	}
	a.CreatedAt, a.UpdatedAt = time.UnixMilli(created), time.UnixMilli(updated)
	return a, nil
}
