package store

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"
)

// Reads for the statistics (D73–D75). internal/query turns them into
// totals, percentiles and tables; the SQL stays here. Everything is a count:
// no address, no text but the counts inside folded events.
//
// The store has a single connection, so a long read would hold up ingress
// and the workers. Stats therefore reads one hour of received or event time
// per transaction (StatsChunk), streams the rows of that hour and counts in
// Go with the same rule as Summarize. An hour that is over is kept in memory
// (statsCache) until an event about one of its mails says that something
// changed: every change of a recipient's status is written together with
// such an event, so the event log is the change log of the counts.

// StatsChunk is the time one transaction of Stats reads, and the unit the
// cache keeps. Bucket widths divide it or are multiples of it.
const StatsChunk = time.Hour

// StatsCacheCells bounds the counts the cache keeps (32 bytes each); an
// hour takes about 100, so this is a little more than 90 days. An entry
// itself counts as statsEntryCells more, so hours without any count (a
// filter for a sender that sent nothing) are bounded too.
const StatsCacheCells = 250_000

// statsEntryCells is about what an entry costs besides its cells (key,
// pointer, map slot), in cells.
const statsEntryCells = 4

// StatsFilter selects what Stats counts. A bucket starts at a multiple of
// Width since the Unix epoch, so buckets are aligned in UTC.
type StatsFilter struct {
	// From and To bound the received time of mails and the time of events:
	// From ≤ t < To.
	From, To time.Time
	// Width is the bucket width: it divides StatsChunk or is a multiple of
	// it.
	Width time.Duration
	// Target, if set, counts only deliveries to this target, and a mail's
	// state is its state at this target (as MailFilter.Target).
	Target string
	// Sender, if set, counts only mails and events of this sending
	// application.
	Sender string
	// Optional names the optional targets: without Target, a mail's state
	// leaves them out (MailStatus).
	Optional []string
	// Events are the event kinds to count by event time, each with the key
	// of the count in its text for events that fold several occurrences
	// ("failures" for failures=<n>); empty: each event counts one.
	Events map[string]string
	// Now and Retained decide which hours may be cached: those that ended
	// by Now and start at or after Retained, before which retention
	// deletes rows without writing an event about them.
	Now, Retained time.Time
}

// StateRow counts mails or deliveries by derived state in one bucket.
type StateRow struct {
	Bucket time.Time
	// Target is set for deliveries, Sender for mails.
	Target string
	Sender string
	State  Status
	Count  int
}

// LatencyRow counts deliveries by the time from acceptance to the last
// hand-off of one of its recipients by the relay itself (not an admin's
// close), rounded down to LatencyStep.
type LatencyRow struct {
	Target string
	Millis int64
	Count  int
}

// EventRow counts events of one kind in one bucket, by event time. Target
// and Sender are empty when the event has none.
type EventRow struct {
	Bucket time.Time
	Target string
	Sender string
	Kind   string
	Count  int
}

// StatsRows is what Stats counted, each key once, in no particular order.
type StatsRows struct {
	// Mails: mails received in each bucket, by sender and derived state
	// (over the required targets, or at Target).
	Mails []StateRow
	// Deliveries: deliveries (a mail at one target) of mails received in
	// each bucket, by target and their state at that target.
	Deliveries []StateRow
	// Latency: over the whole range, per target.
	Latency []LatencyRow
	// Events: the counted kinds by event time.
	Events []EventRow
}

// LatencyStep is the resolution of a latency of ms milliseconds: 100 ms
// below 2 s, 500 ms below 10 s, 1 s below a minute, 10 s below 10 minutes,
// a minute below an hour, 10 minutes below 10 hours, an hour above.
func LatencyStep(ms int64) int64 {
	switch {
	case ms < 2_000:
		return 100
	case ms < 10_000:
		return 500
	case ms < 60_000:
		return 1_000
	case ms < 600_000:
		return 10_000
	case ms < 3_600_000:
		return 60_000
	case ms < 36_000_000:
		return 600_000
	}
	return 3_600_000
}

// latencyKey rounds ms down to its LatencyStep.
func latencyKey(ms int64) int64 {
	ms = max(ms, 0)
	return ms - ms%LatencyStep(ms)
}

var countKeyRe = regexp.MustCompile(`^[a-z_]{1,32}$`)

var errTooManyNames = errors.New("too many distinct names")

// cell is one count; names are indexes into statsCache.names.
type cell struct {
	bucket int64 // start, Unix ms; for cellLatency the latency key
	count  int64
	kind   cellKind
	target uint16
	sender uint16
	name   uint16 // the state or the event kind
}

type cellKind uint8

const (
	cellMail cellKind = iota
	cellDelivery
	cellLatency
	cellEvent
)

// statsCache keeps the counts of hours that are over (see the top of this
// file). Stats holds mu for its whole run.
type statsCache struct {
	mu sync.Mutex
	// seq: the changes up to this event are applied; primed is false until
	// the first run.
	seq    int64
	primed bool
	// names interns targets, senders, states and kinds.
	names   []string
	nameIDs map[string]uint16
	entries map[statsKey]*statsEntry
	cells   int   // sum of the entries' weight
	clock   int64 // counts runs, for eviction
}

type statsKey struct {
	chunk int64 // start, Unix ms
	width int64 // min(Width, StatsChunk), ms
	// filter holds the rest of StatsFilter that changes the counts.
	filter string
}

type statsEntry struct {
	cells []cell
	used  int64
}

// weight is what an entry counts against StatsCacheCells: its cells and
// the entry itself.
func (e *statsEntry) weight() int { return len(e.cells) + statsEntryCells }

func (c *statsCache) id(name string) (uint16, error) {
	if id, ok := c.nameIDs[name]; ok {
		return id, nil
	}
	if len(c.names) >= 1<<16 {
		return 0, errTooManyNames
	}
	if c.nameIDs == nil {
		c.nameIDs = map[string]uint16{}
	}
	id := uint16(len(c.names))
	c.names = append(c.names, name)
	c.nameIDs[name] = id
	return id, nil
}

// drop removes every entry for which gone reports true.
func (c *statsCache) drop(gone func(statsKey) bool) {
	for k, e := range c.entries {
		if gone(k) {
			c.cells -= e.weight()
			delete(c.entries, k)
		}
	}
}

// put keeps an entry and evicts the least recently used ones beyond
// StatsCacheCells.
func (c *statsCache) put(k statsKey, cells []cell) {
	if c.entries == nil {
		c.entries = map[statsKey]*statsEntry{}
	}
	if old := c.entries[k]; old != nil {
		c.cells -= old.weight()
	}
	e := &statsEntry{cells: cells, used: c.clock}
	c.entries[k] = e
	c.cells += e.weight()
	if c.cells <= StatsCacheCells {
		return
	}
	keys := slices.Collect(maps.Keys(c.entries))
	sort.Slice(keys, func(i, j int) bool { return c.entries[keys[i]].used < c.entries[keys[j]].used })
	for _, old := range keys {
		if c.cells <= StatsCacheCells*3/4 {
			break
		}
		c.cells -= c.entries[old].weight()
		delete(c.entries, old)
	}
}

// Stats counts mails, deliveries, latencies and events for the statistics.
func (s *Store) Stats(ctx context.Context, f StatsFilter) (StatsRows, error) {
	hour := StatsChunk.Milliseconds()
	width := f.Width.Milliseconds()
	if width <= 0 || (hour%width != 0 && width%hour != 0) || !f.From.Before(f.To) {
		return StatsRows{}, errors.New("store: stats: invalid range or width")
	}
	for _, key := range f.Events {
		if key != "" && !countKeyRe.MatchString(key) {
			return StatsRows{}, fmt.Errorf("store: stats: invalid count key %q", key)
		}
	}
	c := &s.stats
	c.mu.Lock()
	defer c.mu.Unlock()
	c.clock++
	if err := s.applyChanges(ctx, c); err != nil {
		return StatsRows{}, fmt.Errorf("store: stats: %w", err)
	}
	retained, now := ms(f.Retained), ms(f.Now)
	c.drop(func(k statsKey) bool { return k.chunk < retained })

	r := newStatsReader(f, min(width, hour))
	key := statsKey{width: r.fine, filter: f.cacheKey()}
	merged := map[cell]int64{}
	from, to := ms(f.From), ms(f.To)
	for chunk := from - from%hour; chunk < to; chunk += hour {
		lo, hi := max(chunk, from), min(chunk+hour, to)
		whole := lo == chunk && hi == chunk+hour && hi <= now && chunk >= retained
		key.chunk = chunk
		var cells []cell
		if e := c.entries[key]; whole && e != nil {
			e.used, cells = c.clock, e.cells
		} else {
			var err error
			if cells, err = s.readChunk(ctx, c, r, chunk, lo, hi); err != nil {
				return StatsRows{}, fmt.Errorf("store: stats: %w", err)
			}
			if whole {
				c.put(key, cells)
			}
		}
		for _, x := range cells {
			n := x.count
			x.count = 0
			if x.kind != cellLatency {
				x.bucket -= x.bucket % width // a bucket wider than the hour
			}
			merged[x] += n
		}
	}
	return c.rows(merged), nil
}

// rows turns merged cells into StatsRows.
func (c *statsCache) rows(merged map[cell]int64) StatsRows {
	var out StatsRows
	for x, n := range merged {
		at := time.UnixMilli(x.bucket).UTC()
		switch x.kind {
		case cellMail:
			out.Mails = append(out.Mails, StateRow{Bucket: at, Sender: c.names[x.sender], State: Status(c.names[x.name]), Count: int(n)})
		case cellDelivery:
			out.Deliveries = append(out.Deliveries, StateRow{Bucket: at, Target: c.names[x.target], State: Status(c.names[x.name]), Count: int(n)})
		case cellLatency:
			out.Latency = append(out.Latency, LatencyRow{Target: c.names[x.target], Millis: x.bucket, Count: int(n)})
		case cellEvent:
			out.Events = append(out.Events, EventRow{Bucket: at, Target: c.names[x.target], Sender: c.names[x.sender],
				Kind: c.names[x.name], Count: int(n)})
		}
	}
	return out
}

// cacheKey is the part of the filter, besides the hour and the width, that
// changes the counts.
func (f StatsFilter) cacheKey() string {
	var b strings.Builder
	fmt.Fprintf(&b, "%q|%q|", f.Target, f.Sender)
	for _, t := range slices.Sorted(slices.Values(f.Optional)) {
		fmt.Fprintf(&b, "%q,", t)
	}
	b.WriteString("|")
	for _, k := range slices.Sorted(maps.Keys(f.Events)) {
		fmt.Fprintf(&b, "%q=%q,", k, f.Events[k])
	}
	return b.String()
}

// applyChanges drops the cached hours that an event since the last run
// touched: any event about a mail, by the mail's received time.
func (s *Store) applyChanges(ctx context.Context, c *statsCache) error {
	hour := StatsChunk.Milliseconds()
	return s.tx(ctx, func(tx *sql.Tx) error {
		var last int64
		if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(seq), 0) FROM events`).Scan(&last); err != nil {
			return err
		}
		switch {
		case !c.primed || last < c.seq || last-c.seq > 200_000:
			// Unknown, or reading that many changes costs more than counting
			// again.
			c.drop(func(statsKey) bool { return true })
		case last > c.seq && len(c.entries) > 0:
			rows, err := tx.QueryContext(ctx, `SELECT DISTINCT m.received_at / ? FROM events e
				JOIN mails m ON m.id = e.mail_id WHERE e.seq > ? AND e.seq <= ?`, hour, c.seq, last)
			if err != nil {
				return err
			}
			defer rows.Close()
			touched := map[int64]bool{}
			for rows.Next() {
				var h int64
				if err := rows.Scan(&h); err != nil {
					return err
				}
				touched[h*hour] = true
			}
			if err := rows.Err(); err != nil {
				return err
			}
			c.drop(func(k statsKey) bool { return touched[k.chunk] })
		}
		c.seq, c.primed = last, true
		return nil
	})
}

// statsReader holds the statements of one Stats call.
type statsReader struct {
	fine       int64 // the bucket width within an hour, ms
	optional   map[string]bool
	target     bool // a target filter: a mail's state is its state there
	recipients string
	rcptArgs   func(lo, hi int64) []any
	events     string
	eventArgs  func(lo, hi int64) []any
}

func newStatsReader(f StatsFilter, fine int64) *statsReader {
	r := &statsReader{fine: fine, optional: map[string]bool{}, target: f.Target != ""}
	for _, t := range f.Optional {
		r.optional[t] = true
	}
	// Every recipient of every delivery of the hour's mails, the rows of a
	// mail together (the order of the index mails_stats).
	join, where := "", ""
	if f.Target != "" {
		join = ` AND dr.target = ?`
	}
	if f.Sender != "" {
		where = ` AND m.sender = ?`
	}
	r.recipients = `SELECT m.received_at, m.sender, m.id, dr.target, dr.status, dr.resolution IS NOT NULL, dr.updated_at
		FROM mails m JOIN delivery_recipients dr ON dr.mail_id = m.id` + join + `
		WHERE m.received_at >= ? AND m.received_at < ?` + where + `
		ORDER BY m.received_at, m.sender, m.id`
	r.rcptArgs = func(lo, hi int64) []any {
		var a []any
		if f.Target != "" {
			a = append(a, f.Target)
		}
		a = append(a, lo, hi)
		if f.Sender != "" {
			a = append(a, f.Sender)
		}
		return a
	}
	if len(f.Events) == 0 {
		return r
	}
	var kinds, caseKinds []any
	var sum strings.Builder
	sum.WriteString("CASE kind")
	for _, kind := range slices.Sorted(maps.Keys(f.Events)) {
		kinds = append(kinds, kind)
		key := f.Events[kind]
		if key == "" {
			continue
		}
		// The kind is an argument; the key, checked in Stats, is part of the
		// statement.
		fmt.Fprintf(&sum, ` WHEN ? THEN CASE WHEN instr(text, '%[1]s=') > 0
			THEN max(CAST(substr(text, instr(text, '%[1]s=') + %[2]d) AS INTEGER), 0) ELSE 1 END`, key, len(key)+1)
		caseKinds = append(caseKinds, kind)
	}
	sum.WriteString(" ELSE 1 END")
	cond := `kind IN (` + placeholders(len(kinds)) + `) AND time >= ? AND time < ?`
	if f.Target != "" {
		// Events without a target, such as ingress refusals, stay in.
		cond += ` AND (target = ? OR target IS NULL)`
	}
	if f.Sender != "" {
		cond += ` AND sender = ?`
	}
	r.events = `SELECT time / ? * ?, COALESCE(target, ''), COALESCE(sender, ''), kind, SUM(` + sum.String() + `)
		FROM events WHERE ` + cond + ` GROUP BY 1, 2, 3, 4`
	r.eventArgs = func(lo, hi int64) []any {
		a := append([]any{fine, fine}, caseKinds...)
		a = append(a, kinds...)
		a = append(a, lo, hi)
		if f.Target != "" {
			a = append(a, f.Target)
		}
		if f.Sender != "" {
			a = append(a, f.Sender)
		}
		return a
	}
	return r
}

// mailAcc collects the recipients of one mail while its rows stream by.
type mailAcc struct {
	received int64
	bucket   int64
	sender   uint16
	targets  []targetAcc
}

type targetAcc struct {
	target   uint16
	name     string
	required bool
	n        statusCounts
	total    int
	// last is the latest hand-off by the relay (Unix ms), 0 if none.
	last int64
}

// flush counts one mail: its state (MailStatus: over the required targets
// unless it has none), each delivery's state and latency.
func (r *statsReader) flush(c *statsCache, a *mailAcc, counts map[cell]int64) error {
	var all, req statusCounts
	var nAll, nReq int
	for _, t := range a.targets {
		st := t.n.summary(t.total)
		id, err := c.id(string(st))
		if err != nil {
			return err
		}
		counts[cell{bucket: a.bucket, kind: cellDelivery, target: t.target, name: id}]++
		if t.last > 0 {
			counts[cell{bucket: latencyKey(t.last - a.received), kind: cellLatency, target: t.target}]++
		}
		for i, n := range t.n {
			all[i] += n
			if t.required {
				req[i] += n
			}
		}
		nAll += t.total
		if t.required {
			nReq += t.total
		}
	}
	st := all.summary(nAll)
	if nReq > 0 {
		st = req.summary(nReq)
	}
	id, err := c.id(string(st))
	if err != nil {
		return err
	}
	counts[cell{bucket: a.bucket, kind: cellMail, sender: a.sender, name: id}]++
	return nil
}

// readChunk counts [lo, hi) of the hour that starts at chunk, in one
// transaction.
func (s *Store) readChunk(ctx context.Context, c *statsCache, r *statsReader, chunk, lo, hi int64) ([]cell, error) {
	counts := map[cell]int64{}
	bucketOf := func(t int64) int64 {
		if r.fine < StatsChunk.Milliseconds() {
			return t - t%r.fine
		}
		return chunk
	}
	err := s.tx(ctx, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, r.recipients, r.rcptArgs(lo, hi)...)
		if err != nil {
			return err
		}
		defer rows.Close()
		var acc mailAcc
		var prev []byte
		started := false
		var received, updated int64
		var sender, id, target, status sql.RawBytes
		var closed bool
		for rows.Next() {
			if err := rows.Scan(&received, &sender, &id, &target, &status, &closed, &updated); err != nil {
				return err
			}
			if !started || !bytes.Equal(id, prev) {
				if started {
					if err := r.flush(c, &acc, counts); err != nil {
						return err
					}
				}
				started, prev = true, append(prev[:0], id...)
				sid, err := c.id(string(sender))
				if err != nil {
					return err
				}
				acc = mailAcc{received: received, bucket: bucketOf(received), sender: sid, targets: acc.targets[:0]}
			}
			i := slices.IndexFunc(acc.targets, func(t targetAcc) bool { return t.name == string(target) })
			if i < 0 {
				tid, err := c.id(string(target))
				if err != nil {
					return err
				}
				name := c.names[tid]
				acc.targets = append(acc.targets, targetAcc{target: tid, name: name, required: r.target || !r.optional[name]})
				i = len(acc.targets) - 1
			}
			t := &acc.targets[i]
			st, ok := statusIndex[Status(status)]
			if !ok {
				return fmt.Errorf("unknown status %q", status)
			}
			t.n[st]++
			t.total++
			if st == statusIndex[HandedOff] && !closed {
				t.last = max(t.last, updated)
			}
		}
		if err := rows.Err(); err != nil {
			return err
		}
		if started {
			if err := r.flush(c, &acc, counts); err != nil {
				return err
			}
		}
		rows.Close()
		if r.events == "" {
			return nil
		}
		ev, err := tx.QueryContext(ctx, r.events, r.eventArgs(lo, hi)...)
		if err != nil {
			return err
		}
		defer ev.Close()
		for ev.Next() {
			var b, n int64
			var target, sender, kind string
			if err := ev.Scan(&b, &target, &sender, &kind, &n); err != nil {
				return err
			}
			if r.fine >= StatsChunk.Milliseconds() {
				b = chunk
			}
			x := cell{bucket: b, kind: cellEvent}
			for _, p := range []struct {
				dst  *uint16
				name string
			}{{&x.target, target}, {&x.sender, sender}, {&x.name, kind}} {
				if *p.dst, err = c.id(p.name); err != nil {
					return err
				}
			}
			counts[x] += n
		}
		return ev.Err()
	})
	if err != nil {
		return nil, err
	}
	out := make([]cell, 0, len(counts))
	for x, n := range counts {
		x.count = n
		out = append(out, x)
	}
	return out, nil
}

// TargetTransition is a target going down or coming back (D45).
type TargetTransition struct {
	Target string
	Down   bool
	Time   time.Time
}

// TargetTransitions returns each target's last transition before from, and
// the transitions from ≤ time < to in order, from the target.down and
// target.recovered events.
func (s *Store) TargetTransitions(ctx context.Context, from, to time.Time) (before, during []TargetTransition, err error) {
	scan := func(tx *sql.Tx, out *[]TargetTransition, q string, args ...any) error {
		rows, err := tx.QueryContext(ctx, q, args...)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var t TargetTransition
			var kind string
			var at int64
			if err := rows.Scan(&t.Target, &kind, &at); err != nil {
				return err
			}
			t.Down, t.Time = kind == KindTargetDown, time.UnixMilli(at).UTC()
			*out = append(*out, t)
		}
		return rows.Err()
	}
	err = s.tx(ctx, func(tx *sql.Tx) error {
		// SQLite takes the other columns from the row with the largest time.
		if err := scan(tx, &before, `SELECT target, kind, MAX(time) FROM events
			WHERE kind IN (?, ?) AND time < ? AND target IS NOT NULL GROUP BY target ORDER BY target`,
			KindTargetDown, KindTargetRecovered, ms(from)); err != nil {
			return err
		}
		return scan(tx, &during, `SELECT target, kind, time FROM events
			WHERE kind IN (?, ?) AND time >= ? AND time < ? AND target IS NOT NULL ORDER BY time, seq`,
			KindTargetDown, KindTargetRecovered, ms(from), ms(to))
	})
	if err != nil {
		return nil, nil, fmt.Errorf("store: target transitions: %w", err)
	}
	return before, during, nil
}
