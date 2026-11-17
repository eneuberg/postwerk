package query

import (
	"cmp"
	"context"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/eneuberg/postwerk/internal/store"
)

// Statistics (D73–D75): throughput and outcomes over a range of time, per
// bucket, per target and per sending application. Counts only; nothing here
// names a recipient or holds content.
//
// A mail counts in the bucket it was received in, with the state it has now
// (the state rule of the lists: over its required targets, or at the
// filtered target). Retries, rejected recipients, recipients outside an
// allow-list and ingress refusals are events and count at the time they
// happened; so does the time a target was down.

// StatsPreset is a range that ends now.
type StatsPreset struct {
	Name string
	Span time.Duration
}

// StatsPresets are the ranges offered, shortest first.
var StatsPresets = []StatsPreset{
	{"1h", time.Hour}, {"6h", 6 * time.Hour}, {"24h", 24 * time.Hour},
	{"7d", 7 * 24 * time.Hour}, {"30d", 30 * 24 * time.Hour},
}

const (
	// DefaultStatsRange is the preset without a choice.
	DefaultStatsRange = "24h"
	// CustomRange is the range value for From and To.
	CustomRange = "custom"
	// MaxStatsBuckets is the most buckets a range below two weeks gets; longer
	// ranges have one bucket per day (or week, beyond 200 days).
	MaxStatsBuckets = 72
	// maxStatsSpan bounds a custom range when retention does not.
	maxStatsSpan = 400 * 24 * time.Hour
)

// statsWidths are the bucket widths, each a divisor or a multiple of an
// hour (store.StatsChunk).
var statsWidths = []time.Duration{
	time.Minute, 2 * time.Minute, 5 * time.Minute, 10 * time.Minute, 15 * time.Minute, 30 * time.Minute,
	time.Hour, 2 * time.Hour, 3 * time.Hour, 6 * time.Hour, 12 * time.Hour,
}

// BucketWidth is the bucket width for a range of span: the narrowest that
// gives at most MaxStatsBuckets, a day from two weeks on, a week from 200
// days on. The presets get 1 min, 5 min, 30 min, 3 h and 1 day.
func BucketWidth(span time.Duration) time.Duration {
	switch {
	case span > 200*24*time.Hour:
		return 7 * 24 * time.Hour
	case span > 14*24*time.Hour:
		return 24 * time.Hour
	}
	for _, w := range statsWidths {
		if (span+w-1)/w <= MaxStatsBuckets {
			return w
		}
	}
	return 24 * time.Hour
}

// StatsRange is a requested range.
type StatsRange struct {
	// Preset names a preset; empty for a custom range.
	Preset string
	// From and To: the custom range, From ≤ t < To. Zero To means now.
	From, To time.Time
}

// statsTimeLayouts are the forms a custom From or To may take, all UTC
// unless they carry an offset.
var statsTimeLayouts = []string{time.RFC3339, "2006-01-02T15:04:05", "2006-01-02T15:04", time.DateOnly}

// ParseStatsRange reads the range parameters: a preset (range=24h), or a
// custom range (range=custom, or no range) with from and optionally to. A
// date alone means its start for from and its end for to. strict refuses
// from and to beside a preset (the API); the UI's form sends them always
// and they are ignored then.
func ParseStatsRange(rng, from, to string, strict bool) (StatsRange, error) {
	custom := rng == CustomRange || (rng == "" && (from != "" || to != ""))
	if !custom {
		if rng == "" {
			rng = DefaultStatsRange
		}
		if !slices.ContainsFunc(StatsPresets, func(p StatsPreset) bool { return p.Name == rng }) {
			return StatsRange{}, fmt.Errorf("%w: range", ErrInvalid)
		}
		if strict && (from != "" || to != "") {
			return StatsRange{}, fmt.Errorf("%w: from and to need range=custom", ErrInvalid)
		}
		return StatsRange{Preset: rng}, nil
	}
	var r StatsRange
	var err error
	if from == "" {
		return StatsRange{}, fmt.Errorf("%w: from", ErrInvalid)
	}
	if r.From, err = parseStatsTime(from, false); err != nil {
		return StatsRange{}, fmt.Errorf("%w: from", ErrInvalid)
	}
	if to != "" {
		if r.To, err = parseStatsTime(to, true); err != nil {
			return StatsRange{}, fmt.Errorf("%w: to", ErrInvalid)
		}
		if !r.From.Before(r.To) {
			return StatsRange{}, fmt.Errorf("%w: from is not before to", ErrInvalid)
		}
	}
	return r, nil
}

func parseStatsTime(s string, end bool) (time.Time, error) {
	if len(s) > 40 {
		return time.Time{}, fmt.Errorf("too long")
	}
	for _, layout := range statsTimeLayouts {
		t, err := time.Parse(layout, s)
		if err != nil {
			continue
		}
		if layout == time.DateOnly && end {
			t = t.AddDate(0, 0, 1)
		}
		return t.UTC(), nil
	}
	return time.Time{}, fmt.Errorf("not a time")
}

// StatsRequest asks for statistics.
type StatsRequest struct {
	Range StatsRange
	// Target and Sender filter; empty: all.
	Target, Sender string
}

// Counts are the outcomes of mails (or of deliveries, per target).
type Counts struct {
	// Total counts mails received (per target: deliveries).
	Total int
	// By derived state; InFlight is queued or sending.
	HandedOff, Partial, Failed, Unknown, InFlight, Skipped int
	// Events: recipients the target refused for good (delivery.rejected,
	// part of Failed or Partial), recipients outside an allow-list
	// (delivery.not_allowed, part of Skipped) and retries.
	Rejected, NotAllowed, Retries int
}

// Settled counts the outcomes that are over and expected a hand-off: the
// base of the success rate. Skipped and in-flight mails are left out;
// unknown ones count against it until someone closes them.
func (c Counts) Settled() int { return c.HandedOff + c.Partial + c.Failed + c.Unknown }

// SuccessRate is the share of settled mails handed off for every recipient;
// ok is false when nothing settled.
func (c Counts) SuccessRate() (rate float64, ok bool) {
	if c.Settled() == 0 {
		return 0, false
	}
	return float64(c.HandedOff) / float64(c.Settled()), true
}

func (c *Counts) addState(st store.Status, n int) {
	c.Total += n
	switch st {
	case store.HandedOff:
		c.HandedOff += n
	case store.Partial:
		c.Partial += n
	case store.Failed:
		c.Failed += n
	case store.Unknown:
		c.Unknown += n
	case store.Queued, store.Sending:
		c.InFlight += n
	case store.Skipped:
		c.Skipped += n
	}
}

func (c *Counts) addEvent(kind string, n int) {
	switch kind {
	case store.KindDeliveryRejected:
		c.Rejected += n
	case store.KindDeliveryNotAllowed:
		c.NotAllowed += n
	case store.KindDeliveryRetry:
		c.Retries += n
	}
}

// Latency is the time from acceptance until the target took a delivery
// (the last of its recipients the relay handed off), over Count deliveries.
// Percentiles are rounded down to store.LatencyStep.
type Latency struct {
	Count    int
	P50, P95 time.Duration
}

// Ingress counts what ingress refused (events, by their time).
type Ingress struct {
	// AuthFailed: failed AUTH attempts; Rejected: refused commands of a
	// logged-in sender; RateLimited: refusals for sending too fast.
	AuthFailed, Rejected, RateLimited int
}

// Total is every refusal.
func (i Ingress) Total() int { return i.AuthFailed + i.Rejected + i.RateLimited }

func (i *Ingress) add(kind string, n int) {
	switch kind {
	case kindIngressAuthFailed:
		i.AuthFailed += n
	case kindIngressRejected:
		i.Rejected += n
	case kindIngressRateLimited:
		i.RateLimited += n
	}
}

// Ingress event kinds (internal/ingress), with the count each folds.
const (
	kindIngressAuthFailed  = "ingress.auth_failed"
	kindIngressRejected    = "ingress.rejected"
	kindIngressRateLimited = "ingress.rate_limited"
)

// statsEvents are the event kinds the statistics count, with the key of
// the count in their text.
var statsEvents = map[string]string{
	store.KindDeliveryRetry: "", store.KindDeliveryRejected: "", store.KindDeliveryNotAllowed: "",
	kindIngressAuthFailed: "failures", kindIngressRejected: "rejected", kindIngressRateLimited: "limited",
}

// Bucket is one bar of the chart: mails received in [Start, End).
type Bucket struct {
	Start, End time.Time
	Counts
	Ingress Ingress
}

// Outage is a time a target was down, within the range.
type Outage struct {
	From, To time.Time
	// Open: still down at the end of the range.
	Open bool
}

// TargetStats is one target over the range; its Counts are deliveries.
type TargetStats struct {
	Target     string
	Configured bool
	Optional   bool
	Counts
	Latency Latency
	// Down is the time the target was down within the range (target.down
	// to target.recovered); DownNow is its state now.
	Down    time.Duration
	Outages []Outage
	DownNow bool
}

// SenderStats is one sending application over the range; its Counts are
// mails. An empty Sender holds failed logins with an unknown name.
type SenderStats struct {
	Sender string
	Counts
	Ingress Ingress
}

// Stats is the statistics page's data.
type Stats struct {
	Now time.Time
	// Preset is the preset's name, empty for a custom range.
	Preset string
	// From and To are the range counted: From is aligned to a bucket (UTC),
	// To is at most Now.
	From, To time.Time
	Width    time.Duration
	// Retained is the oldest time retention still holds (zero when the
	// retention is not known); Clamped says that the range asked for began
	// before it and was cut.
	Retained time.Time
	Clamped  bool
	// RecordRetention and EventRetention as configured (zero when unknown).
	RecordRetention, EventRetention time.Duration
	Target, Sender                  string
	Totals                          Counts
	Latency                         Latency
	Ingress                         Ingress
	Buckets                         []Bucket
	Targets                         []TargetStats
	Senders                         []SenderStats
}

// Empty reports that nothing at all happened in the range.
func (s Stats) Empty() bool {
	return s.Totals.Total == 0 && s.Ingress.Total() == 0 && !slices.ContainsFunc(s.Targets, func(t TargetStats) bool {
		return t.Total > 0 || t.Down > 0
	})
}

// Senders are application logins, never addresses.
var senderRe = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)

// Stats counts what happened in a range (D73–D75).
func (s *Service) Stats(ctx context.Context, req StatsRequest) (Stats, error) {
	if req.Target != "" && !targetRe.MatchString(req.Target) {
		return Stats{}, fmt.Errorf("%w: target", ErrInvalid)
	}
	if req.Sender != "" && !senderRe.MatchString(req.Sender) {
		return Stats{}, fmt.Errorf("%w: sender", ErrInvalid)
	}
	now := s.opts.Now().UTC()
	st := Stats{Now: now, Preset: req.Range.Preset, To: now, Target: req.Target, Sender: req.Sender,
		RecordRetention: s.opts.RecordRetention, EventRetention: s.opts.EventRetention}
	if p := slices.IndexFunc(StatsPresets, func(p StatsPreset) bool { return p.Name == req.Range.Preset }); p >= 0 {
		st.From = now.Add(-StatsPresets[p].Span)
	} else if req.Range.Preset != "" {
		return Stats{}, fmt.Errorf("%w: range", ErrInvalid)
	} else {
		st.From = req.Range.From.UTC()
		if !req.Range.To.IsZero() && req.Range.To.Before(now) {
			st.To = req.Range.To.UTC()
		}
		if !st.From.Before(st.To) {
			return Stats{}, fmt.Errorf("%w: from is not before now", ErrInvalid)
		}
	}
	if keep := min(nonZero(s.opts.RecordRetention), nonZero(s.opts.EventRetention)); keep < time.Duration(1<<63-1) {
		st.Retained = now.Add(-keep)
		if st.From.Before(st.Retained) {
			st.From, st.Clamped = st.Retained, true
		}
		if !st.From.Before(st.To) {
			return Stats{}, fmt.Errorf("%w: the range ends before the oldest record retention keeps", ErrInvalid)
		}
	}
	if st.To.Sub(st.From) > maxStatsSpan {
		return Stats{}, fmt.Errorf("%w: range too long", ErrInvalid)
	}
	st.Width = BucketWidth(st.To.Sub(st.From))
	st.From = time.UnixMilli(st.From.UnixMilli() - st.From.UnixMilli()%st.Width.Milliseconds()).UTC()

	rows, err := s.opts.Store.Stats(ctx, store.StatsFilter{
		From: st.From, To: st.To, Width: st.Width, Target: req.Target, Sender: req.Sender,
		Optional: s.opts.Optional, Events: statsEvents, Now: now, Retained: st.Retained,
	})
	if err != nil {
		return Stats{}, err
	}
	before, during, err := s.opts.Store.TargetTransitions(ctx, st.From, st.To)
	if err != nil {
		return Stats{}, err
	}
	s.fill(&st, rows, before, during)
	return st, nil
}

func nonZero(d time.Duration) time.Duration {
	if d <= 0 {
		return time.Duration(1<<63 - 1)
	}
	return d
}

func (s *Service) optional(target string) bool { return slices.Contains(s.opts.Optional, target) }

// fill turns the store's rows into the totals and tables.
func (s *Service) fill(st *Stats, rows store.StatsRows, before, during []store.TargetTransition) {
	n := int((st.To.Sub(st.From) + st.Width - 1) / st.Width)
	st.Buckets = make([]Bucket, n)
	for i := range st.Buckets {
		b := &st.Buckets[i]
		b.Start = st.From.Add(time.Duration(i) * st.Width)
		b.End = b.Start.Add(st.Width)
		if b.End.After(st.To) {
			b.End = st.To
		}
	}
	bucket := func(t time.Time) *Bucket {
		i := int(t.Sub(st.From) / st.Width)
		if t.Before(st.From) || i >= n {
			return nil
		}
		return &st.Buckets[i]
	}

	// Targets: the configured ones in their order, then any other seen.
	targets := map[string]*TargetStats{}
	var order []string
	target := func(name string) *TargetStats {
		if t := targets[name]; t != nil {
			return t
		}
		t := &TargetStats{Target: name, Configured: slices.Contains(s.opts.Targets, name), Optional: s.optional(name)}
		targets[name] = t
		order = append(order, name)
		return t
	}
	if st.Target != "" {
		target(st.Target)
	} else {
		for _, name := range s.opts.Targets {
			target(name)
		}
	}
	senders := map[string]*SenderStats{}
	sender := func(name string) *SenderStats {
		if x := senders[name]; x != nil {
			return x
		}
		x := &SenderStats{Sender: name}
		senders[name] = x
		return x
	}

	for _, r := range rows.Mails {
		st.Totals.addState(r.State, r.Count)
		sender(r.Sender).addState(r.State, r.Count)
		if b := bucket(r.Bucket); b != nil {
			b.addState(r.State, r.Count)
		}
	}
	for _, r := range rows.Deliveries {
		target(r.Target).addState(r.State, r.Count)
	}
	// An event about a delivery counts for the mail's totals when the
	// target is required, or the one filtered for.
	counts := func(t string) bool { return t != "" && (st.Target != "" || !s.optional(t)) }
	for _, r := range rows.Events {
		b := bucket(r.Bucket)
		if strings.HasPrefix(r.Kind, "ingress.") {
			st.Ingress.add(r.Kind, r.Count)
			sender(r.Sender).Ingress.add(r.Kind, r.Count)
			if b != nil {
				b.Ingress.add(r.Kind, r.Count)
			}
			continue
		}
		if r.Target != "" {
			target(r.Target).addEvent(r.Kind, r.Count)
		}
		if counts(r.Target) {
			st.Totals.addEvent(r.Kind, r.Count)
			if r.Sender != "" {
				sender(r.Sender).addEvent(r.Kind, r.Count)
			}
			if b != nil {
				b.addEvent(r.Kind, r.Count)
			}
		}
	}

	// Latency: per target, and over the required (or filtered) targets.
	hist := map[string]map[int64]int{}
	overall := map[int64]int{}
	for _, r := range rows.Latency {
		if hist[r.Target] == nil {
			hist[r.Target] = map[int64]int{}
		}
		hist[r.Target][r.Millis] += r.Count
		if counts(r.Target) {
			overall[r.Millis] += r.Count
		}
	}
	for name, h := range hist {
		target(name).Latency = percentiles(h)
	}
	st.Latency = percentiles(overall)

	// Down time per target.
	health := map[string]TargetHealth{}
	if s.opts.Health != nil {
		for _, h := range s.opts.Health() {
			health[h.Target] = h
		}
	}
	for name, outs := range outages(before, during, st.From, st.To, health) {
		if st.Target != "" && name != st.Target {
			continue
		}
		t := target(name)
		t.Outages = outs
		for _, o := range outs {
			t.Down += o.To.Sub(o.From)
		}
	}
	for name, h := range health {
		if t := targets[name]; t != nil {
			t.DownNow = h.Down
		}
	}

	for _, name := range order {
		st.Targets = append(st.Targets, *targets[name])
	}
	for _, x := range senders {
		st.Senders = append(st.Senders, *x)
	}
	// Busiest first; the unknown sender of failed logins last.
	slices.SortFunc(st.Senders, func(a, b SenderStats) int {
		if (a.Sender == "") != (b.Sender == "") {
			if a.Sender == "" {
				return 1
			}
			return -1
		}
		return cmp.Or(cmp.Compare(b.Total, a.Total), cmp.Compare(b.Ingress.Total(), a.Ingress.Total()),
			cmp.Compare(a.Sender, b.Sender))
	})
}

// percentiles reads p50 and p95 from a histogram of latency keys.
func percentiles(h map[int64]int) Latency {
	var l Latency
	keys := make([]int64, 0, len(h))
	for k, n := range h {
		keys = append(keys, k)
		l.Count += n
	}
	if l.Count == 0 {
		return l
	}
	slices.Sort(keys)
	at := func(p float64) time.Duration {
		// The smallest key with at least p of all values at or below it.
		need, seen := int(p*float64(l.Count)+0.999999), 0
		for _, k := range keys {
			seen += h[k]
			if seen >= need {
				return time.Duration(k) * time.Millisecond
			}
		}
		return time.Duration(keys[len(keys)-1]) * time.Millisecond
	}
	l.P50, l.P95 = at(0.50), at(0.95)
	return l
}

// outages turns the down and recovered events into the times each target
// was down within [from, to). A target down at the end is down until to,
// unless its worker has seen it up since (after a restart): then until
// then.
func outages(before, during []store.TargetTransition, from, to time.Time, health map[string]TargetHealth) map[string][]Outage {
	out := map[string][]Outage{}
	downSince := map[string]time.Time{}
	for _, t := range before {
		if t.Down {
			downSince[t.Target] = from
		}
	}
	for _, t := range during {
		start, down := downSince[t.Target]
		switch {
		case t.Down && !down:
			downSince[t.Target] = t.Time
		case !t.Down && down:
			out[t.Target] = append(out[t.Target], Outage{From: start, To: t.Time})
			delete(downSince, t.Target)
		}
	}
	for name, start := range downSince {
		o := Outage{From: start, To: to, Open: true}
		if h, ok := health[name]; ok && !h.Down && h.Since.After(start) && h.Since.Before(to) {
			o.To, o.Open = h.Since, false
		}
		out[name] = append(out[name], o)
	}
	return out
}
