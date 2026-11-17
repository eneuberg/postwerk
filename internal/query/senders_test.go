package query

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/eneuberg/postwerk/internal/id"
	"github.com/eneuberg/postwerk/internal/senders"
	"github.com/eneuberg/postwerk/internal/store"
)

func TestSenders(t *testing.T) {
	st := openStore(t)
	now := time.Now().Truncate(time.Millisecond)
	fileKey, fileID := senders.NewKey()
	line, _ := senders.KeyLine("app", fileKey)
	path := filepath.Join(t.TempDir(), "senders")
	if err := os.WriteFile(path, []byte(line+"\napp:policy max_per_minute=9\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	file, err := senders.NewFileSource(senders.FileOptions{Path: path})
	if err != nil {
		t.Fatal(err)
	}
	db, err := senders.NewDBSource(ctx, st, nil)
	if err != nil {
		t.Fatal(err)
	}
	reg, err := senders.NewRegistry(file, db)
	if err != nil {
		t.Fatal(err)
	}
	billing, err := db.Create(ctx, senders.CreateRequest{Name: "billing", Actor: "ops"})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Revoke(ctx, billing.KeyID, "ops"); err != nil {
		t.Fatal(err)
	}

	// Mails: app 2 (one failed recipient), billing 1, a former sender 1.
	accept(t, st, id.NewAt(now.Add(-time.Hour)), now.Add(-time.Hour), []string{"a@example.test", "b@example.test"},
		map[string][]store.Status{"t": {store.HandedOff, store.Failed}})
	accept(t, st, id.NewAt(now.Add(-time.Minute)), now.Add(-time.Minute), []string{"a@example.test"}, nil)
	for _, m := range []struct{ sender, key string }{{"billing", billing.KeyID}, {"gone", ""}} {
		mid := id.NewAt(now)
		if err := st.Accept(ctx, store.NewMail{ID: mid, ReceivedAt: now, Sender: m.sender, Recipients: []string{"x@example.test"},
			Raw: []byte{0}, Audit: store.MailAudit{KeyID: m.key, KeySource: "db", ClientIP: "192.0.2.10"}}); err != nil {
			t.Fatal(err)
		}
	}
	// Folded ingress events and old mails outside the window.
	for _, e := range []store.Event{
		{Time: now, Kind: "ingress.rate_limited", Level: store.Warn, Sender: "app", Text: "limit=max_per_minute limited=4"},
		{Time: now, Kind: "ingress.rejected", Level: store.Warn, Sender: "app", Text: "reason=from_not_allowed rejected=2"},
		{Time: now, Kind: "ingress.auth_failed", Level: store.Warn, Sender: "billing", Text: "failures=3"},
		{Time: now.Add(-48 * time.Hour), Kind: "ingress.auth_failed", Level: store.Warn, Sender: "app", Text: "failures=9"},
	} {
		if err := st.Append(ctx, e); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.TouchSenderUsage(ctx, []store.SenderUse{
		{Source: "file", Sender: "app", KeyID: fileID, At: now.Add(-time.Minute)},
		{Source: "db", Sender: "app", KeyID: "zzzzzzzzzzzz", At: now}, // another source: ignored
	}); err != nil {
		t.Fatal(err)
	}

	q := New(Options{Store: st, Senders: reg, Now: func() time.Time { return now }})
	list, err := q.Senders(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 3 || list[0].Name != "app" || list[1].Name != "billing" || list[2].Name != "gone" {
		t.Fatalf("list %+v", list)
	}
	app, bill, gone := list[0], list[1], list[2]
	if app.Source != senders.KindFile || app.Policy.MaxPerMinute() != 9 || len(app.Keys) != 1 ||
		app.Keys[0].ID != fileID || !app.Keys[0].Active || !app.Keys[0].LastUsed.Equal(now.Add(-time.Minute)) ||
		!app.LastUsed.Equal(now.Add(-time.Minute)) {
		t.Fatalf("app %+v", app)
	}
	if app.Recent != (store.SenderActivity{Mails: 2, FailedRecipients: 1, Rejected: 2, RateLimited: 4}) {
		t.Fatalf("app activity %+v", app.Recent)
	}
	if bill.Source != senders.KindDB || bill.CreatedBy != "ops" || len(bill.Keys) != 1 || bill.Keys[0].Active ||
		bill.Keys[0].RevokedBy != "ops" || bill.Recent.Mails != 1 || bill.Recent.AuthFailures != 3 {
		t.Fatalf("billing %+v", bill)
	}
	if !gone.Removed || gone.Source != "" || gone.Recent.Mails != 1 {
		t.Fatalf("gone %+v", gone)
	}

	one, err := q.Sender(ctx, "billing")
	if err != nil || one.Name != "billing" {
		t.Fatalf("Sender: %+v %v", one, err)
	}
	if _, err := q.Sender(ctx, "nobody"); !errors.Is(err, ErrNoSender) {
		t.Fatalf("unknown: %v", err)
	}
	if _, err := q.Sender(ctx, "Bad Name"); !errors.Is(err, ErrNoSender) {
		t.Fatalf("invalid: %v", err)
	}

	// Mails by sender and by key.
	page, err := q.List(ctx, ListFilter{Sender: "app"})
	if err != nil || len(page.Rows) != 2 {
		t.Fatalf("by sender: %d %v", len(page.Rows), err)
	}
	page, err = q.List(ctx, ListFilter{KeyID: billing.KeyID})
	if err != nil || len(page.Rows) != 1 || page.Rows[0].Sender != "billing" {
		t.Fatalf("by key: %+v %v", page.Rows, err)
	}
	for _, f := range []ListFilter{{Sender: "Bad Name"}, {KeyID: "short"}} {
		if _, err := q.List(ctx, f); !errors.Is(err, ErrInvalid) {
			t.Fatalf("%+v: %v", f, err)
		}
	}
	// The detail carries the audit.
	d, err := q.Mail(ctx, page.Rows[0].ID, Full)
	if err != nil || d.Audit.KeyID != billing.KeyID || d.Audit.ClientIP != "192.0.2.10" {
		t.Fatalf("detail audit %+v %v", d.Audit, err)
	}
}
