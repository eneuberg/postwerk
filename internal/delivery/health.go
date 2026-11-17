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
// A target that cannot be reached is often back a moment later (a
// provider's restart, a network blip). So for it target.down waits until
// DownAfterProbes probes have failed too (D58); sending pauses at once all
// the same. If a probe succeeds first, nothing is written: no target.down,
// no target.recovered. A rejected login is written at once, since it does
// not go away by itself.
//
// The state lives in memory only. After a restart every target starts up,
// and the first attempt finds out again. At start and on recovery every
// queued delivery of the target becomes due at once (resume).

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
	// reported is set once target.down is written for this outage;
	// failedProbes counts the probes that failed since it began.
	reported     bool
	failedProbes int
	// What a failed store write left to do, retried at each step of the
	// worker (retryPending): target.down and target.recovered events in
	// their order, and a resume. In memory only, like the rest.
	pending   []store.Event
	resumeDue bool
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

// goDown marks the target down after a failed attempt. It writes the one
// target.down event at once for a rejected login, and for an unreachable
// target only if DownAfterProbes is 0; otherwise whileDown writes it.
// detail is the reply or error, redacted by the store.
func (w *worker) goDown(ctx context.Context, reason Reason, code int, detail string) {
	now := w.opts.Now()
	w.health.set(true, now, reason)
	w.health.probeWait = w.opts.ProbeInterval
	w.health.probeAt = now.Add(w.health.probeWait)
	w.health.reported, w.health.failedProbes = false, 0
	w.log.Warn("target down, sending paused until a probe succeeds", "reason", string(reason), "code", code)
	w.reportDown(ctx, now, reason, code, detail)
}

// reportDown writes target.down once per outage, when the reason and the
// number of failed probes call for it (D58).
func (w *worker) reportDown(ctx context.Context, now time.Time, reason Reason, code int, detail string) {
	if w.health.reported {
		return
	}
	if reason == ReasonUnreachable && w.health.failedProbes < w.opts.DownAfterProbes {
		return
	}
	w.health.reported = true
	w.writeEvent(ctx, store.Event{Time: now, Target: w.target.Name, Kind: store.KindTargetDown,
		Level: w.problem(), Code: code, Text: join(string(reason), detail)})
}

// writeEvent appends a health event. If the store cannot take it (a full
// volume, D50), the event waits in memory and retryPending writes it later
// with its own time; events stay in order.
func (w *worker) writeEvent(ctx context.Context, e store.Event) {
	if len(w.health.pending) == 0 {
		err := w.appendHealth(ctx, e)
		if err == nil {
			return
		}
		w.log.Error("writing "+e.Kind+" failed; trying again", "err", err)
	}
	w.health.pending = append(w.health.pending, e)
}

func (w *worker) appendHealth(ctx context.Context, e store.Event) error {
	if w.opts.appendHealth != nil {
		return w.opts.appendHealth(ctx, e)
	}
	return w.opts.Store.Append(ctx, e)
}

// retryPending writes the events a failed write left behind and runs a
// resume that failed. It logs only the success: the failure was logged
// once, and the store's own trouble is reported elsewhere (disk.low,
// /readyz).
func (w *worker) retryPending(ctx context.Context) {
	for len(w.health.pending) > 0 {
		e := w.health.pending[0]
		if err := w.appendHealth(ctx, e); err != nil {
			return
		}
		w.health.pending = w.health.pending[1:]
		w.log.Info("written late: "+e.Kind, "event_time", e.Time)
	}
	if w.health.resumeDue {
		w.resume(ctx, w.opts.Now())
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
		w.health.failedProbes++
		if reason != was {
			w.health.set(true, since, reason)
		}
		w.log.Info("target still down", "reason", string(reason), "code", code, "err", err,
			"next_probe_in", w.health.probeWait.String())
		_, _, text := Classify(err, false)
		w.reportDown(ctx, now, reason, code, text)
		return
	}
	down := now.Sub(since).Round(time.Second)
	w.health.set(false, now, "")
	w.resume(ctx, now)
	w.log.Info("target up again, sending resumes", "down_for", down.String())
	if !w.health.reported {
		return // down too briefly for target.down, so no target.recovered either
	}
	w.writeEvent(ctx, store.Event{Time: now, Target: w.target.Name, Kind: store.KindTargetRecovered,
		Level: store.Info, Text: fmt.Sprintf("%s, down for %s", was, down)})
}

// resume makes every queued delivery of the target due now, so the mails
// that waited go out at once instead of after their own backoff. It runs
// when the worker starts and when the target comes back (D45). If it
// fails, the mails keep their backoff and retryPending tries again.
func (w *worker) resume(ctx context.Context, now time.Time) {
	n, err := w.opts.Store.MakeDue(ctx, w.target.Name, now)
	if err != nil {
		if !w.health.resumeDue && ctx.Err() == nil {
			w.log.Error("making queued deliveries due failed; trying again", "err", err)
		}
		w.health.resumeDue = true
		return
	}
	w.health.resumeDue = false
	if n > 0 {
		w.log.Info("queued deliveries due now", "deliveries", n)
	}
}

// probe connects (with STARTTLS or implicit TLS as the URL says), says
// EHLO and logs in, without sending a mail. It
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
