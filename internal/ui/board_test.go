package ui

import (
	"math"
	"testing"
	"time"

	"github.com/eneuberg/postwerk/internal/query"
	"github.com/eneuberg/postwerk/internal/store"
)

func TestBoard(t *testing.T) {
	o := query.Overview{Targets: []query.TargetOverview{
		{Target: "mailjet", Configured: true, Queued: 2, OldestQueuedAge: 20 * time.Minute, RecentTotal: 10,
			Recent: []query.StatusCount{{Status: store.HandedOff, Count: 8}, {Status: store.Failed, Count: 1}, {Status: store.Unknown, Count: 1}}},
		{Target: "capture", Configured: true, Optional: true, Health: query.TargetHealth{Down: true}, RecentTotal: 3,
			Recent: []query.StatusCount{{Status: store.Failed, Count: 3}}},
	}}
	b := newBoard(o)
	if b.Targets != 2 || b.Down != 1 || b.DownNames[0] != "capture" || b.Queued != 2 || b.Oldest != 20*time.Minute ||
		b.Recipients != 13 || b.Failed != 4 || b.Unknown != 1 {
		t.Fatalf("board: %+v", b)
	}
	if b.TargetsLamp != store.Failed || b.QueueLamp != store.Unknown || b.RecentLamp != store.Failed {
		t.Fatalf("lamps: %s %s %s", b.TargetsLamp, b.QueueLamp, b.RecentLamp)
	}
	// Failures only at an optional target are a warning, not a failure.
	o.Targets[0].Recent = []query.StatusCount{{Status: store.HandedOff, Count: 10}}
	o.Targets[0].Queued = 0
	if b := newBoard(o); b.RecentLamp != store.Unknown || b.QueueLamp != store.Skipped {
		t.Fatalf("optional failures: %s, queue %s", b.RecentLamp, b.QueueLamp)
	}
	if b := newBoard(query.Overview{}); b.Targets != 0 || b.RecentLamp != store.Skipped {
		t.Fatalf("empty: %+v", b)
	}
}

func TestMix(t *testing.T) {
	if newMix(query.TargetOverview{}) != nil {
		t.Fatal("a target without recipients has a bar")
	}
	segs := newMix(query.TargetOverview{RecentTotal: 1000, Recent: []query.StatusCount{
		{Status: store.Queued}, {Status: store.HandedOff, Count: 999}, {Status: store.Failed, Count: 1}}})
	if len(segs) != 2 || segs[0].Status != store.HandedOff || segs[1].Status != store.Failed {
		t.Fatalf("segments: %+v", segs)
	}
	if segs[1].W < 1 {
		t.Fatalf("one failed recipient in a thousand is invisible: %v", segs[1].W)
	}
	end := segs[1].X + segs[1].W
	if math.Abs(end-100) > 1e-9 || segs[0].X != 0 || segs[1].X <= segs[0].W {
		t.Fatalf("layout: %+v", segs)
	}
}
