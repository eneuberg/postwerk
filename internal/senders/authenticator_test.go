package senders

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/eneuberg/postwerk/internal/auth"
	"github.com/eneuberg/postwerk/internal/store"
)

type usageLog struct {
	mu   sync.Mutex
	seen []Principal
}

func (u *usageLog) Record(p Principal, _ time.Time) {
	u.mu.Lock()
	u.seen = append(u.seen, p)
	u.mu.Unlock()
}

func TestAuthenticate(t *testing.T) {
	ctx := context.Background()
	fileKey, fileID, fileLine := keyLine(t, "app")
	otherKey, _, otherLine := keyLine(t, "other")
	fx := newFixture(t, "legacy:"+testHash()+"\n"+fileLine+"\n"+otherLine+"\napp:policy max_per_minute=7\n")
	dbIssued, err := fx.db.Create(ctx, CreateRequest{Name: "billing", Actor: "ops"})
	if err != nil {
		t.Fatal(err)
	}
	unknownKey, _ := NewKey()
	tamperedDB := dbIssued.Key[:len(dbIssued.Key)-1] + "a"
	if tamperedDB == dbIssued.Key {
		tamperedDB = dbIssued.Key[:len(dbIssued.Key)-1] + "b"
	}
	u := &usageLog{}
	a := NewAuthenticator(fx.reg, AuthOptions{Usage: u})
	for _, tc := range []struct {
		name, user, pass string
		want             Principal // zero: refused
	}{
		{"file key", "app", fileKey, Principal{Sender: "app", Source: KindFile, Method: MethodKey, KeyID: fileID}},
		{"db key", "billing", dbIssued.Key, Principal{Sender: "billing", Source: KindDB, Method: MethodKey, KeyID: dbIssued.KeyID}},
		{"legacy password", "legacy", "secret", Principal{Sender: "legacy", Source: KindFile, Method: MethodPassword}},
		{"legacy cached", "legacy", "secret", Principal{Sender: "legacy", Source: KindFile, Method: MethodPassword}},
		{"wrong password", "legacy", "wrong", Principal{}},
		{"key of another sender", "app", otherKey, Principal{}},
		{"key under the key id", fileID, fileKey, Principal{}},
		{"empty name", "", fileKey, Principal{}},
		{"unknown key", "app", unknownKey, Principal{}},
		{"tampered secret", "billing", tamperedDB, Principal{}},
		{"key as password of a password sender", "legacy", fileKey, Principal{}},
		{"password for a key sender", "app", "secret", Principal{}},
		{"unknown name", "nobody", "secret", Principal{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, err := a.Authenticate(tc.user, tc.pass)
			if tc.want.Sender == "" {
				if !errors.Is(err, ErrBadCredentials) {
					t.Fatalf("err = %v, principal %+v", err, p)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if p.Sender != tc.want.Sender || p.Source != tc.want.Source || p.Method != tc.want.Method || p.KeyID != tc.want.KeyID {
				t.Fatalf("principal %+v, want %+v", p, tc.want)
			}
		})
	}
	p, _ := a.Authenticate("app", fileKey)
	if p.Policy.MaxPerMinute() != 7 {
		t.Fatal("policy missing from the principal")
	}
	if len(u.seen) != 5 {
		t.Fatalf("usage recorded %d logins", len(u.seen))
	}
	if !a.Known("app") || !a.Known("billing") || !a.Known("legacy") || a.Known("nobody") {
		t.Fatal("Known")
	}
}

func TestRotateRevokeExpire(t *testing.T) {
	ctx := context.Background()
	fx := newFixture(t, "")
	now := time.Now()
	clock := func() time.Time { return now }
	fx.db.now = clock
	a := NewAuthenticator(fx.reg, AuthOptions{Now: clock})
	first, err := fx.db.Create(ctx, CreateRequest{Name: "app", Actor: "ops"})
	if err != nil {
		t.Fatal(err)
	}
	p1, err := a.Authenticate("app", first.Key)
	if err != nil {
		t.Fatal(err)
	}
	second, err := fx.db.Rotate(ctx, "app", now.Add(time.Hour), "ops")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fx.db.Rotate(ctx, "app", time.Time{}, "ops"); !errors.Is(err, ErrTooManyKeys) {
		t.Fatalf("third key: %v", err)
	}
	for _, k := range []string{first.Key, second.Key} {
		if _, err := a.Authenticate("app", k); err != nil {
			t.Fatal("a key of the rotation does not work")
		}
	}
	if err := fx.db.Revoke(ctx, first.KeyID, "ops"); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Authenticate("app", first.Key); !errors.Is(err, ErrBadCredentials) {
		t.Fatal("revoked key works")
	}
	if _, ok := a.Refresh(p1); ok {
		t.Fatal("a session of a revoked key survives Refresh")
	}
	p2, err := a.Authenticate("app", second.Key)
	if err != nil {
		t.Fatal("the other key stopped working")
	}
	if err := fx.db.Revoke(ctx, first.KeyID, "ops"); !errors.Is(err, ErrRevoked) {
		t.Fatalf("revoke twice: %v", err)
	}
	if err := fx.db.Revoke(ctx, "aaaaaaaaaaaa", "ops"); !errors.Is(err, ErrNoKey) {
		t.Fatalf("revoke unknown: %v", err)
	}
	// A new policy reaches open sessions.
	pol, _ := NewPolicy([]string{"*@example.com"}, 3, 0)
	if err := fx.db.SetPolicy(ctx, "app", pol, "ops"); err != nil {
		t.Fatal(err)
	}
	if r, ok := a.Refresh(p2); !ok || r.Policy.MaxPerMinute() != 3 {
		t.Fatalf("refresh: %v %+v", ok, r)
	}
	// The second key expires after an hour.
	now = now.Add(2 * time.Hour)
	if _, err := a.Authenticate("app", second.Key); !errors.Is(err, ErrBadCredentials) {
		t.Fatal("expired key works")
	}
	if _, ok := a.Refresh(p2); ok {
		t.Fatal("expired key survives Refresh")
	}
	// Errors of the write side.
	if _, err := fx.db.Rotate(ctx, "nobody", time.Time{}, "ops"); !errors.Is(err, ErrNoSender) {
		t.Fatalf("rotate unknown: %v", err)
	}
	if _, err := fx.db.Create(ctx, CreateRequest{Name: "Bad", Actor: "ops"}); !errors.Is(err, ErrInvalidName) {
		t.Fatalf("bad name: %v", err)
	}
	if _, err := fx.db.Create(ctx, CreateRequest{Name: "x", Actor: ""}); !errors.Is(err, ErrInvalidActor) {
		t.Fatalf("no actor: %v", err)
	}
	if _, err := fx.db.Create(ctx, CreateRequest{Name: "x", Actor: "ops", ExpiresAt: now.Add(-time.Second)}); !errors.Is(err, ErrExpiry) {
		t.Fatalf("past expiry: %v", err)
	}
	if err := fx.db.SetPolicy(ctx, "nobody", pol, "ops"); !errors.Is(err, ErrNoSender) {
		t.Fatalf("policy of unknown: %v", err)
	}

	// Every change is an event with the actor; no key secret anywhere.
	evs, err := fx.st.EventsAfter(ctx, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	var kinds []string
	for _, e := range evs {
		if e.Actor != "ops" || e.Sender != "app" {
			t.Fatalf("event %+v", e)
		}
		for _, k := range []string{first.Key, second.Key} {
			if strings.Contains(e.Text, k[len(KeyPrefix)+13:]) {
				t.Fatal("secret in an event")
			}
		}
		kinds = append(kinds, e.Kind+" "+e.Text)
	}
	want := []string{
		store.KindSenderCreated + " key=" + first.KeyID,
		store.KindSenderRotated + " key=" + second.KeyID,
		store.KindSenderRevoked + " key=" + first.KeyID,
		store.KindSenderPolicyChanged + " from_patterns=1 max_per_minute=3 max_rcpt_per_hour=0",
	}
	if strings.Join(kinds, "\n") != strings.Join(want, "\n") {
		t.Fatalf("events:\n%s", strings.Join(kinds, "\n"))
	}

	// A new DBSource reads the same state back.
	again, err := NewDBSource(ctx, fx.st, clock)
	if err != nil {
		t.Fatal(err)
	}
	s, _ := again.ByName("app")
	if len(s.Keys) != 2 || s.Keys[0].ID != second.KeyID || s.Keys[1].RevokedBy != "ops" || !s.Policy.Equal(pol) ||
		s.CreatedBy != "ops" || len(s.ActiveKeys(now)) != 0 {
		t.Fatalf("read back %+v", s)
	}
}

// Password checks are limited to MaxChecks at once; key checks need no slot.
func TestPasswordChecksLimited(t *testing.T) {
	key, _, line := keyLine(t, "app")
	h, err := auth.Hash("pw", auth.MinIterations*20)
	if err != nil {
		t.Fatal(err)
	}
	fx := newFixture(t, line+"\nslow:"+h+"\n")
	a := NewAuthenticator(fx.reg, AuthOptions{MaxChecks: 1, Wait: 20 * time.Millisecond})
	var wg sync.WaitGroup
	errs := make(chan error, 4)
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := a.Authenticate("slow", "wrong")
			errs <- err
		}()
	}
	// While the slot is taken, keys still log in.
	if _, err := a.Authenticate("app", key); err != nil {
		t.Fatal(err)
	}
	wg.Wait()
	close(errs)
	busy := 0
	for err := range errs {
		if errors.Is(err, ErrBusy) {
			busy++
		}
	}
	if busy == 0 {
		t.Fatal("no check was refused for lack of a slot")
	}
}

func TestPasswordRefresh(t *testing.T) {
	ctx := context.Background()
	fx := newFixture(t, "legacy:"+testHash()+"\n")
	a := NewAuthenticator(fx.reg, AuthOptions{})
	p, err := a.Authenticate("legacy", "secret")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := a.Refresh(p); !ok {
		t.Fatal("unchanged file ends the session")
	}
	h2, _ := auth.Hash("new", auth.MinIterations)
	writeFile(t, fx.path, "legacy:"+h2+"\n")
	fx.file.Check(ctx)
	if _, ok := a.Refresh(p); ok {
		t.Fatal("a changed password keeps the session")
	}
}

func TestNoPasswordLinesCostNothing(t *testing.T) {
	_, _, line := keyLine(t, "app")
	fx := newFixture(t, line+"\n")
	a := NewAuthenticator(fx.reg, AuthOptions{})
	start := time.Now()
	if _, err := a.Authenticate("app", "not-a-key"); !errors.Is(err, ErrBadCredentials) {
		t.Fatal(err)
	}
	if time.Since(start) > 50*time.Millisecond {
		t.Fatal("a password check ran without password lines")
	}
}

func TestUsageFlush(t *testing.T) {
	ctx := context.Background()
	st := openStore(t)
	u := NewUsage(st, time.Hour, nil)
	t0 := time.UnixMilli(1_800_000_000_000)
	u.Record(Principal{Sender: "app", Source: KindDB, KeyID: "aaaaaaaaaaaa"}, t0)
	u.Record(Principal{Sender: "app", Source: KindDB, KeyID: "aaaaaaaaaaaa"}, t0.Add(time.Minute))
	u.Record(Principal{Sender: "app", Source: KindDB, KeyID: "aaaaaaaaaaaa"}, t0) // older: ignored
	u.Record(Principal{Sender: "legacy", Source: KindFile}, t0)
	if err := u.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	u.Record(Principal{Sender: "legacy", Source: KindFile}, t0.Add(-time.Hour)) // never moves back
	if err := u.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	uses, err := st.SenderUsage(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(uses) != 2 || !uses[0].At.Equal(t0.Add(time.Minute)) || uses[0].KeyID != "aaaaaaaaaaaa" ||
		uses[1].Sender != "legacy" || !uses[1].At.Equal(t0) || uses[1].KeyID != "" {
		t.Fatalf("uses %+v", uses)
	}
}

// readFailDB is the store with a Senders read that fails while fail is set.
type readFailDB struct {
	*store.Store
	fail atomic.Bool
}

func (f *readFailDB) Senders(ctx context.Context) ([]store.SenderRow, error) {
	if f.fail.Load() {
		return nil, errors.New("read failed")
	}
	return f.Store.Senders(ctx)
}

// A revoke or a policy change whose read-back fails must not leave the old
// state usable: the keys it was about stop until a read succeeds.
func TestFailedReadAfterChangeFailsClosed(t *testing.T) {
	ctx := context.Background()
	db := &readFailDB{Store: openStore(t)}
	src, err := NewDBSource(ctx, db, nil)
	if err != nil {
		t.Fatal(err)
	}
	reg, err := NewRegistry(src)
	if err != nil {
		t.Fatal(err)
	}
	a := NewAuthenticator(reg, AuthOptions{})
	app1, _ := src.Create(ctx, CreateRequest{Name: "app", Actor: "ops"})
	app2, _ := src.Rotate(ctx, "app", time.Time{}, "ops")
	other, _ := src.Create(ctx, CreateRequest{Name: "other", Actor: "ops"})
	p2, err := a.Authenticate("app", app2.Key)
	if err != nil {
		t.Fatal(err)
	}

	db.fail.Store(true)
	if err := src.Revoke(ctx, app1.KeyID, "ops"); err == nil {
		t.Fatal("a failed read-back was not reported")
	}
	if _, err := a.Authenticate("app", app1.Key); !errors.Is(err, ErrBadCredentials) {
		t.Fatal("revoked key works after a failed read")
	}
	if _, err := a.Authenticate("app", app2.Key); err != nil {
		t.Fatal("the other key of the sender stopped after a revoke")
	}
	pol, _ := NewPolicy([]string{"*@example.com"}, 0, 0)
	if err := src.SetPolicy(ctx, "app", pol, "ops"); err == nil {
		t.Fatal("a failed read-back was not reported")
	}
	if _, ok := a.Refresh(p2); ok {
		t.Fatal("a session survived a policy change whose read failed")
	}
	if _, err := a.Authenticate("app", app2.Key); !errors.Is(err, ErrBadCredentials) {
		t.Fatal("a key logs in with a policy that may be stale")
	}
	if _, err := a.Authenticate("other", other.Key); err != nil {
		t.Fatal("another sender was affected")
	}

	// The next change that reads successfully restores the real state.
	db.fail.Store(false)
	if err := src.SetPolicy(ctx, "app", pol, "ops"); err != nil {
		t.Fatal(err)
	}
	p, err := a.Authenticate("app", app2.Key)
	if err != nil || p.Policy.String() != "from=*@example.com" {
		t.Fatalf("after a good read: %+v %v", p, err)
	}
	if _, err := a.Authenticate("app", app1.Key); !errors.Is(err, ErrBadCredentials) {
		t.Fatal("revoked key works again")
	}
}
