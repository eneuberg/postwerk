package ingress

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/emersion/go-sasl"

	"github.com/eneuberg/postwerk/internal/store"
)

// refusalEvents returns the ingress events as "kind sender text", sorted.
func refusalEvents(t *testing.T, st *store.Store) []string {
	t.Helper()
	evs, err := st.EventsAfter(context.Background(), 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range evs {
		if !strings.HasPrefix(e.Kind, "ingress.") {
			continue
		}
		if e.Level != store.Warn {
			t.Errorf("%s: level %s, want warn", e.Kind, e.Level)
		}
		out = append(out, strings.Join([]string{e.Kind, e.Sender, e.Text}, " "))
	}
	slices.Sort(out)
	return out
}

// TestRefusalEvents: failed logins and refused commands are counted and
// written as one event per kind, sender and reason (D51). A login name is
// kept only if it is in the senders file; passwords and addresses never
// reach an event or the log.
func TestRefusalEvents(t *testing.T) {
	e := start(t, func(o *Options) { o.refusalWindow = time.Hour })

	c := dial(t, e.addr)
	c1 := c
	for range 2 {
		if err := c.Auth(sasl.NewPlainClient("", "app", "canary-password")); code(err) != 535 {
			t.Fatalf("wrong password: %v", err)
		}
	}
	c = dial(t, e.addr)
	if err := c.Auth(sasl.NewPlainClient("", "canary-password-as-name", "x")); code(err) != 535 {
		t.Fatalf("unknown name: %v", err)
	}
	conn, _ := rawSession(t, e.addr) // a good login, then refused commands
	long := "canary-" + strings.Repeat("x", 320) + "@example.test"
	for _, c := range []struct {
		line string
		want int
	}{
		{"MAIL FROM:<a@example.test> SIZE=999999", 552},
		{"MAIL FROM:<" + long + ">", 553},
		{"MAIL FROM:<a@example.test>", 250},
		{"RCPT TO:<canäry@example.test>", 553},
		{"RCPT TO:<" + long + ">", 553},
		{"RCPT TO:<a@example.test>", 250},
		{"RCPT TO:<b@example.test>", 250},
		{"RCPT TO:<c@example.test>", 250},
		{"RCPT TO:<canary-d@example.test>", 452},
	} {
		if got, text := cmd(conn, c.line); got != c.want {
			t.Fatalf("%s: %d %s", c.line, got, text)
		}
	}
	// A message larger than the limit, found while reading DATA. (A SIZE=
	// above the limit at MAIL, as in the first line above, is answered by
	// the SMTP library itself and not counted.)
	if got, _ := cmd(conn, "DATA"); got != 354 {
		t.Fatalf("DATA: %d", got)
	}
	if got, _ := cmd(conn, "Subject: big\r\n\r\n"+strings.Repeat(strings.Repeat("x", 76)+"\r\n", 100)+"."); got != 552 {
		t.Fatalf("oversized DATA: %d", got)
	}
	if got := refusalEvents(t, e.store); len(got) != 0 {
		t.Fatalf("events while the window is open: %q", got)
	}
	c1.Close()
	c.Close()
	conn.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := e.srv.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"ingress.auth_failed  failures=1",
		"ingress.auth_failed app failures=2",
		"ingress.rejected app reason=bad_address rejected=2",
		"ingress.rejected app reason=needs_smtputf8 rejected=1",
		"ingress.rejected app reason=too_large rejected=1",
		"ingress.rejected app reason=too_many_recipients rejected=1",
	}
	if got := refusalEvents(t, e.store); !slices.Equal(got, want) {
		t.Fatalf("events =\n%q\nwant\n%q", got, want)
	}
	evs, _ := e.store.EventsAfter(context.Background(), 0, 100)
	for _, ev := range evs {
		if s := fmt.Sprintf("%+v", ev); strings.Contains(s, "canary") || strings.Contains(s, "canäry") {
			t.Errorf("event holds a password or an address: %s", s)
		}
	}
	if s := e.logs.String(); strings.Contains(s, "canary") {
		t.Errorf("log holds a password or an address: %s", s)
	}
}

// fakeSpace stands in for the free-space check.
type fakeSpace struct {
	ok    atomic.Bool
	sizes atomic.Int64 // the largest size asked for
}

func (f *fakeSpace) Allow(size int64) bool {
	if size > f.sizes.Load() {
		f.sizes.Store(size)
	}
	return f.ok.Load()
}

// TestNoSpace: below the reserve, MAIL and RCPT get 452 4.3.1 and nothing
// is stored; the check before the commit is asked with the message's size
// (D50).
func TestNoSpace(t *testing.T) {
	space := &fakeSpace{}
	space.ok.Store(true)
	e := start(t, func(o *Options) { o.Space = space })
	conn, _ := rawSession(t, e.addr)
	if got, _ := cmd(conn, "MAIL FROM:<a@example.test>"); got != 250 {
		t.Fatalf("MAIL with space: %d", got)
	}
	space.ok.Store(false)
	if got, text := cmd(conn, "RCPT TO:<a@example.test>"); got != 452 || text != "4.3.1 Insufficient system storage, try again later" {
		t.Fatalf("RCPT without space: %d %s", got, text)
	}
	cmd(conn, "RSET")
	if got, _ := cmd(conn, "MAIL FROM:<a@example.test>"); got != 452 {
		t.Fatalf("MAIL without space: %d", got)
	}
	// Space runs out while the message is read: refused before it is stored.
	space.ok.Store(true)
	cmd(conn, "RSET")
	cmd(conn, "MAIL FROM:<a@example.test>")
	cmd(conn, "RCPT TO:<a@example.test>")
	if got, _ := cmd(conn, "DATA"); got != 354 {
		t.Fatalf("DATA: %d", got)
	}
	space.ok.Store(false)
	if got, _ := cmd(conn, msg+"."); got != 452 {
		t.Fatalf("end of DATA without space: %d", got)
	}
	if space.sizes.Load() < int64(len(msg)) {
		t.Fatalf("largest size asked = %d, want the message's", space.sizes.Load())
	}
	if evs, _ := e.store.EventsAfter(context.Background(), 0, 10); len(evs) != 0 {
		t.Fatalf("stored without space: %+v", evs)
	}
	space.ok.Store(true)
	cmd(conn, "RSET")
	cmd(conn, "MAIL FROM:<a@example.test>")
	cmd(conn, "RCPT TO:<a@example.test>")
	cmd(conn, "DATA")
	if got, _ := cmd(conn, msg+"."); got != 250 {
		t.Fatalf("with space again: %d", got)
	}
}
