package report

// The Sentry-protocol sink (A4). It sends warn and error events to Sentry or
// a Sentry-compatible tool such as GlitchTip, one envelope per event, with
// plain net/http and no SDK. Every field comes from the event record; there
// are no breadcrumbs, no request data and no host details
// (privacy-and-security.md, change 12).
//
// Protocol references, checked in November 2026:
// develop.sentry.dev/sdk/data-model/envelopes/ (envelope format),
// develop.sentry.dev/sdk/foundations/transport/authentication/ (DSN and
// X-Sentry-Auth), develop.sentry.dev/sdk/foundations/transport/rate-limiting/
// (X-Sentry-Rate-Limits, Retry-After), develop.sentry.dev/sdk/data-model/
// event-payloads/ (event fields), and GlitchTip's apps/event_ingest (schema.py,
// views.py, authentication.py).

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/eneuberg/postwerk/internal/config"
	"github.com/eneuberg/postwerk/internal/redact"
	"github.com/eneuberg/postwerk/internal/store"
)

// Timing of the Sentry sink.
const (
	// sentryTimeout bounds one request, connect to last byte.
	sentryTimeout = 10 * time.Second
	// sentryBackoffMin and sentryBackoffMax bound the wait after a failure
	// (network error, 5xx, refused key). It doubles with every failure.
	sentryBackoffMin = 5 * time.Second
	sentryBackoffMax = 5 * time.Minute
	// sentryDefaultRateLimit is the wait after a 429 without a usable
	// header, as the protocol says.
	sentryDefaultRateLimit = 60 * time.Second
	// sentryMaxRateLimit caps any wait a server asks for.
	sentryMaxRateLimit = time.Hour
	// sentryMaxEnvelope bounds the request body. An event is far smaller:
	// every field is bounded (texts by redact.MaxLen).
	sentryMaxEnvelope = 16 << 10
	// sentryMaxResponse is how much of a response body is read.
	sentryMaxResponse = 4 << 10
)

// SentryOptions configure a Sentry sink.
type SentryOptions struct {
	DSN config.SentryDSN
	// MinLevel is store.Error (default) or store.Warn. Info events are
	// never sent.
	MinLevel store.Level
	// Instance becomes the environment and the instance tag; may be empty.
	Instance string
	// Version is Postwerk's version, sent as the SDK version.
	Version string
	Logger  *slog.Logger
	// Client replaces the HTTP client, for tests.
	Client *http.Client
	// Now replaces the clock, for tests.
	Now func() time.Time
}

// Sentry exports warn and error events to a Sentry-protocol endpoint.
type Sentry struct {
	opts   SentryOptions
	client *http.Client
	auth   string

	mu       sync.Mutex
	until    time.Time // no request before this time
	failures int       // consecutive failures, for the backoff
	down     bool      // a failure was logged; log the recovery
}

// NewSentry returns the sink.
func NewSentry(opts SentryOptions) *Sentry {
	if opts.MinLevel != store.Warn {
		opts.MinLevel = store.Error
	}
	if opts.Logger == nil {
		opts.Logger = slog.New(slog.DiscardHandler)
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.Version == "" {
		opts.Version = "dev"
	}
	c := opts.Client
	if c == nil {
		c = &http.Client{Timeout: sentryTimeout}
	}
	cc := *c
	// A redirect would carry X-Sentry-Auth to wherever it points.
	cc.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	auth := "Sentry sentry_version=7, sentry_client=postwerk/" + opts.Version + ", sentry_key=" + opts.DSN.PublicKey
	if opts.DSN.SecretKey != "" {
		auth += ", sentry_secret=" + opts.DSN.SecretKey
	}
	return &Sentry{opts: opts, client: &cc, auth: auth}
}

// Name implements Sink.
func (s *Sentry) Name() string { return "sentry" }

// wanted reports whether an event of this level is sent.
func (s *Sentry) wanted(l store.Level) bool {
	switch l {
	case store.Error:
		return true
	case store.Warn:
		return s.opts.MinLevel == store.Warn
	}
	return false
}

// Export implements Sink. Events below the level return nil at once, so
// the cursor moves on. While the endpoint is down or rate limited, Export
// returns an error wrapping ErrDeferred without a request.
func (s *Sentry) Export(ctx context.Context, e store.Event) error {
	if !s.wanted(e.Level) {
		return nil
	}
	now := s.opts.Now()
	s.mu.Lock()
	until := s.until
	s.mu.Unlock()
	if now.Before(until) {
		return fmt.Errorf("%w: sentry waits until %s", ErrDeferred, until.UTC().Format(time.RFC3339))
	}
	body, err := s.envelope(e, now)
	if err != nil {
		// Cannot happen with bounded fields; never let one event stall the sink.
		s.opts.Logger.Error("sentry event dropped: cannot build it", "seq", e.Seq, "err", err)
		return nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.opts.DSN.Endpoint, bytes.NewReader(body))
	if err != nil {
		return s.failed(now, 0, err)
	}
	req.Header.Set("Content-Type", "application/x-sentry-envelope")
	req.Header.Set("User-Agent", "postwerk/"+s.opts.Version)
	req.Header.Set("X-Sentry-Auth", s.auth)
	resp, err := s.client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return s.failed(now, 0, scrubURLError(err))
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, sentryMaxResponse))
	resp.Body.Close()

	wait, limited := rateLimit(resp.Header, resp.StatusCode, now)
	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		s.succeeded(wait)
		return nil
	case resp.StatusCode == http.StatusTooManyRequests || limited:
		return s.limited(now, wait, resp.StatusCode)
	case resp.StatusCode == http.StatusBadRequest || resp.StatusCode == http.StatusRequestEntityTooLarge ||
		resp.StatusCode == http.StatusUnprocessableEntity:
		// The endpoint refuses this event; sending it again will not help.
		s.opts.Logger.Warn("sentry refused an event; it is skipped", "seq", e.Seq, "kind", e.Kind,
			"status", resp.StatusCode, "detail", redact.Text(resp.Header.Get("X-Sentry-Error")))
		s.succeeded(0)
		return nil
	default:
		// 5xx, and 401/403/404: a server problem or a wrong DSN. Keep the
		// event and back off, as with a rejected SMTP login.
		return s.failed(now, resp.StatusCode, nil)
	}
}

func (s *Sentry) succeeded(wait time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.failures = 0
	if wait > 0 {
		s.until = s.opts.Now().Add(wait)
	}
	if s.down {
		s.down = false
		s.opts.Logger.Info("sentry export works again", "host", s.opts.DSN.Host)
	}
}

func (s *Sentry) failed(now time.Time, status int, err error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	wait := sentryBackoffMin << min(s.failures, 10)
	wait = min(wait, sentryBackoffMax)
	s.failures++
	s.until = now.Add(wait)
	attrs := []any{"host", s.opts.DSN.Host, "retry_in", wait.String()}
	if status != 0 {
		attrs = append(attrs, "status", status)
	}
	if err != nil {
		attrs = append(attrs, "err", err)
	}
	if !s.down {
		s.down = true
		msg := "sentry export failing; events wait in the event log and are sent later"
		if status == http.StatusUnauthorized || status == http.StatusForbidden || status == http.StatusNotFound {
			msg = "sentry refused the DSN (wrong key or project?); events wait in the event log"
			s.opts.Logger.Error(msg, attrs...)
		} else {
			s.opts.Logger.Warn(msg, attrs...)
		}
	}
	if err == nil {
		err = fmt.Errorf("status %d", status)
	}
	return fmt.Errorf("%w: sentry: %w", ErrDeferred, err)
}

func (s *Sentry) limited(now time.Time, wait time.Duration, status int) error {
	if wait <= 0 {
		wait = sentryDefaultRateLimit
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.until = now.Add(wait)
	s.opts.Logger.Warn("sentry rate limit; events wait in the event log", "host", s.opts.DSN.Host,
		"status", status, "retry_in", wait.String())
	s.down = true
	return fmt.Errorf("%w: sentry rate limit for %s", ErrDeferred, wait)
}

// rateLimit reads X-Sentry-Rate-Limits (on any response) and, on a 429
// without it, Retry-After. It returns the longest wait that applies to
// the categories this sink sends (error and default, or all), and whether
// such a limit was found.
func rateLimit(h http.Header, status int, now time.Time) (time.Duration, bool) {
	var wait time.Duration
	found := false
	if v := h.Get("X-Sentry-Rate-Limits"); v != "" {
		for _, quota := range strings.Split(v, ",") {
			parts := strings.Split(strings.ReplaceAll(quota, " ", ""), ":")
			secs, err := strconv.ParseFloat(parts[0], 64)
			if err != nil || secs < 0 {
				continue
			}
			applies := len(parts) < 2 || parts[1] == ""
			if !applies {
				for _, c := range strings.Split(parts[1], ";") {
					if c == "error" || c == "default" {
						applies = true
					}
				}
			}
			if applies {
				found = true
				wait = max(wait, time.Duration(secs*float64(time.Second)))
			}
		}
		return min(wait, sentryMaxRateLimit), found
	}
	if status != http.StatusTooManyRequests {
		return 0, false
	}
	if v := strings.TrimSpace(h.Get("Retry-After")); v != "" {
		if secs, err := strconv.Atoi(v); err == nil && secs >= 0 {
			return min(time.Duration(secs)*time.Second, sentryMaxRateLimit), true
		}
		if t, err := http.ParseTime(v); err == nil {
			return min(max(t.Sub(now), 0), sentryMaxRateLimit), true
		}
	}
	return sentryDefaultRateLimit, true
}

// scrubURLError drops the URL from a client error: it is long and adds
// nothing to the host, which the log line names anyway.
func scrubURLError(err error) error {
	var ue *url.Error
	if errors.As(err, &ue) {
		var ne net.Error
		if errors.As(ue.Err, &ne) && ne.Timeout() {
			return errors.New("timeout")
		}
		return ue.Err
	}
	return err
}

// EventID returns the Sentry event id of an event: a UUID (version 8,
// RFC 9562) from SHA-256 over instance, seq, time, mail id and kind. The
// same event always gets the same id, so a second export after a crash is
// dropped as a duplicate by the receiver. Time and mail id keep ids apart
// when a new database starts again at seq 1 under the same instance name.
func EventID(instance string, e store.Event) string {
	h := sha256.New()
	fmt.Fprintf(h, "postwerk-sentry\x00%s\x00%d\x00%d\x00%s\x00%s", instance, e.Seq, e.Time.UnixMilli(), e.MailID, e.Kind)
	var b [16]byte
	copy(b[:], h.Sum(nil))
	b[6] = b[6]&0x0f | 0x80 // version 8
	b[8] = b[8]&0x3f | 0x80 // variant RFC 9562
	return hex.EncodeToString(b[:])
}

type sentrySDK struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

type sentryEnvelopeHeader struct {
	EventID string    `json:"event_id"`
	SentAt  string    `json:"sent_at"`
	SDK     sentrySDK `json:"sdk"`
}

type sentryItemHeader struct {
	Type        string `json:"type"`
	Length      int    `json:"length"`
	ContentType string `json:"content_type"`
}

type sentryLogEntry struct {
	Formatted string `json:"formatted"`
}

// sentryEvent is the event payload. It has no field for addresses,
// content, request data, user data, breadcrumbs or the host name.
type sentryEvent struct {
	EventID     string            `json:"event_id"`
	Timestamp   string            `json:"timestamp"`
	Platform    string            `json:"platform"`
	Level       string            `json:"level"`
	Logger      string            `json:"logger"`
	Environment string            `json:"environment,omitempty"`
	LogEntry    sentryLogEntry    `json:"logentry"`
	Fingerprint []string          `json:"fingerprint"`
	Tags        map[string]string `json:"tags"`
	Extra       map[string]any    `json:"extra,omitempty"`
	SDK         sentrySDK         `json:"sdk"`
}

const sentryTime = "2006-01-02T15:04:05.000Z"

// envelope builds the request body for one event.
func (s *Sentry) envelope(e store.Event, now time.Time) ([]byte, error) {
	id := EventID(s.opts.Instance, e)
	level := "error"
	if e.Level == store.Warn {
		level = "warning"
	}
	text := redact.Text(e.Text) // already redacted when stored; again, to be sure
	ev := sentryEvent{
		EventID:     id,
		Timestamp:   e.Time.UTC().Format(sentryTime),
		Platform:    "other",
		Level:       level,
		Logger:      "postwerk",
		Environment: s.opts.Instance,
		LogEntry:    sentryLogEntry{Formatted: message(e, text)},
		Fingerprint: []string{"postwerk", e.Kind},
		Tags:        map[string]string{},
		Extra:       map[string]any{"seq": e.Seq},
		SDK:         sentrySDK{Name: "postwerk", Version: s.opts.Version},
	}
	if e.Target != "" {
		ev.Fingerprint = append(ev.Fingerprint, e.Target) // one issue per kind and target
	}
	// Tag values must not be empty (Relay refuses empty values).
	for k, v := range map[string]string{
		"kind": e.Kind, "target": e.Target, "sender": e.Sender, "instance": s.opts.Instance,
		"level": string(e.Level), "mail_id": e.MailID, "actor": e.Actor,
	} {
		if v != "" {
			ev.Tags[k] = v
		}
	}
	if e.MailID != "" {
		ev.Extra["ui_path"] = "/mails/" + e.MailID
	}
	if e.Attempt != 0 {
		ev.Extra["attempt"] = e.Attempt
	}
	if e.Recipient != nil {
		ev.Extra["recipient"] = *e.Recipient // position, never the address
	}
	switch {
	case FromSMTPReply(e):
		ev.Extra["smtp_code"] = e.Code
		if text != "" {
			ev.Extra["smtp_text"] = text
		}
	case text != "":
		ev.Extra["text"] = text
	}
	payload, err := marshal(ev)
	if err != nil {
		return nil, err
	}
	header, err := marshal(sentryEnvelopeHeader{EventID: id, SentAt: now.UTC().Format(sentryTime),
		SDK: sentrySDK{Name: "postwerk", Version: s.opts.Version}})
	if err != nil {
		return nil, err
	}
	item, err := marshal(sentryItemHeader{Type: "event", Length: len(payload), ContentType: "application/json"})
	if err != nil {
		return nil, err
	}
	var b bytes.Buffer
	b.Write(header)
	b.WriteByte('\n')
	b.Write(item)
	b.WriteByte('\n')
	b.Write(payload)
	b.WriteByte('\n')
	if b.Len() > sentryMaxEnvelope {
		return nil, fmt.Errorf("envelope of %d bytes exceeds %d", b.Len(), sentryMaxEnvelope)
	}
	return b.Bytes(), nil
}

// message is the event title, such as
// "delivery.failed at mailjet: 550 5.1.1 <redacted>: unknown user".
func message(e store.Event, text string) string {
	m := e.Kind
	if e.Target != "" {
		m += " at " + e.Target
	}
	if e.Actor != "" {
		m += " by " + e.Actor
		return m
	}
	var reply []string
	if e.Code != 0 {
		reply = append(reply, strconv.Itoa(e.Code))
	}
	if text != "" {
		reply = append(reply, text)
	}
	if len(reply) > 0 {
		m += ": " + strings.Join(reply, " ")
	}
	return m
}

// marshal encodes v as one line of JSON without HTML escaping.
func marshal(v any) ([]byte, error) {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(b.Bytes(), []byte("\n")), nil
}
