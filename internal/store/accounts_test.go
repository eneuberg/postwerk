package store

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestUIAccounts(t *testing.T) {
	ctx := context.Background()
	s := open(t, t.TempDir(), Options{})
	at := time.UnixMilli(1_000_000)

	if n, err := s.CountUIAccounts(ctx); err != nil || n != 0 {
		t.Fatalf("count on a new store: %d %v", n, err)
	}
	if err := s.CreateUIAccount(ctx, "alice", "hash-a", "ops", at); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateUIAccount(ctx, "alice", "hash-x", "ops", at); !errors.Is(err, ErrAccountExists) {
		t.Fatalf("duplicate: %v", err)
	}
	if err := s.CreateUIAccount(ctx, "bob", "hash-b", "alice", at.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := s.SetUIPassword(ctx, "bob", "hash-b2", "alice", at.Add(2*time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := s.SetUIPassword(ctx, "nobody", "h", "alice", at); !errors.Is(err, ErrNoAccount) {
		t.Fatalf("password of a missing account: %v", err)
	}
	b, err := s.UIAccount(ctx, "bob")
	if err != nil || b.Hash != "hash-b2" || b.CreatedBy != "alice" || b.UpdatedBy != "alice" ||
		!b.UpdatedAt.Equal(at.Add(2*time.Second)) || !b.CreatedAt.Equal(at.Add(time.Second)) {
		t.Fatalf("bob = %+v %v", b, err)
	}
	if _, err := s.UIAccount(ctx, "nobody"); !errors.Is(err, ErrNoAccount) {
		t.Fatalf("missing account: %v", err)
	}
	if err := s.DeleteUIAccount(ctx, "nobody", "alice", at); !errors.Is(err, ErrNoAccount) {
		t.Fatalf("delete missing: %v", err)
	}
	if err := s.DeleteUIAccount(ctx, "alice", "bob", at.Add(3*time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteUIAccount(ctx, "bob", "bob", at); !errors.Is(err, ErrLastAccount) {
		t.Fatalf("delete the last account: %v", err)
	}
	all, err := s.UIAccounts(ctx)
	if err != nil || len(all) != 1 || all[0].Name != "bob" {
		t.Fatalf("accounts = %+v %v", all, err)
	}

	evs, err := s.EventsAfter(ctx, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, e := range evs {
		got = append(got, e.Kind+"/"+e.Actor+"/"+e.Text)
		if strings.Contains(e.Text, "hash") {
			t.Errorf("event carries a hash: %+v", e)
		}
	}
	want := "ui.account_created/ops/alice ui.account_created/alice/bob ui.password_set/alice/bob ui.account_deleted/bob/alice"
	if strings.Join(got, " ") != want {
		t.Fatalf("events = %q, want %q", got, want)
	}
}

func TestMailEventsLeaveOutPeople(t *testing.T) {
	ctx := context.Background()
	s := open(t, t.TempDir(), Options{})
	at := time.UnixMilli(1_000_000)
	if err := s.Accept(ctx, newMail("pw_1", at, "mailjet")); err != nil {
		t.Fatal(err)
	}
	if err := s.Append(ctx, Event{Time: at, MailID: "pw_1", Kind: KindUIView, Level: Info, Actor: "ops"}); err != nil {
		t.Fatal(err)
	}
	evs, err := s.MailEvents(ctx, "pw_1", 100)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range evs {
		if e.Kind == KindUIView {
			t.Fatal("ui.view in the mail's timeline")
		}
	}
	all, _ := s.EventsAfter(ctx, 0, 100)
	if last := all[len(all)-1]; last.Kind != KindUIView || last.MailID != "pw_1" || last.Actor != "ops" || last.Sender == "" {
		t.Fatalf("view event = %+v", last)
	}
}
