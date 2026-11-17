package senders

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"
)

func TestRevokeAll(t *testing.T) {
	ctx := context.Background()
	db, err := NewDBSource(ctx, openStore(t), nil)
	if err != nil {
		t.Fatal(err)
	}
	first, err := db.Create(ctx, CreateRequest{Name: "shop", Actor: "ops"})
	if err != nil {
		t.Fatal(err)
	}
	second, err := db.Rotate(ctx, "shop", time.Time{}, "ops")
	if err != nil {
		t.Fatal(err)
	}
	// One of them is revoked meanwhile: skipped, not an error.
	if err := db.Revoke(ctx, first.KeyID, "alice"); err != nil {
		t.Fatal(err)
	}
	done, err := RevokeAll(ctx, db, []string{first.KeyID, second.KeyID}, "ops")
	if err != nil || !slices.Equal(done, []string{second.KeyID}) {
		t.Fatalf("revoked %v, %v", done, err)
	}
	s, _ := db.ByName("shop")
	if len(s.ActiveKeys(time.Now())) != 0 || len(s.Keys) != 2 {
		t.Fatalf("after: %+v", s)
	}
	// An unknown key stops it, with what was done so far.
	third, _ := db.Rotate(ctx, "shop", time.Time{}, "ops")
	done, err = RevokeAll(ctx, db, []string{third.KeyID, "aaaaaaaaaaaa"}, "ops")
	if !errors.Is(err, ErrNoKey) || !slices.Equal(done, []string{third.KeyID}) {
		t.Fatalf("revoked %v, %v", done, err)
	}
}
