// Package delivery sends queued mails to their targets (D1–D11).
//
// Each target has its own worker goroutine, so a slow or broken target never
// delays another (D2). A worker claims one due delivery at a time, sends it,
// classifies the result and writes the next state with its event. A send in
// progress is never cut by a shutdown: it runs on a context without
// cancellation and is bounded only by the SMTP timeouts.
//
// The stored message is decrypted right before the attempt. A message that
// retention has removed, or that cannot be decrypted, fails the delivery
// without anything being sent.
package delivery

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/emersion/go-sasl"
	"github.com/emersion/go-smtp"

	"github.com/eneuberg/postwerk/internal/config"
	"github.com/eneuberg/postwerk/internal/message"
	"github.com/eneuberg/postwerk/internal/seal"
	"github.com/eneuberg/postwerk/internal/store"
)

// Defaults for Options. Every single timeout fits within DefaultAttemptTimeout,
// and config.DefaultShutdownTimeout (100 s) is longer than it, so a send that
// is in progress when the shutdown starts can always finish.
const (
	DefaultPollInterval      = 30 * time.Second
	DefaultDialTimeout       = 15 * time.Second
	DefaultCommandTimeout    = 30 * time.Second
	DefaultSubmissionTimeout = 60 * time.Second
	DefaultAttemptTimeout    = 90 * time.Second
)

// Options configure a Manager.
type Options struct {
	Store *store.Store
	// Sealer opens the stored messages. Required.
	Sealer  *seal.Sealer
	Targets []config.Target
	Logger  *slog.Logger
	// HeloName is the name Postwerk gives in EHLO. With STARTTLS the
	// first EHLO, before TLS, says "localhost" (a limit of the SMTP
	// library); the EHLO inside TLS uses HeloName.
	HeloName string

	// Backoff returns the wait after attempt n; nil means DefaultBackoff.
	Backoff func(attempt int) time.Duration
	// PollInterval bounds how long a worker sleeps without a wake signal.
	PollInterval time.Duration
	// Timeouts; zero means the default.
	DialTimeout       time.Duration
	CommandTimeout    time.Duration
	SubmissionTimeout time.Duration
	// AttemptTimeout bounds one whole attempt, including the message body.
	AttemptTimeout time.Duration
	// Now returns the current time; nil means time.Now.
	Now func() time.Time

	// rootCAs replaces the system roots. Only tests set it.
	rootCAs *x509.CertPool
}

// Manager runs one worker per target.
type Manager struct {
	workers []*worker
	wg      sync.WaitGroup
	cancel  context.CancelFunc
}

// New creates a Manager. Call Start to run it.
func New(opts Options) *Manager {
	if opts.Sealer == nil {
		panic("delivery: Options.Sealer is required")
	}
	if opts.Backoff == nil {
		opts.Backoff = DefaultBackoff
	}
	if opts.PollInterval <= 0 {
		opts.PollInterval = DefaultPollInterval
	}
	if opts.DialTimeout <= 0 {
		opts.DialTimeout = DefaultDialTimeout
	}
	if opts.CommandTimeout <= 0 {
		opts.CommandTimeout = DefaultCommandTimeout
	}
	if opts.SubmissionTimeout <= 0 {
		opts.SubmissionTimeout = DefaultSubmissionTimeout
	}
	if opts.AttemptTimeout <= 0 {
		opts.AttemptTimeout = DefaultAttemptTimeout
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.Logger == nil {
		opts.Logger = slog.New(slog.DiscardHandler)
	}
	if opts.HeloName == "" {
		opts.HeloName = "localhost"
	}
	m := &Manager{}
	for _, t := range opts.Targets {
		m.workers = append(m.workers, &worker{
			opts:   &opts,
			target: t,
			log:    opts.Logger.With("target", t.Name),
			wake:   make(chan struct{}, 1),
		})
	}
	return m
}

// Start starts the workers. They stop when ctx is cancelled or Stop is
// called, after finishing the send in progress.
func (m *Manager) Start(ctx context.Context) {
	ctx, m.cancel = context.WithCancel(ctx)
	for _, w := range m.workers {
		w.running.Store(true)
		m.wg.Add(1)
		go func() {
			defer m.wg.Done()
			defer w.running.Store(false)
			w.run(ctx)
		}()
	}
}

// Recover handles the deliveries of every worker that were left in sending by
// a crash or a cut shutdown (D6), before the workers start. The caller runs
// it before the first retention run, so that retention sees the recovered
// state: a mail past its maximum age is then expired, not sent. Each worker
// runs the same recovery again when it starts; that finds nothing left.
func (m *Manager) Recover(ctx context.Context) error {
	var errs []error
	for _, w := range m.workers {
		if err := w.recoverSending(ctx); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// Wake tells every worker that new work may be due. It never blocks.
func (m *Manager) Wake() {
	for _, w := range m.workers {
		select {
		case w.wake <- struct{}{}:
		default:
		}
	}
}

// Running reports whether every worker is running.
func (m *Manager) Running() bool {
	for _, w := range m.workers {
		if !w.running.Load() {
			return false
		}
	}
	return len(m.workers) > 0
}

// Stop asks the workers to stop and waits until they have, or until ctx ends.
// A worker finishes its current send first.
func (m *Manager) Stop(ctx context.Context) error {
	if m.cancel != nil {
		m.cancel()
	}
	done := make(chan struct{})
	go func() {
		m.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

type worker struct {
	opts    *Options
	target  config.Target
	log     *slog.Logger
	wake    chan struct{}
	running atomic.Bool
}

func (w *worker) run(ctx context.Context) {
	for ctx.Err() == nil {
		err := w.recoverSending(context.WithoutCancel(ctx))
		if err == nil {
			break
		}
		w.log.Error("recovery of interrupted deliveries failed, will retry", "err", err)
		w.sleep(ctx, w.opts.PollInterval)
	}
	for ctx.Err() == nil {
		c, err := w.opts.Store.Claim(ctx, w.target.Name, w.opts.Now())
		if err != nil {
			if ctx.Err() == nil {
				w.log.Error("claim failed", "err", err)
				w.sleep(ctx, w.opts.PollInterval)
			}
			continue
		}
		if c == nil {
			w.wait(ctx)
			continue
		}
		// From here on the send runs to its end, even during shutdown.
		if !w.process(context.WithoutCancel(ctx), c) {
			w.sleep(ctx, w.opts.PollInterval) // the store is in trouble; do not spin
		}
	}
}

// wait sleeps until a wake signal, the next due time or the poll interval.
func (w *worker) wait(ctx context.Context) {
	d := w.opts.PollInterval
	next, ok, err := w.opts.Store.NextDue(ctx, w.target.Name)
	if err != nil && ctx.Err() == nil {
		w.log.Error("next due lookup failed", "err", err)
	}
	if ok {
		d = min(d, max(next.Sub(w.opts.Now()), time.Millisecond))
	}
	w.sleep(ctx, d)
}

func (w *worker) sleep(ctx context.Context, d time.Duration) {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-w.wake:
	case <-t.C:
	}
}

func (w *worker) policy(received time.Time) policy {
	return policy{
		onUnknown: w.target.OnUnknown,
		optional:  w.target.Optional,
		deadline:  received.Add(w.target.RetryWindow),
		backoff:   w.opts.Backoff,
	}
}

// recoverSending handles recipients left in sending by a crash (D6): before
// data_sent_at they are queued again, after it they are an unclear outcome.
func (w *worker) recoverSending(ctx context.Context) error {
	stuck, err := w.opts.Store.Sending(ctx, w.target.Name)
	if err != nil {
		return err
	}
	for _, st := range stuck {
		now := w.opts.Now()
		var d decision
		if !st.DataSent {
			d = decision{status: store.Queued, next: now, kind: store.KindDeliveryRetry, level: store.Warn,
				text: "interrupted before the message was sent, queued again"}
		} else {
			d = decide(Unclear, "interrupted after the message was sent", w.policy(st.ReceivedAt), now, st.Attempt)
		}
		r := store.Result{MailID: st.MailID, Target: w.target.Name, At: now, Attempt: st.Attempt, Text: d.text}
		for _, p := range st.Positions {
			r.Recipients = append(r.Recipients, d.result(p, 0))
		}
		r.NextAttempt = d.next
		if err := w.opts.Store.Finish(ctx, r); err != nil {
			return err
		}
		w.log.Warn("recovered interrupted delivery", "mail_id", st.MailID, "status", string(d.status),
			"recipients", len(st.Positions))
	}
	return nil
}

// process runs one attempt and stores its result. It reports false if the
// result could not be stored.
func (w *worker) process(ctx context.Context, c *store.Claimed) bool {
	if c.Raw == nil {
		// Retention removed the message between scheduling and this claim.
		return w.finish(ctx, w.all(c, decision{status: store.Failed, kind: store.KindRawExpired, level: w.problem(),
			text: "stored message was removed by retention; the delivery can no longer be sent"}, 0))
	}
	raw, err := w.opts.Sealer.Open(c.MailID, c.Raw)
	if err != nil {
		// Never send what cannot be authenticated. The error names the key id
		// at most, never content.
		w.log.Error("stored message cannot be decrypted, delivery failed", "mail_id", c.MailID, "err", err)
		return w.finish(ctx, w.all(c, decision{status: store.Failed, kind: store.KindDecryptFailed, level: store.Error,
			text: err.Error()}, 0))
	}
	res := w.send(ctx, c, raw)
	if res.storeErr != nil {
		// The message was not sent and nothing of this attempt was
		// recorded. Queue every recipient again; if the store is broken this
		// fails too and recovery handles the rows after a restart.
		w.log.Error("store failed during send, message not sent", "mail_id", c.MailID, "err", res.storeErr)
		now := w.opts.Now()
		w.finish(ctx, w.all(c, decision{status: store.Queued, next: now.Add(w.opts.Backoff(c.Attempt)),
			kind: store.KindDeliveryRetry, level: store.Warn, text: "internal storage error before sending"}, 0))
		return false // the store is in trouble; the caller slows down
	}

	now := w.opts.Now()
	r := store.Result{MailID: c.MailID, Target: c.Target, At: now, Attempt: c.Attempt}
	next := func(d decision) {
		if d.status == store.Queued && (r.NextAttempt.IsZero() || d.next.Before(r.NextAttempt)) {
			r.NextAttempt = d.next
		}
	}
	answered := make(map[int]bool, len(res.rejected))
	for _, rj := range res.rejected {
		answered[rj.result.Position] = true
		next(rj.decision)
		if !res.recorded {
			r.Recipients = append(r.Recipients, rj.result)
		}
	}
	// The recipients the target accepted, or that the attempt never got
	// to, share the outcome of the attempt.
	var d decision
	switch {
	case res.refused != "":
		d = decision{status: store.Failed, kind: res.refused, level: w.problem(), text: res.reason}
		r.Text = res.reason
	case res.err == nil && res.dataSent:
		d = decide(HandedOff, res.reply, w.policy(c.ReceivedAt), now, c.Attempt)
		r.Code, r.Text = 250, res.reply
	case res.err == nil:
		// Every recipient was refused at RCPT; the last refusal stands
		// for the attempt.
		if n := len(res.rejected); n > 0 {
			r.Code, r.Text = res.rejected[n-1].result.Code, res.rejected[n-1].result.Text
		}
	default:
		o, code, text := Classify(res.err, res.dataSent)
		d = decide(o, text, w.policy(c.ReceivedAt), now, c.Attempt)
		if o == Temporary && code == 0 && d.status == store.Queued {
			d.kind = store.KindTargetUnreachable
		}
		r.Code, r.Text = code, d.text
		if code != 0 {
			r.Text = text
		}
	}
	for _, rc := range c.Recipients {
		if answered[rc.Position] {
			continue
		}
		if d.status == "" {
			// Cannot happen: without an error every recipient was
			// answered. Keep the recipient rather than lose it.
			d = decision{status: store.Queued, next: now.Add(w.opts.Backoff(c.Attempt)), kind: store.KindDeliveryRetry,
				level: store.Warn, text: "internal: recipient without an outcome, queued again"}
		}
		r.Recipients = append(r.Recipients, d.result(rc.Position, r.Code))
		next(d)
	}
	return w.finish(ctx, r)
}

// problem is the level of a failure at this target.
func (w *worker) problem() store.Level {
	if w.target.Optional {
		return store.Warn
	}
	return store.Error
}

// all applies one decision to every recipient of the claim.
func (w *worker) all(c *store.Claimed, d decision, code int) store.Result {
	r := store.Result{MailID: c.MailID, Target: c.Target, At: w.opts.Now(), Attempt: c.Attempt,
		NextAttempt: d.next, Code: code, Text: d.text}
	for _, rc := range c.Recipients {
		r.Recipients = append(r.Recipients, d.result(rc.Position, code))
	}
	return r
}

// finish stores the result of an attempt and reports whether that worked.
func (w *worker) finish(ctx context.Context, r store.Result) bool {
	if err := w.opts.Store.Finish(ctx, r); err != nil {
		w.log.Error("storing the result of an attempt failed", "mail_id", r.MailID, "err", err)
		return false
	}
	return true
}

// rejection is a recipient the target refused at RCPT, or that Postwerk did
// not offer to the target.
type rejection struct {
	result   store.RecipientResult
	decision decision
}

// sendResult is the raw result of one attempt.
type sendResult struct {
	dataSent bool   // data_sent_at was recorded
	reply    string // the target's text after the message, on success
	err      error  // from the network or the target, for every recipient not in rejected
	storeErr error  // recording data_sent_at failed; nothing was sent or recorded
	refused  string // event kind: the mail was not sent because the target cannot take it
	reason   string // why, for the event
	rejected []rejection
	recorded bool // rejected was stored with data_sent_at
}

// rcptRejection turns a target's reply to RCPT into a result for that
// recipient alone. A 552 at RCPT means "too many recipients" and is
// temporary (RFC 5321 section 4.5.3.1.10); a 5xx otherwise is permanent for
// that recipient, a 4xx is retried with the next attempt.
func (w *worker) rcptRejection(c *store.Claimed, pos int, se *smtp.SMTPError, now time.Time) rejection {
	o, code, text := Classify(se, false)
	if code == 552 {
		o = Temporary
	}
	d := decide(o, text, w.policy(c.ReceivedAt), now, c.Attempt)
	return rejection{result: d.result(pos, code), decision: d}
}

// send makes one attempt with the decrypted message raw.
func (w *worker) send(ctx context.Context, c *store.Claimed, raw []byte) sendResult {
	client, err := w.connect(ctx)
	if err != nil {
		return sendResult{err: err}
	}
	defer client.Close()

	opts, reason, utf8Only := mailOptions(client, c, raw)
	if reason != "" {
		_ = client.Quit()
		return sendResult{refused: store.KindDeliveryUnsupported, reason: reason}
	}
	if w.target.Status == config.StatusMailjet {
		// Tracking must be unambiguous: only Postwerk's own X-MJ-CustomID
		// reaches Mailjet.
		raw = message.RemoveField(raw, message.HeaderMailjetCustomID)
	}
	headers := w.headers(c.MailID)
	size := message.Size(headers, raw)
	if limit, ok := client.MaxMessageSize(); ok && limit > 0 && size > limit {
		_ = client.Quit()
		return sendResult{refused: store.KindDeliveryTooLarge, reason: fmt.Sprintf(
			"the message has %d bytes, the target takes at most %d (SIZE); not sent", size, limit)}
	}
	opts.Size = int64(size) // sent as SIZE= where the target offers SIZE
	if w.target.Username != "" {
		if err := client.Auth(sasl.NewPlainClient("", w.target.Username, w.target.Password)); err != nil {
			return sendResult{err: err}
		}
	}
	if err := client.Mail(c.EnvelopeFrom, opts); err != nil {
		return sendResult{err: err}
	}
	var res sendResult
	accepted := 0
	for _, rc := range c.Recipients {
		if utf8Only[rc.Position] {
			d := decision{status: store.Failed, kind: store.KindDeliveryUnsupported, level: w.problem(),
				text: "the recipient address needs SMTPUTF8, which the target does not offer; not sent to it"}
			res.rejected = append(res.rejected, rejection{result: d.result(rc.Position, 0), decision: d})
			continue
		}
		if err := client.Rcpt(rc.Address, nil); err != nil {
			var se *smtp.SMTPError
			if !errors.As(err, &se) {
				res.err = err // the connection broke: the rest shares this outcome
				return res
			}
			res.rejected = append(res.rejected, w.rcptRejection(c, rc.Position, se, w.opts.Now()))
			continue
		}
		accepted++
	}
	if accepted == 0 {
		_ = client.Reset()
		_ = client.Quit()
		return res
	}
	wc, err := client.Data()
	if err != nil {
		res.err = err
		return res
	}
	// The target has answered 354 and waits for the message. From the
	// first byte on, a broken connection leaves it open whether the target
	// has it (D5). The recipients it refused are recorded now, so a crash
	// cannot make their clear answer unclear.
	rejected := make([]store.RecipientResult, len(res.rejected))
	for i, rj := range res.rejected {
		rejected[i] = rj.result
	}
	if err := w.opts.Store.MarkDataSent(ctx, c.MailID, c.Target, w.opts.Now(), c.Attempt, rejected); err != nil {
		return sendResult{storeErr: err}
	}
	res.recorded, res.dataSent = true, true
	if err := message.Write(wc, headers, raw); err != nil {
		res.err = err
		return res
	}
	resp, err := wc.CloseWithResponse()
	if err != nil {
		res.err = err
		return res
	}
	_ = client.Quit()
	res.reply = resp.StatusText
	return res
}

// mailOptions returns the MAIL parameters for this target, passing on what
// the sender declared (store.Params) where the target offers the extension.
// If the mail needs an extension the target does not offer, it returns the
// reason instead; the mail must then not be sent, because sending it without
// the parameter would hand the target data it did not agree to take.
//
//   - SMTPUTF8 is needed when the sender declared it and the envelope or the
//     header section really holds non-ASCII characters. Many clients (Go's
//     net/smtp, for one) declare SMTPUTF8 whenever it is offered; for a pure
//     ASCII mail the parameter changes nothing, so it is dropped where the
//     target lacks it. When only some recipient addresses need it, those
//     recipients are returned in utf8Only and fail alone; the others are
//     sent without it.
//   - 8BITMIME is needed when the message holds any byte outside 7-bit
//     ASCII, declared or not. The SMTP library sends BODY=8BITMIME to every
//     target that offers it; a 7-bit message is valid 8BITMIME as well.
func mailOptions(client *smtp.Client, c *store.Claimed, raw []byte) (opts *smtp.MailOptions, reason string,
	utf8Only map[int]bool) {
	opts = &smtp.MailOptions{}
	if c.Params.SMTPUTF8 {
		hasUTF8, _ := client.Extension("SMTPUTF8")
		switch {
		case hasUTF8:
			opts.UTF8 = true
		case !isASCII(c.EnvelopeFrom) || message.HeaderHas8Bit(raw):
			return nil, "the mail needs SMTPUTF8 (non-ASCII sender address or header), which the target does not offer; not sent", nil
		default:
			for _, rc := range c.Recipients {
				if !isASCII(rc.Address) {
					if utf8Only == nil {
						utf8Only = make(map[int]bool)
					}
					utf8Only[rc.Position] = true
				}
			}
		}
	}
	if has8Bit, _ := client.Extension("8BITMIME"); !has8Bit && message.Has8Bit(raw) {
		return nil, "the message has 8-bit content and needs 8BITMIME, which the target does not offer; not sent", nil
	}
	return opts, "", utf8Only
}

func isASCII(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] >= 0x80 {
			return false
		}
	}
	return true
}

func (w *worker) headers(mailID string) []message.Field {
	h := []message.Field{{Name: message.HeaderID, Value: mailID}}
	if w.target.Status == config.StatusMailjet {
		h = append(h, message.Field{Name: message.HeaderMailjetCustomID, Value: mailID})
	}
	return h
}

func (w *worker) tlsConfig() *tls.Config {
	return &tls.Config{
		ServerName: w.target.Host,
		MinVersion: tls.VersionTLS12,
		RootCAs:    w.opts.rootCAs,
	}
}

// connect dials the target per its scheme (D7) and says EHLO.
func (w *worker) connect(ctx context.Context) (*smtp.Client, error) {
	d := net.Dialer{Timeout: w.opts.DialTimeout}
	raw, err := d.DialContext(ctx, "tcp", w.target.Addr)
	if err != nil {
		return nil, err
	}
	conn := &boundConn{Conn: raw, hard: time.Now().Add(w.opts.AttemptTimeout), soft: w.opts.CommandTimeout}
	if err := conn.SetDeadline(time.Time{}); err != nil {
		conn.Close()
		return nil, err
	}

	var client *smtp.Client
	switch w.target.Scheme {
	case config.SchemeSMTPS:
		client = smtp.NewClient(tls.Client(conn, w.tlsConfig()))
	case config.SchemeSTARTTLS:
		// NewClientStartTLS greets, says EHLO and starts TLS with the
		// library's default timeouts; the soft limit caps them.
		client, err = smtp.NewClientStartTLS(conn, w.tlsConfig())
		if err != nil {
			conn.Close()
			return nil, err
		}
	case config.SchemePlain:
		client = smtp.NewClient(conn)
	default:
		conn.Close()
		return nil, errors.New("unsupported scheme")
	}
	client.CommandTimeout = w.opts.CommandTimeout
	client.SubmissionTimeout = w.opts.SubmissionTimeout
	conn.soft = 0 // the client's own timeouts apply from here on
	if err := client.Hello(w.opts.HeloName); err != nil {
		client.Close()
		return nil, err
	}
	return client, nil
}

// maxTargetBytes bounds what one attempt reads from a target. Replies are a
// few hundred bytes; the SMTP client limits the length of a line but not the
// number of lines in a multi-line reply, which it keeps in memory.
const maxTargetBytes = 1 << 20

var errTooMuchFromTarget = errors.New("target sent more than 1 MiB of replies in one attempt")

// boundConn caps every deadline the SMTP client sets: never later than hard
// (the end of the attempt), and while soft is set, never later than soft
// from now. A cleared deadline becomes hard, so writing the message body
// cannot hang forever either. It also stops reading after maxTargetBytes.
type boundConn struct {
	net.Conn
	hard time.Time
	soft time.Duration
	read int
}

func (c *boundConn) Read(b []byte) (int, error) {
	if c.read >= maxTargetBytes {
		return 0, errTooMuchFromTarget
	}
	if rest := maxTargetBytes - c.read; len(b) > rest {
		b = b[:rest]
	}
	n, err := c.Conn.Read(b)
	c.read += n
	return n, err
}

func (c *boundConn) bound(t time.Time) time.Time {
	if c.soft > 0 {
		if s := time.Now().Add(c.soft); t.IsZero() || t.After(s) {
			t = s
		}
	}
	if t.IsZero() || t.After(c.hard) {
		t = c.hard
	}
	return t
}

func (c *boundConn) SetDeadline(t time.Time) error      { return c.Conn.SetDeadline(c.bound(t)) }
func (c *boundConn) SetReadDeadline(t time.Time) error  { return c.Conn.SetReadDeadline(c.bound(t)) }
func (c *boundConn) SetWriteDeadline(t time.Time) error { return c.Conn.SetWriteDeadline(c.bound(t)) }
