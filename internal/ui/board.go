package ui

import (
	"time"

	"github.com/eneuberg/postwerk/internal/query"
	"github.com/eneuberg/postwerk/internal/store"
)

// board is the summary line at the top of the overview: the state of the
// targets, of the queue and of the last 24 hours, each with the lamp that
// fits it, before the detail of each target.
type board struct {
	Targets, Down, Unconfigured int
	DownNames                   []string
	Queued                      int
	Oldest                      time.Duration
	Recipients                  int
	Failed, Unknown             int
	// Lamp classes (a status name) for the three parts.
	TargetsLamp, QueueLamp, RecentLamp store.Status
}

// oldQueue is the age from which the oldest queued mail colours the queue
// part of the summary.
const oldQueue = 15 * time.Minute

func newBoard(o query.Overview) board {
	b := board{Targets: len(o.Targets), TargetsLamp: store.HandedOff, QueueLamp: store.HandedOff, RecentLamp: store.HandedOff}
	failedRequired := false
	for _, t := range o.Targets {
		if !t.Configured {
			b.Unconfigured++
		}
		if t.Health.Down {
			b.Down++
			b.DownNames = append(b.DownNames, t.Target)
		}
		b.Queued += t.Queued
		if t.Queued > 0 && t.OldestQueuedAge > b.Oldest {
			b.Oldest = t.OldestQueuedAge
		}
		b.Recipients += t.RecentTotal
		for _, c := range t.Recent {
			switch c.Status {
			case store.Failed:
				b.Failed += c.Count
				if !t.Optional && c.Count > 0 {
					failedRequired = true
				}
			case store.Unknown:
				b.Unknown += c.Count
			}
		}
	}
	switch {
	case b.Down > 0:
		b.TargetsLamp = store.Failed
	case b.Unconfigured > 0:
		b.TargetsLamp = store.Unknown
	}
	switch {
	case b.Queued == 0:
		b.QueueLamp = store.Skipped
	case b.Oldest >= oldQueue:
		b.QueueLamp = store.Unknown
	default:
		b.QueueLamp = store.Queued
	}
	switch {
	case failedRequired:
		b.RecentLamp = store.Failed
	case b.Failed > 0 || b.Unknown > 0:
		b.RecentLamp = store.Unknown
	case b.Recipients == 0:
		b.RecentLamp = store.Skipped
	}
	return b
}

// mixSegment is one part of a target's bar of recipients by status, in
// the 0–100 units of the bar's viewBox.
type mixSegment struct {
	Status store.Status
	Count  int
	X, W   float64
}

// mixGap separates two segments; mixMin keeps a single failed recipient
// among thousands visible.
const (
	mixGap = 0.4
	mixMin = 1.2
)

func newMix(t query.TargetOverview) []mixSegment {
	if t.RecentTotal == 0 {
		return nil
	}
	var segs []mixSegment
	var sum float64
	for _, c := range t.Recent {
		if c.Count == 0 {
			continue
		}
		w := max(float64(c.Count)/float64(t.RecentTotal)*100, mixMin)
		segs = append(segs, mixSegment{Status: c.Status, Count: c.Count, W: w})
		sum += w
	}
	room := 100 - mixGap*float64(len(segs)-1)
	x := 0.0
	for i := range segs {
		segs[i].W = segs[i].W / sum * room
		segs[i].X = x
		x += segs[i].W + mixGap
	}
	return segs
}

// dayOf and clockOf split a time for the event timeline, which shows the
// day once and then the time of each event.
func dayOf(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.DateOnly)
}

func clockOf(t time.Time) string {
	if t.IsZero() {
		return "—"
	}
	return t.UTC().Format(time.TimeOnly)
}
