package delivery

import (
	"errors"
	"fmt"
	"time"

	"github.com/emersion/go-smtp"

	"github.com/eneuberg/postwerk/internal/config"
	"github.com/eneuberg/postwerk/internal/store"
)

// Outcome is what one attempt amounted to, before any policy is applied.
type Outcome int

// Outcomes of an attempt (architecture.md, "Classification").
const (
	// HandedOff: the target replied 2xx after the message.
	HandedOff Outcome = iota
	// Temporary: a 4xx reply, or no reply before the message was sent.
	Temporary
	// AuthFailed: the target rejected our login with 535.
	AuthFailed
	// Permanent: any other 5xx reply, including a rejected recipient.
	Permanent
	// Unclear: no reply after the message was sent (D5).
	Unclear
)

// Classify maps the error of an attempt to an outcome. dataSent tells
// whether data_sent_at was recorded before the error. It also returns the
// reply code (0 without a reply) and a short text for the event.
func Classify(err error, dataSent bool) (Outcome, int, string) {
	if err == nil {
		return HandedOff, 250, ""
	}
	var se *smtp.SMTPError
	if errors.As(err, &se) {
		text := replyText(se)
		switch {
		case se.Code == 535:
			return AuthFailed, se.Code, text
		case se.Code >= 400 && se.Code < 500:
			return Temporary, se.Code, text
		case se.Code >= 500 && se.Code < 600:
			return Permanent, se.Code, text
		case se.Code >= 200 && se.Code < 300 && dataSent:
			// A 2xx other than the expected 250 after the message.
			return HandedOff, se.Code, text
		}
		// An unexpected code is treated like a missing reply.
		if dataSent {
			return Unclear, se.Code, text
		}
		return Temporary, se.Code, text
	}
	if dataSent {
		return Unclear, 0, err.Error()
	}
	return Temporary, 0, err.Error()
}

func replyText(se *smtp.SMTPError) string {
	ec := se.EnhancedCode
	if ec == smtp.EnhancedCodeNotSet || ec == smtp.NoEnhancedCode {
		return se.Message
	}
	return fmt.Sprintf("%d.%d.%d %s", ec[0], ec[1], ec[2], se.Message)
}

// policy is what decide needs to know about a target and a delivery.
type policy struct {
	onUnknown config.OnUnknown
	optional  bool
	deadline  time.Time // end of the retry window
	backoff   func(attempt int) time.Duration
}

// decision is the next state of a delivery.
type decision struct {
	status store.Status
	next   time.Time
	kind   string
	level  store.Level
	text   string
}

// decide applies a target's policy to an outcome (D3, D4, D5).
func decide(o Outcome, text string, p policy, now time.Time, attempt int) decision {
	problem := store.Error
	if p.optional {
		problem = store.Warn
	}
	retry := func(kind string, level store.Level, why string) decision {
		if !now.Before(p.deadline) {
			return decision{status: store.Failed, kind: store.KindDeliveryFailed, level: problem,
				text: join("retry window exceeded", why)}
		}
		next := now.Add(p.backoff(attempt))
		if next.After(p.deadline) {
			next = p.deadline // one last attempt at the end of the window
		}
		return decision{status: store.Queued, next: next, kind: kind, level: level, text: why}
	}

	switch o {
	case HandedOff:
		return decision{status: store.HandedOff, kind: store.KindDeliveryHandedOff, level: store.Info, text: text}
	case Temporary:
		return retry(store.KindDeliveryRetry, store.Warn, text)
	case AuthFailed:
		// D5 in decisions.md: a rejected login is our outage, the mail stays
		// queued and the error is raised at once.
		return retry(store.KindTargetAuthFailed, problem, text)
	case Permanent:
		return decision{status: store.Failed, kind: store.KindDeliveryFailed, level: problem, text: text}
	case Unclear:
		switch p.onUnknown {
		case config.Retry:
			return retry(store.KindDeliveryRetry, store.Warn,
				join("no reply after the message was sent, sending again (policy retry)", text))
		case config.Check:
			return decision{status: store.Unknown, kind: store.KindDeliveryUnknown, level: problem,
				text: join("no reply after the message was sent; provider check pending, status adapter not built yet", text)}
		default:
			return decision{status: store.Unknown, kind: store.KindDeliveryUnknown, level: problem,
				text: join("no reply after the message was sent; held for an admin (policy hold)", text)}
		}
	}
	return decision{status: store.Failed, kind: store.KindDeliveryFailed, level: problem, text: "internal: unknown outcome"}
}

func join(a, b string) string {
	if b == "" {
		return a
	}
	return a + ": " + b
}

// DefaultBackoff is the retry schedule of D4: 30 s, 1 min, 2 min, 5 min,
// 10 min, 30 min, then hourly. attempt is the number of attempts made so far.
func DefaultBackoff(attempt int) time.Duration {
	steps := []time.Duration{
		30 * time.Second, time.Minute, 2 * time.Minute, 5 * time.Minute, 10 * time.Minute, 30 * time.Minute,
	}
	if attempt >= 1 && attempt <= len(steps) {
		return steps[attempt-1]
	}
	if attempt < 1 {
		return steps[0]
	}
	return time.Hour
}
