package watch

import (
	"context"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"time"

	"github.com/eneuberg/postwerk/internal/store"
)

// KindSenderVolumeHigh: a sender sent far more than usual in the last hour
// (D67). A soft signal for a leaked key: an error, so it reaches the default
// alerting, at most once per sender and hour.
const KindSenderVolumeHigh = "sender.volume_high"

// Volume intervals.
const (
	// VolumeInterval is how often the last hour is counted.
	VolumeInterval = time.Minute
	// volumeWindow is the hour that is compared.
	volumeWindow = time.Hour
	// volumeBaseline is the history the average comes from: the 7 days
	// before the window.
	volumeBaseline = 7 * 24 * time.Hour
	// volumeRefresh is how often the averages are read again.
	volumeRefresh = time.Hour
	// volumeRepeat is the least time between two events of one sender.
	volumeRepeat = time.Hour
)

// VolumeStore is what the volume check needs from the store.
type VolumeStore interface {
	Append(ctx context.Context, e store.Event) error
	MailsPerSender(ctx context.Context, from, to time.Time) (map[string]int, error)
	LastBySender(ctx context.Context, kind string, since time.Time) (map[string]time.Time, error)
}

// VolumeOptions configure a Volume check.
type VolumeOptions struct {
	Store VolumeStore
	// Factor: more mails in the last hour than Factor times the sender's
	// hourly average over the 7 days before; 0 turns this rule off.
	Factor int
	// Min: the factor rule needs at least Min mails in the hour, so a
	// sender that sends 1 mail an hour does not alarm at 11.
	Min int
	// Max: more mails than Max in one hour, whatever the average; 0 off.
	Max    int
	Logger *slog.Logger
	Now    func() time.Time
}

// Volume compares each sender's last hour with its own history and writes
// sender.volume_high when it is far above. It counts accepted mails in the
// store, so it needs no hook in ingress and survives a restart: the last
// event per sender is read back at start.
type Volume struct {
	opts VolumeOptions

	baseline  map[string]float64 // mails per hour
	refreshed time.Time
	written   map[string]time.Time
}

// NewVolume returns a Volume that knows the events of the last hour.
func NewVolume(ctx context.Context, opts VolumeOptions) (*Volume, error) {
	if opts.Logger == nil {
		opts.Logger = slog.New(slog.DiscardHandler)
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	v := &Volume{opts: opts, written: map[string]time.Time{}}
	if v.off() {
		return v, nil
	}
	last, err := opts.Store.LastBySender(ctx, KindSenderVolumeHigh, opts.Now().Add(-volumeRepeat))
	if err != nil {
		return nil, err
	}
	v.written = last
	return v, nil
}

func (v *Volume) off() bool { return v.opts.Factor <= 0 && v.opts.Max <= 0 }

// Run checks every VolumeInterval until ctx ends.
func (v *Volume) Run(ctx context.Context) {
	if v.off() {
		return
	}
	t := time.NewTicker(VolumeInterval)
	defer t.Stop()
	for {
		if err := v.Check(ctx); err != nil && ctx.Err() == nil {
			v.opts.Logger.Warn("sender volume check failed", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// Check counts the last hour once and writes the events that are due.
func (v *Volume) Check(ctx context.Context) error {
	if v.off() {
		return nil
	}
	now := v.opts.Now()
	from := now.Add(-volumeWindow)
	if v.baseline == nil || now.Sub(v.refreshed) >= volumeRefresh {
		counts, err := v.opts.Store.MailsPerSender(ctx, from.Add(-volumeBaseline), from)
		if err != nil {
			return err
		}
		v.baseline = map[string]float64{}
		for name, n := range counts {
			v.baseline[name] = float64(n) / volumeBaseline.Hours()
		}
		v.refreshed = now
	}
	current, err := v.opts.Store.MailsPerSender(ctx, from, now)
	if err != nil {
		return err
	}
	for _, name := range slices.Sorted(maps.Keys(current)) {
		n := current[name]
		avg := v.baseline[name]
		var why string
		switch {
		case v.opts.Max > 0 && n > v.opts.Max:
			why = fmt.Sprintf("max=%d", v.opts.Max)
		case v.opts.Factor > 0 && n >= v.opts.Min && float64(n) > float64(v.opts.Factor)*avg:
			why = fmt.Sprintf("factor=%d", v.opts.Factor)
		default:
			continue
		}
		if last, ok := v.written[name]; ok && now.Sub(last) < volumeRepeat {
			continue
		}
		e := store.Event{Time: now, Kind: KindSenderVolumeHigh, Level: store.Error, Sender: name,
			Text: fmt.Sprintf("mails_last_hour=%d hourly_average=%.1f %s", n, avg, why)}
		if err := v.opts.Store.Append(ctx, e); err != nil {
			return err // not marked, so the next check tries again
		}
		v.written[name] = now
	}
	return nil
}
