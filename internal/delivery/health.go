package delivery

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/emersion/go-sasl"
	"github.com/emersion/go-smtp"

	"github.com/eneuberg/postwerk/internal/store"
)

// Target health (D45). A rejected login (535) or a target that cannot be
// reached (connect, TLS, greeting or EHLO failed) is a property of the
// target, not of a mail. On the first such failure the target goes down:
// its worker stops sending and instead probes the target (connect, EHLO,
// AUTH, QUIT; never a mail) with a growing pause. When a probe succeeds the
// target is up again and the queue resumes. Going down writes one
// target.down event, coming back one target.recovered event; the mails in
// between write nothing, so a provider that locks an account after repeated
// failed logins sees one login per probe, not one per mail.
//
// The state lives in memory only. After a restart every target starts up,
// and the first attempt finds out again.

// Reason says why a target is down.
type Reason string

// Reasons a target goes down.
const (
	// ReasonAuth: the target rejected the login with 535.
	ReasonAuth Reason = "auth"
	// ReasonUnreachable: connecting, TLS, the greeting or EHLO failed.
	ReasonUnreachable Reason = "unreachable"
)

// Health is the state of one target.
type Health struct {
	Target string
	Down   bool
	// Since is when the target went down, or when it was last seen up
	// again (or the worker started).
	Since time.Time
	// Reason is set while the target is down.
	Reason Reason
}

// health is a worker's view of its target. The worker goroutine changes
// it; Manager.Health reads it.
type health struct {
	mu     sync.Mutex
	down   bool
	since  time.Time
	reason Reason

	// Only the worker goroutine uses these.
	probeWait time.Duration
	probeAt   time.Time
}

func (h *health) get() (down bool, since time.Time, reason Reason) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.down, h.since, h.reason
}

func (h *health) set(down bool, since time.Time, reason Reason) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.down, h.since, h.reason = down, since, reason
}

// Health returns the state of every target, in configuration order.
func (m *Manager) Health() []Health {
	out := make([]Health, 0, len(m.workers))
	for _, w := range m.workers {
		down, since, reason := w.health.get()
		out = append(out, Health{Target: w.target.Name, Down: down, Since: since, Reason: reason})
	}
	return out
}

// goDown marks the target down after a failed attempt and writes the one
// target.down event; detail is the reply or error, redacted by the store.
func (w *worker) goDown(ctx context.Context, reason Reason, code int, detail string) {
	now := w.opts.Now()
	w.health.set(true, now, reason)
	w.health.probeWait = w.opts.ProbeInterval
	w.health.probeAt = now.Add(w.health.probeWait)
	w.log.Warn("target down, sending paused until a probe succeeds", "reason", string(reason), "code", code)
	if err := w.opts.Store.Append(ctx, store.Event{Time: now, Target: w.target.Name, Kind: store.KindTargetDown,
		Level: w.problem(), Code: code, Text: join(string(reason), detail)}); err != nil {
		w.log.Error("writing target.down failed", "err", err)
	}
}

// whileDown is one step of a down worker: fail the mails whose retry window
// ended, then wait for the next probe or run it.
func (w *worker) whileDown(ctx context.Context) {
	now := w.opts.Now()
	w.expire(ctx, now)
	if wait := w.health.probeAt.Sub(now); wait > 0 {
		// New mails need no wake-up: they wait for the probe.
		w.sleepQuiet(ctx, min(wait, w.opts.PollInterval))
		return
	}
	reason, code, err := w.probe(ctx)
	if ctx.Err() != nil {
		return
	}
	now = w.opts.Now()
	_, since, was := w.health.get()
	if err != nil {
		w.health.probeWait = min(2*w.health.probeWait, w.opts.ProbeMaxInterval)
		w.health.probeAt = now.Add(w.health.probeWait)
		if reason != was {
			w.health.set(true, since, reason)
		}
		w.log.Info("target still down", "reason", string(reason), "code", code, "err", err,
			"next_probe_in", w.health.probeWait.String())
		return
	}
	down := now.Sub(since).Round(time.Second)
	w.health.set(false, now, "")
	w.log.Info("target up again, sending resumes", "down_for", down.String())
	if err := w.opts.Store.Append(ctx, store.Event{Time: now, Target: w.target.Name, Kind: store.KindTargetRecovered,
		Level: store.Info, Text: fmt.Sprintf("%s, down for %s", was, down)}); err != nil {
		w.log.Error("writing target.recovered failed", "err", err)
	}
}

// probe connects, says EHLO and logs in, without sending a mail. It
// returns why the target is still down if it fails.
func (w *worker) probe(ctx context.Context) (Reason, int, error) {
	client, err := w.connect(ctx)
	if err != nil {
		_, code, _ := Classify(err, false)
		return ReasonUnreachable, code, err
	}
	defer client.Close()
	if w.target.Username != "" {
		if err := client.Auth(sasl.NewPlainClient("", w.target.Username, w.target.Password)); err != nil {
			o, code, _ := Classify(err, false)
			if o == AuthFailed {
				return ReasonAuth, code, err
			}
			return ReasonUnreachable, code, err
		}
	}
	_ = client.Quit()
	return "", 0, nil
}

// expire fails the queued recipients whose retry window ended while the
// target was down, as an attempt would have done.
func (w *worker) expire(ctx context.Context, now time.Time) {
	_, _, reason := w.health.get()
	for ctx.Err() == nil {
		n, err := w.opts.Store.ExpireQueued(ctx, store.Expiry{
			Target: w.target.Name, ReceivedBefore: now.Add(-w.target.RetryWindow), At: now,
			Kind: store.KindDeliveryFailed, Level: w.problem(),
			Text: "retry window exceeded while the target was down (" + string(reason) + ")",
		})
		if err != nil {
			w.log.Error("failing expired deliveries failed", "err", err)
			return
		}
		if n < store.ExpireBatch {
			return
		}
	}
}

// sleepQuiet sleeps like sleep, but a wake signal does not end it.
func (w *worker) sleepQuiet(ctx context.Context, d time.Duration) {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-t.C:
	}
}

// recipientRejected reports whether a permanent reply refuses one
// recipient's address rather than the sender, the login or the target's
// policy (D46): 550, 551 or 553 whose enhanced code is 5.1.x (address) or
// 5.2.x (mailbox), or, at RCPT, one without an enhanced code. Such a
// rejection is delivery.rejected (warn): only the sender of the mail can do
// something about it.
func recipientRejected(err error, atRcpt bool) bool {
	var se *smtp.SMTPError
	if !errors.As(err, &se) {
		return false
	}
	switch se.Code {
	case 550, 551, 553:
	default:
		return false
	}
	ec := se.EnhancedCode
	if ec == smtp.EnhancedCodeNotSet || ec == smtp.NoEnhancedCode {
		return atRcpt
	}
	return ec[0] == 5 && (ec[1] == 1 || ec[1] == 2)
}
