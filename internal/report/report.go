// Package report exports the event log to sinks (A2, A3, A4).
//
// Each sink keeps its own cursor (the last exported seq) in the store and
// runs on its own: a slow or failing sink never holds up another. A sink
// that fails keeps its cursor and catches up on the next round; nothing is
// lost while it is down. Delivery is at least once: after a crash between
// export and cursor update, a few events can be exported twice.
package report

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"time"

	"github.com/eneuberg/postwerk/internal/store"
)

// Sink exports events.
type Sink interface {
	// Name identifies the sink's cursor. It must stay stable.
	Name() string
	// Export exports one event. An error stops this round for the sink.
	Export(ctx context.Context, e store.Event) error
}

// ErrDeferred marks an export error the sink has already logged, such as a
// remote end that is down or asks to wait. The reporter retries the event
// later without logging it again.
var ErrDeferred = errors.New("export deferred")

// DefaultInterval is how often the reporter looks for events without a wake
// signal.
const DefaultInterval = 5 * time.Second

const batchSize = 256

// Options configure a Reporter.
type Options struct {
	Store    *store.Store
	Sinks    []Sink
	Interval time.Duration
	Logger   *slog.Logger
}

// Reporter moves events from the store to the sinks.
type Reporter struct {
	opts  Options
	sinks []*runner
}

// runner is one sink with its own wake signal and lock.
type runner struct {
	sink Sink
	wake chan struct{}
	mu   sync.Mutex // one flush of this sink at a time
}

// New creates a Reporter.
func New(opts Options) *Reporter {
	if opts.Interval <= 0 {
		opts.Interval = DefaultInterval
	}
	if opts.Logger == nil {
		opts.Logger = slog.New(slog.DiscardHandler)
	}
	r := &Reporter{opts: opts}
	for _, s := range opts.Sinks {
		r.sinks = append(r.sinks, &runner{sink: s, wake: make(chan struct{}, 1)})
	}
	return r
}

// Wake asks every sink for a flush soon. It never blocks.
func (r *Reporter) Wake() {
	for _, s := range r.sinks {
		select {
		case s.wake <- struct{}{}:
		default:
		}
	}
}

// Run flushes each sink on every wake signal and every interval until ctx
// ends. Every sink runs in its own goroutine.
func (r *Reporter) Run(ctx context.Context) {
	var wg sync.WaitGroup
	for _, s := range r.sinks {
		wg.Go(func() { r.run(ctx, s) })
	}
	wg.Wait()
}

func (r *Reporter) run(ctx context.Context, s *runner) {
	t := time.NewTicker(r.opts.Interval)
	defer t.Stop()
	for {
		if err := r.flushSink(ctx, s); err != nil && ctx.Err() == nil && !errors.Is(err, ErrDeferred) {
			r.opts.Logger.Warn("event export incomplete, will retry", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-s.wake:
		case <-t.C:
		}
	}
}

// Flush exports every pending event to every sink, all sinks at once. It
// returns the first error in sink order; other sinks are still served.
func (r *Reporter) Flush(ctx context.Context) error {
	errs := make([]error, len(r.sinks))
	var wg sync.WaitGroup
	for i, s := range r.sinks {
		wg.Go(func() { errs[i] = r.flushSink(ctx, s) })
	}
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			return err
		}
	}
	return nil
}

func (r *Reporter) flushSink(ctx context.Context, run *runner) error {
	run.mu.Lock()
	defer run.mu.Unlock()
	s := run.sink
	cursor, err := r.opts.Store.Cursor(ctx, s.Name())
	if err != nil {
		return err
	}
	for {
		events, err := r.opts.Store.EventsAfter(ctx, cursor, batchSize)
		if err != nil {
			return err
		}
		if len(events) == 0 {
			return nil
		}
		done := cursor
		var exportErr error
		for _, e := range events {
			if exportErr = s.Export(ctx, e); exportErr != nil {
				break
			}
			done = e.Seq
		}
		if done != cursor {
			if err := r.opts.Store.SetCursor(ctx, s.Name(), done); err != nil {
				return err
			}
			cursor = done
		}
		if exportErr != nil {
			return fmt.Errorf("sink %s: %w", s.Name(), exportErr)
		}
		if len(events) < batchSize {
			return nil
		}
	}
}

// Stdout writes one JSON line per event (A3).
type Stdout struct {
	w        io.Writer
	instance string
}

// NewStdout returns a sink that writes to w. instance is added to every line
// if not empty.
func NewStdout(w io.Writer, instance string) *Stdout {
	return &Stdout{w: w, instance: instance}
}

// Name implements Sink.
func (s *Stdout) Name() string { return "stdout" }

type smtpReply struct {
	Code int    `json:"code,omitempty"`
	Text string `json:"text,omitempty"`
}

// FromSMTPReply reports whether an event's text comes with an SMTP reply:
// it has a reply code. Such events carry code and text as "smtp"; every
// other event (a network error, a certificate, retention counts, a UI or
// API action) carries its text as "text".
func FromSMTPReply(e store.Event) bool { return e.Code != 0 }

// line is the event shape of design.md section 8. It has no field for
// content or addresses.
type line struct {
	Seq      int64      `json:"seq"`
	Time     string     `json:"time"`
	Level    string     `json:"level"`
	Kind     string     `json:"kind"`
	MailID   string     `json:"mail_id,omitempty"`
	Target   string     `json:"target,omitempty"`
	Rcpt     *int       `json:"recipient,omitempty"` // position, never the address
	Attempt  int        `json:"attempt,omitempty"`
	SMTP     *smtpReply `json:"smtp,omitempty"`
	Sender   string     `json:"sender,omitempty"`
	Actor    string     `json:"actor,omitempty"` // token name for UI events, never an address
	Text     string     `json:"text,omitempty"`  // free text of every event that is not an SMTP reply, never a query
	Instance string     `json:"instance,omitempty"`
}

// Export implements Sink.
func (s *Stdout) Export(_ context.Context, e store.Event) error {
	l := line{
		Seq: e.Seq, Time: e.Time.UTC().Format("2006-01-02T15:04:05.000Z"), Level: string(e.Level), Kind: e.Kind,
		MailID: e.MailID, Target: e.Target, Rcpt: e.Recipient, Attempt: e.Attempt, Sender: e.Sender, Actor: e.Actor,
		Instance: s.instance,
	}
	if FromSMTPReply(e) {
		l.SMTP = &smtpReply{Code: e.Code, Text: e.Text}
	} else {
		l.Text = e.Text
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false) // keep "<redacted>" readable
	if err := enc.Encode(l); err != nil {
		return err
	}
	_, err := s.w.Write(buf.Bytes()) // one write per line
	return err
}
