package ui

import (
	"fmt"
	"html/template"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/eneuberg/postwerk/internal/query"
	"github.com/eneuberg/postwerk/internal/store"
)

func TestStatsPage(t *testing.T) {
	h := newHarness(t)
	now := h.clock.Now()
	h.sender = "billing"
	h.seedMail("pw_s1", now.Add(-50*time.Minute), []string{"a@example.test"}, map[string][]store.Status{"mailjet": {store.HandedOff}})
	h.sender = "shop"
	h.seedMail("pw_s2", now.Add(-20*time.Hour), []string{"b@example.test"}, map[string][]store.Status{"mailjet": {store.Failed}})
	b := h.loggedIn("viewer", viewerPassword)

	r := b.get("/stats")
	if r.StatusCode != 200 {
		t.Fatalf("GET /stats: %d", r.StatusCode)
	}
	for _, want := range []string{
		`href="/stats" aria-current="page"`, "<h1>Statistics</h1>", "one bar per 30 minutes",
		`<option value="24h" selected>`, `role="img" aria-labelledby="chart-wide-title chart-wide-desc"`,
		`<title id="chart-wide-title">Mails received per 30 minutes, by state</title>`,
		`class="seg seg-handed_off"`, `class="seg seg-failed"`, "The chart as a table",
		`<p class="board-value num">2</p>`, "50.0 %", `href="/stats?range=24h&amp;target=mailjet"`,
		`href="/stats?range=24h&amp;sender=billing"`, "UTC",
	} {
		if !strings.Contains(r.body, want) {
			t.Errorf("/stats lacks %q", want)
		}
	}
	if strings.Contains(r.body, " style=") || strings.Contains(r.body, "<script") {
		t.Error("the page has a style attribute or a script; the CSP forbids both")
	}

	// A filter and a custom range from the form's fields.
	r = b.get("/stats?range=custom&from=2026-11-15+10:00&to=2026-11-15T12:00&target=mailjet&sender=billing")
	if r.StatusCode != 200 || !strings.Contains(r.body, `<option value="billing" selected>`) ||
		!strings.Contains(r.body, `value="2026-11-15 10:00"`) || !strings.Contains(r.body, "one bar per 2 minutes") {
		t.Errorf("custom range: %d", r.StatusCode)
	}
	// After a preset the fields hold what was counted.
	r = b.get("/stats?range=1h&from=2001-01-01T00:00")
	if r.StatusCode != 200 || !strings.Contains(r.body, `value="2026-11-15 11:00"`) {
		t.Errorf("1h: %d", r.StatusCode)
	}

	// Nothing in the range: an empty state, no chart.
	r = b.get("/stats?range=custom&from=2026-10-01&to=2026-10-02")
	if r.StatusCode != 200 || !strings.Contains(r.body, "No mail was received in this range") || strings.Contains(r.body, "<svg") {
		t.Errorf("empty: %d", r.StatusCode)
	}

	for _, q := range []string{"range=2h", "range=custom", "from=2026-11-16T00:00", "target=Mail%20Jet", "sender=a%40b", "from=x"} {
		r := b.get("/stats?" + q)
		if r.StatusCode != 400 || !strings.Contains(r.body, "cannot be shown") || !strings.Contains(r.body, `action="/stats"`) {
			t.Errorf("%s: %d", q, r.StatusCode)
		}
		if strings.Contains(r.body, "a@b") || strings.Contains(r.body, "Mail Jet") {
			t.Errorf("%s: the input is echoed outside the form", q)
		}
	}
}

// statsFor builds statistics with the given mails per bucket, all handed
// off but one failed in the last bucket.
func statsFor(width time.Duration, counts []int) query.Stats {
	from := time.Date(2026, 11, 15, 0, 0, 0, 0, time.UTC)
	s := query.Stats{From: from, Width: width, To: from.Add(time.Duration(len(counts)) * width)}
	for i, n := range counts {
		b := query.Bucket{Start: from.Add(time.Duration(i) * width)}
		b.Total, b.HandedOff = n, n
		s.Totals.Total += n
		s.Buckets = append(s.Buckets, b)
	}
	if last := len(s.Buckets) - 1; last >= 0 && counts[last] > 0 {
		s.Buckets[last].HandedOff--
		s.Buckets[last].Failed++
	}
	return s
}

var labelRe = regexp.MustCompile(`<text class="chart-label" x="([0-9.]+)" y="([0-9.]+)"(?: dy="[^"]*")? text-anchor="(\w+)" font-size="([0-9.]+)">([^<]*)</text>`)

// The axis labels never overlap and stay inside the drawing, for tiny and
// huge counts and every bucket count from the presets.
func TestChartLabels(t *testing.T) {
	cases := []struct {
		width  time.Duration
		counts []int
	}{
		{time.Minute, make([]int, 60)},
		{time.Minute, append(make([]int, 59), 1)},
		{5 * time.Minute, ramp(72, 3)},
		{30 * time.Minute, ramp(48, 987_654)},
		{3 * time.Hour, ramp(56, 49_999)},
		{24 * time.Hour, ramp(31, 1_500_000)},
		{24 * time.Hour, ramp(90, 123_456_789)},
		{7 * 24 * time.Hour, ramp(53, 7)},
	}
	for _, c := range cases {
		s := statsFor(c.width, c.counts)
		for _, size := range []chartSize{chartWide, chartNarrow} {
			name := fmt.Sprintf("%s×%d max %d at %g", c.width, len(c.counts), c.counts[len(c.counts)-1], size.W)
			ch := newChart("t", s, size)
			if len(ch.XTicks) < 2 {
				t.Errorf("%s: %d time labels", name, len(ch.XTicks))
			}
			if len(ch.YTicks) < 2 || len(ch.YTicks) > 6 {
				t.Errorf("%s: %d count labels", name, len(ch.YTicks))
			}
			var buf strings.Builder
			if err := testPages(t).ExecuteTemplate(&buf, "chart", ch); err != nil {
				t.Fatal(err)
			}
			checkLabels(t, name, buf.String(), size)
			// The bars stay inside the plot and the failure shows.
			for _, bar := range ch.Bars {
				for _, sg := range bar.Segs {
					if sg.Y < ch.PlotY-0.01 || sg.Y+sg.H > ch.Bottom+0.01 || sg.X < ch.PlotX || sg.X+sg.W > ch.PlotX+ch.PlotW+0.01 {
						t.Errorf("%s: a bar leaves the plot: %+v", name, sg)
					}
				}
			}
			if last := ch.Bars[len(ch.Bars)-1]; c.counts[len(c.counts)-1] > 0 &&
				!strings.Contains(fmt.Sprint(last.Segs), "failed") {
				t.Errorf("%s: the one failure is not drawn", name)
			}
		}
	}
}

func ramp(n, top int) []int {
	out := make([]int, n)
	for i := range out {
		out[i] = top * (i + 1) / n
	}
	return out
}

type box struct{ x0, x1, y0, y1 float64 }

func checkLabels(t *testing.T, name, svg string, size chartSize) {
	t.Helper()
	var boxes []box
	for _, m := range labelRe.FindAllStringSubmatch(svg, -1) {
		x, _ := strconv.ParseFloat(m[1], 64)
		y, _ := strconv.ParseFloat(m[2], 64)
		font, _ := strconv.ParseFloat(m[4], 64)
		w := textWidth(m[5], font)
		b := box{x0: x - w/2, x1: x + w/2, y0: y - font, y1: y + font*0.3}
		switch m[3] {
		case "start":
			b.x0, b.x1 = x, x+w
		case "end":
			b.x0, b.x1 = x-w, x
		}
		if b.x0 < -0.01 || b.x1 > size.W+0.01 || b.y0 < -0.01 || b.y1 > size.H+0.01 {
			t.Errorf("%s: label %q outside the drawing: %+v", name, m[5], b)
		}
		for _, o := range boxes {
			if b.x0 < o.x1 && o.x0 < b.x1 && b.y0 < o.y1 && o.y0 < b.y1 {
				t.Errorf("%s: label %q overlaps another", name, m[5])
			}
		}
		boxes = append(boxes, b)
	}
	if len(boxes) == 0 {
		t.Errorf("%s: no labels", name)
	}
}

func TestNiceScale(t *testing.T) {
	for n, want := range map[int][2]int{
		0: {1, 1}, 1: {1, 1}, 3: {1, 3}, 7: {2, 8}, 10: {2, 10}, 11: {5, 15}, 99: {20, 100},
		987_654: {200_000, 1_000_000}, 1_500_001: {500_000, 2_000_000},
	} {
		step, top := niceScale(n)
		if step != want[0] || top != want[1] || top < n || top/step > 5 {
			t.Errorf("niceScale(%d) = %d, %d; want %v", n, step, top, want)
		}
	}
}

func TestFormats(t *testing.T) {
	for n, want := range map[int]string{0: "0", 999: "999", 1000: "1k", 1260: "1.3k", 9999: "10k", 45_000: "45k",
		1_000_000: "1M", 1_260_000: "1.3M", 123_456_789: "123M"} {
		if got := formatCompact(n); got != want {
			t.Errorf("formatCompact(%d) = %q, want %q", n, got, want)
		}
	}
	for n, want := range map[int]string{7: "7", 1234: "1,234", 1234567: "1,234,567"} {
		if got := formatCount(n); got != want {
			t.Errorf("formatCount(%d) = %q", n, got)
		}
	}
	for c, want := range map[query.Counts]string{
		{}:                                   "—",
		{HandedOff: 3}:                       "100 %",
		{HandedOff: 9999, Failed: 1}:         "99.9 %",
		{HandedOff: 1, Partial: 1}:           "50.0 %",
		{HandedOff: 2, Unknown: 1}:           "66.6 %",
		{Failed: 2, Skipped: 5, InFlight: 3}: "0.0 %",
	} {
		if got := formatRate(c); got != want {
			t.Errorf("formatRate(%+v) = %q, want %q", c, got, want)
		}
	}
	for d, want := range map[time.Duration]string{300 * time.Millisecond: "300 ms", 2 * time.Second: "2 s",
		2500 * time.Millisecond: "2.5 s", 45 * time.Second: "45 s", 5 * time.Minute: "5 min", 90 * time.Minute: "1 h 30 min"} {
		if got := formatDuration(d); got != want {
			t.Errorf("formatDuration(%s) = %q, want %q", d, got, want)
		}
	}
}

func testPages(t *testing.T) *template.Template {
	t.Helper()
	pages, err := parseTemplates()
	if err != nil {
		t.Fatal(err)
	}
	return pages["stats"]
}
