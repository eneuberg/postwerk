package ui

import (
	"context"
	"fmt"
	"math"
	"math/rand/v2"
	"testing"
	"time"

	"github.com/eneuberg/postwerk/internal/store"
)

// seedTraffic fills the demo with 30 days of made-up traffic for the
// statistics, through the store's own transitions: a daily curve with
// quieter weekends, four applications, mailjet for every mail and the
// optional capture for a quarter, hand-offs after 0.4–3 s, a few failures
// and rejected addresses, a newsletter burst three days ago that hit its
// rate limit, refused logins, and a 25-minute outage of mailjet yesterday
// whose mails were retried and went out after it. It leaves nothing queued.
func seedTraffic(t *testing.T, st *store.Store, now time.Time) {
	t.Helper()
	ctx := context.Background()
	rnd := rand.New(rand.NewPCG(4711, 17))
	day := 24 * time.Hour
	start := now.Add(-30 * day).Truncate(time.Hour)
	downFrom := now.Add(-day).Truncate(24 * time.Hour).Add(14 * time.Hour)
	downTo := downFrom.Add(25 * time.Minute)
	burst := now.Add(-3 * day).Truncate(24 * time.Hour).Add(9 * time.Hour)
	event := func(e store.Event) {
		t.Helper()
		if err := st.Append(ctx, e); err != nil {
			t.Fatal(err)
		}
	}
	n := 0
	for hour := start; hour.Before(now.Add(-10 * time.Minute)); hour = hour.Add(time.Hour) {
		// Busy around 13:00 UTC, quiet at night and on weekends, a little
		// more in the last week.
		h := float64(hour.Hour())
		rate := 3 + 22*math.Exp(-(h-13)*(h-13)/18)
		if wd := hour.Weekday(); wd == time.Saturday || wd == time.Sunday {
			rate *= 0.45
		}
		if hour.After(now.Add(-7 * day)) {
			rate *= 1.3
		}
		mails := int(rate*(0.8+0.4*rnd.Float64())) + 1
		if !hour.Before(burst) && hour.Before(burst.Add(2*time.Hour)) {
			mails += 140
		}
		for i := range mails {
			at := hour.Add(time.Duration(rnd.Int64N(int64(time.Hour)))).Truncate(time.Millisecond)
			if !at.Before(now.Add(-10 * time.Minute)) {
				continue
			}
			sender := []string{"shop", "shop", "shop", "auth-service", "auth-service", "billing"}[rnd.IntN(6)]
			if !hour.Before(burst) && hour.Before(burst.Add(2*time.Hour)) && i >= mails-140 {
				sender = "newsletter"
			}
			n++
			id := fmt.Sprintf("pw_01KTRAFFIC%015d", n)
			rcpts := []string{fmt.Sprintf("c%d@example.com", n)}
			if rnd.IntN(10) == 0 {
				rcpts = append(rcpts, fmt.Sprintf("d%d@example.org", n))
			}
			m := store.NewMail{ID: id, ReceivedAt: at, Sender: sender, EnvelopeFrom: "noreply@example.com", Recipients: rcpts,
				Raw: []byte("Subject: demo\r\n\r\ndemo\r\n"), Size: 1800 + rnd.IntN(4000),
				Deliveries: []store.NewDelivery{{Target: "mailjet", Status: store.Queued}}}
			capture := rnd.IntN(4) == 0
			if capture {
				m.Deliveries = append([]store.NewDelivery{{Target: "capture", Status: store.Queued}}, m.Deliveries...)
			}
			if err := st.Accept(ctx, m); err != nil {
				t.Fatal(err)
			}
			finish := func(target string, at time.Time, sts []store.Status, kinds []string, code int, next time.Time) {
				t.Helper()
				c, err := st.Claim(ctx, target, at.Add(-300*time.Millisecond))
				if err != nil || c == nil || c.MailID != id {
					t.Fatalf("claim %s/%s: %+v %v", id, target, c, err)
				}
				r := store.Result{MailID: id, Target: target, At: at, Attempt: c.Attempt, Code: code, Text: "2.0.0 OK",
					NextAttempt: next}
				for i, rc := range c.Recipients {
					level := store.Info
					if sts[i] != store.HandedOff {
						level = store.Warn
					}
					r.Recipients = append(r.Recipients, store.RecipientResult{Position: rc.Position, Status: sts[i],
						Code: code, Text: "demo reply", Kind: kinds[i], Level: level})
				}
				if err := st.Finish(ctx, r); err != nil {
					t.Fatal(err)
				}
			}
			all := func(s store.Status, kind string) ([]store.Status, []string) {
				var sts []store.Status
				var kinds []string
				for range rcpts {
					sts, kinds = append(sts, s), append(kinds, kind)
				}
				return sts, kinds
			}
			latency := time.Duration(400+rnd.IntN(900)+rnd.IntN(1700)) * time.Millisecond
			if capture {
				sts, kinds := all(store.HandedOff, store.KindDeliveryHandedOff)
				finish("capture", at.Add(300*time.Millisecond), sts, kinds, 250, time.Time{})
			}
			switch r := rnd.IntN(1000); {
			case !at.Before(downFrom) && at.Before(downTo):
				// Mailjet refused the login: queued, then out after the probe.
				sts, kinds := all(store.Queued, store.KindDeliveryRetry)
				finish("mailjet", at.Add(time.Second), sts, kinds, 535, downTo)
				sts, kinds = all(store.HandedOff, store.KindDeliveryHandedOff)
				finish("mailjet", downTo.Add(time.Duration(1+rnd.IntN(40))*time.Second), sts, kinds, 250, time.Time{})
			case r < 4:
				sts, kinds := all(store.Failed, store.KindDeliveryRejected)
				finish("mailjet", at.Add(latency), sts, kinds, 550, time.Time{})
			case r < 7:
				sts, kinds := all(store.Failed, store.KindDeliveryFailed)
				finish("mailjet", at.Add(latency), sts, kinds, 554, time.Time{})
			case r < 25:
				sts, kinds := all(store.Queued, store.KindDeliveryRetry)
				retry := at.Add(latency + time.Duration(30+rnd.IntN(240))*time.Second)
				finish("mailjet", at.Add(latency), sts, kinds, 451, retry)
				sts, kinds = all(store.HandedOff, store.KindDeliveryHandedOff)
				finish("mailjet", retry.Add(latency), sts, kinds, 250, time.Time{})
			case r < 26 && at.After(now.Add(-2*day)):
				sts, kinds := all(store.Unknown, store.KindDeliveryUnknown)
				finish("mailjet", at.Add(latency), sts, kinds, 0, time.Time{})
			default:
				sts, kinds := all(store.HandedOff, store.KindDeliveryHandedOff)
				finish("mailjet", at.Add(latency), sts, kinds, 250, time.Time{})
			}
		}
		// Refusals at ingress now and then.
		if rnd.IntN(9) == 0 {
			event(store.Event{Time: hour.Add(17 * time.Minute), Kind: "ingress.auth_failed", Level: store.Warn,
				Text: fmt.Sprintf("failures=%d", 1+rnd.IntN(6))})
		}
		if rnd.IntN(30) == 0 {
			event(store.Event{Time: hour.Add(41 * time.Minute), Kind: "ingress.rejected", Level: store.Warn, Sender: "billing",
				Text: "reason=too_large rejected=1"})
		}
	}
	for i := range 2 {
		event(store.Event{Time: burst.Add(time.Duration(i)*time.Hour + 20*time.Minute), Kind: "ingress.rate_limited",
			Level: store.Warn, Sender: "newsletter", Text: fmt.Sprintf("limit=max_per_minute limited=%d", 37+i*11)})
	}
	event(store.Event{Time: downFrom, Target: "mailjet", Kind: store.KindTargetDown, Level: store.Error, Code: 535,
		Text: "auth: 5.7.8 authentication failed"})
	event(store.Event{Time: downTo, Target: "mailjet", Kind: store.KindTargetRecovered, Level: store.Info,
		Text: "auth, down for 25m0s"})
	t.Logf("demo traffic: %d mails over 30 days", n)
}
