package ui

import (
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/eneuberg/postwerk/internal/query"
)

// The statistics page (D73–D75): throughput and outcomes over a range.

func (u *UI) statsRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /stats", u.authed(u.stats))
}

type rangeOption struct {
	Value, Label string
}

var rangeOptions = []rangeOption{
	{"1h", "Last hour"}, {"6h", "Last 6 hours"}, {"24h", "Last 24 hours"}, {"7d", "Last 7 days"},
	{"30d", "Last 30 days"}, {query.CustomRange, "From and to below"},
}

type statsView struct {
	S       query.Stats
	Ranges  []rangeOption
	Targets []string
	Senders []string
	// The form as given, or as counted after a preset.
	Range, From, To, Target, Sender string
	Filtered                        bool
	Wide, Narrow                    chart
	Error                           string
	// ListFrom and ListTo are the days of the range, for links to the
	// mail list (which filters by day).
	ListFrom, ListTo string
}

// fieldLayout shows a time in the form's fields, read back as UTC.
const fieldLayout = "2006-01-02 15:04"

// stats shows the statistics. The filters are GET parameters: a range, a
// target and a sending application's login, never an address.
func (u *UI) stats(w http.ResponseWriter, r *http.Request, sess session) {
	q := r.URL.Query()
	v := statsView{Ranges: rangeOptions, Range: q.Get("range"), From: q.Get("from"), To: q.Get("to"),
		Target: q.Get("target"), Sender: q.Get("sender"), Targets: u.opts.Query.Targets(), Senders: u.opts.Query.SenderNames()}
	v.Filtered = v.Target != "" || v.Sender != ""
	show := func(status int) {
		u.render(w, r, status, "stats", pageView(sess, "stats", "Statistics", v))
	}
	rng, err := query.ParseStatsRange(v.Range, v.From, v.To, false)
	if err == nil {
		v.S, err = u.opts.Query.Stats(r.Context(), query.StatsRequest{Range: rng, Target: v.Target, Sender: v.Sender})
	}
	if errors.Is(err, query.ErrInvalid) {
		v.Error = "This range or filter cannot be shown: " + strings.TrimPrefix(err.Error(), query.ErrInvalid.Error()+": ") + "."
		if v.Range == "" {
			v.Range = query.CustomRange
		}
		show(http.StatusBadRequest)
		return
	}
	if err != nil {
		u.queryFailed(w, r, "stats", err)
		return
	}
	if v.Range == "" || v.Range == query.CustomRange {
		v.Range = cmpOr(v.S.Preset, query.CustomRange)
	}
	// After a preset, the fields show what was counted, ready to change.
	v.From, v.To = v.S.From.Format(fieldLayout), v.S.To.Format(fieldLayout)
	v.ListFrom, v.ListTo = dateValue(v.S.From), dateValue(v.S.To.Add(-time.Millisecond))
	for _, t := range v.S.Targets {
		if !slices.Contains(v.Targets, t.Target) {
			v.Targets = append(v.Targets, t.Target)
		}
	}
	for _, x := range v.S.Senders {
		if x.Sender != "" && !slices.Contains(v.Senders, x.Sender) {
			v.Senders = append(v.Senders, x.Sender)
		}
	}
	if v.Sender != "" && !slices.Contains(v.Senders, v.Sender) {
		v.Senders = append(v.Senders, v.Sender)
	}
	slices.Sort(v.Senders)
	v.Wide, v.Narrow = newChart("chart-wide", v.S, chartWide), newChart("chart-narrow", v.S, chartNarrow)
	show(http.StatusOK)
}

func cmpOr(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// statsURL is the page with the same range and one filter changed.
func (v statsView) URL(key, value string) string {
	p := url.Values{}
	if v.S.Preset != "" {
		p.Set("range", v.S.Preset)
	} else {
		p.Set("range", query.CustomRange)
		p.Set("from", v.From)
		p.Set("to", v.To)
	}
	for k, val := range map[string]string{"target": v.Target, "sender": v.Sender} {
		if val != "" {
			p.Set(k, val)
		}
	}
	if value == "" {
		p.Del(key)
	} else {
		p.Set(key, value)
	}
	return "/stats?" + p.Encode()
}

// formatRate shows a share in percent, rounded down to a tenth, so that
// 99.96 % never reads as 100 %.
func formatRate(c query.Counts) string {
	r, ok := c.SuccessRate()
	switch {
	case !ok:
		return "—"
	case r >= 1:
		return "100 %"
	}
	return fmt.Sprintf("%.1f %%", math.Floor(r*1000)/10)
}

// formatLatency shows a latency at the resolution it is kept in.
func formatLatency(l query.Latency) string {
	if l.Count == 0 {
		return "—"
	}
	return formatDuration(l.P95)
}

func formatDuration(d time.Duration) string {
	switch {
	case d < time.Second:
		return fmt.Sprintf("%d ms", d.Milliseconds())
	case d < 10*time.Second:
		return trimZero(fmt.Sprintf("%.1f", d.Seconds())) + " s"
	case d < time.Minute:
		return fmt.Sprintf("%d s", int(d.Seconds()))
	}
	return formatAge(d)
}

// formatPeriod shows a configured period: "90 days", "1 day", "36 hours".
func formatPeriod(d time.Duration) string {
	if d >= 24*time.Hour && d%(24*time.Hour) == 0 {
		return countWord(int(d/(24*time.Hour)), "day", "days")
	}
	if d >= time.Hour && d%time.Hour == 0 {
		return countWord(int(d/time.Hour), "hour", "hours")
	}
	return formatAge(d)
}
