// Package report exports the event log to sinks (A2, A3).
//
// Each sink keeps its own cursor (the last exported seq) in the store. A sink
// that fails keeps its cursor and catches up on the next round; nothing is
// lost while it is down. Delivery is at least once: after a crash between
// export and cursor update, a few events can be exported twice.
package report

import (
	"bytes"
	"context"
	"encoding/json"
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
	opts Options
	wake chan struct{}
	mu   sync.Mutex // one flush at a time
}

// New creates a Reporter.
func New(opts Options) *Reporter {
	if opts.Interval <= 0 {
		opts.Interval = DefaultInterval
	}
	if opts.Logger == nil {
		opts.Logger = slog.New(slog.DiscardHandler)
	}
	return &Reporter{opts: opts, wake: make(chan struct{}, 1)}
}

// Wake asks for a flush soon. It never blocks.
func (r *Reporter) Wake() {
	select {
	case r.wake <- struct{}{}:
	default:
	}
}

// Run flushes on every wake signal and every interval until ctx ends.
func (r *Reporter) Run(ctx context.Context) {
	t := time.NewTicker(r.opts.Interval)
	defer t.Stop()
	for {
		if err := r.Flush(ctx); err != nil && ctx.Err() == nil {
			r.opts.Logger.Warn("event export incomplete, will retry", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-r.wake:
		case <-t.C:
		}
	}
}

// Flush exports every pending event to every sink. It returns the first
// error; other sinks are still served.
func (r *Reporter) Flush(ctx context.Context) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	var first error
	for _, s := range r.opts.Sinks {
		if err := r.flushSink(ctx, s); err != nil && first == nil {
			first = err
		}
	}
	return first
}

func (r *Reporter) flushSink(ctx context.Context, s Sink) error {
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
	Text     string     `json:"text,omitempty"`  // detail of a UI event (role, search kind), never a query
	Instance string     `json:"instance,omitempty"`
}

// Export implements Sink.
func (s *Stdout) Export(_ context.Context, e store.Event) error {
	l := line{
		Seq: e.Seq, Time: e.Time.UTC().Format("2006-01-02T15:04:05.000Z"), Level: string(e.Level), Kind: e.Kind,
		MailID: e.MailID, Target: e.Target, Rcpt: e.Recipient, Attempt: e.Attempt, Sender: e.Sender, Actor: e.Actor,
		Instance: s.instance,
	}
	switch {
	case e.Actor != "":
		// An event caused by a person: its text is not an SMTP reply.
		l.Text = e.Text
	case e.Code != 0 || e.Text != "":
		l.SMTP = &smtpReply{Code: e.Code, Text: e.Text}
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
