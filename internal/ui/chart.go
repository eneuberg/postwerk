package ui

import (
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/eneuberg/postwerk/internal/query"
)

// The throughput chart of the statistics page: stacked bars of mails per
// bucket by their state, drawn as inline SVG on the server (the UI has no
// JavaScript). Every mark is filled by class from the design tokens, never
// by a style attribute (the CSP allows none). Both axes take their ticks
// from one scale each, and the labels are spaced by their estimated width,
// so they never overlap whatever the counts or the range.

// chartLayer is one state in the stack, bottom first.
type chartLayer struct {
	Class, Label string
	count        func(query.Counts) int
}

var chartLayers = []chartLayer{
	{"handed_off", "handed off", func(c query.Counts) int { return c.HandedOff }},
	{"partial", "partial", func(c query.Counts) int { return c.Partial }},
	{"failed", "failed", func(c query.Counts) int { return c.Failed }},
	{"unknown", "unknown", func(c query.Counts) int { return c.Unknown }},
	{"queued", "in flight", func(c query.Counts) int { return c.InFlight }},
	{"skipped", "skipped", func(c query.Counts) int { return c.Skipped }},
}

// chartSize is one of the two drawings: wide for screens, narrow for
// phones (the page shows one by a media query).
type chartSize struct {
	W, H                     float64
	Font                     float64 // tick labels, in viewBox units
	Left, Right, Top, Bottom float64 // margins
}

var (
	chartWide   = chartSize{W: 1000, H: 300, Font: 12, Left: 0, Right: 8, Top: 14, Bottom: 26}
	chartNarrow = chartSize{W: 360, H: 240, Font: 12, Left: 0, Right: 4, Top: 14, Bottom: 26}
)

// minSegment is the least height of a count above zero, so one failure
// among thousands still shows.
const minSegment = 2

type chartSeg struct {
	Class      string
	Y, H       float64
	X, W       float64
	Title      string
	ShowsCount bool
}

type chartBar struct {
	X, W  float64
	Segs  []chartSeg
	Title string
}

type chartTick struct {
	X, Y   float64
	Label  string
	Anchor string // text-anchor
}

type chartBand struct {
	X, W  float64
	Title string
}

// chart is the data of one SVG drawing.
type chart struct {
	ID          string // prefix for the title and desc ids
	W, H        float64
	Font        float64
	PlotX       float64
	PlotY       float64
	PlotW       float64
	PlotH       float64
	Bottom      float64 // y of the base line
	Bars        []chartBar
	YTicks      []chartTick
	XTicks      []chartTick
	Bands       []chartBand
	Title, Desc string
}

// newChart lays out the stats for one size.
func newChart(id string, s query.Stats, size chartSize) chart {
	c := chart{ID: id, W: size.W, H: size.H, Font: size.Font}
	maxN := 0
	for _, b := range s.Buckets {
		maxN = max(maxN, b.Total)
	}
	step, top := niceScale(maxN)
	// The y labels sit left inside the plot's margin, as wide as the widest.
	labelW := 0.0
	for v := 0; v <= top; v += step {
		labelW = max(labelW, textWidth(formatCompact(v), size.Font))
	}
	c.PlotX = size.Left + labelW + 6
	c.PlotY = size.Top
	c.PlotW = size.W - c.PlotX - size.Right
	c.PlotH = size.H - size.Top - size.Bottom
	c.Bottom = c.PlotY + c.PlotH
	scale := c.PlotH / float64(top)
	for v := 0; v <= top; v += step {
		c.YTicks = append(c.YTicks, chartTick{X: c.PlotX - 6, Y: c.Bottom - float64(v)*scale, Label: formatCompact(v), Anchor: "end"})
	}

	n := len(s.Buckets)
	slot := c.PlotW / float64(max(n, 1))
	gap := min(slot*0.2, 3)
	if slot < 3 {
		gap = 0
	}
	for i, b := range s.Buckets {
		bar := chartBar{X: c.PlotX + float64(i)*slot + gap/2, W: slot - gap, Title: bucketTitle(b, s.Width)}
		total := float64(b.Total) * scale
		heights := make([]float64, len(chartLayers))
		sum, largest := 0.0, -1
		for j, l := range chartLayers {
			v := l.count(b.Counts)
			if v == 0 {
				continue
			}
			heights[j] = max(float64(v)*scale, minSegment)
			sum += heights[j]
			if largest < 0 || heights[j] > heights[largest] {
				largest = j
			}
		}
		// The least heights above may add up to more than the bar: take it
		// from the largest part.
		if largest >= 0 && sum > total && heights[largest]-(sum-total) >= minSegment {
			heights[largest] -= sum - total
		}
		y := c.Bottom
		for j, l := range chartLayers {
			if heights[j] == 0 {
				continue
			}
			y -= heights[j]
			bar.Segs = append(bar.Segs, chartSeg{Class: l.Class, X: bar.X, W: bar.W, Y: y, H: heights[j]})
		}
		c.Bars = append(c.Bars, bar)
	}
	c.XTicks = timeTicks(s, c.PlotX, slot, size)
	for _, t := range s.Targets {
		for _, o := range t.Outages {
			x0 := c.PlotX + float64(o.From.Sub(s.From))/float64(s.Width)*slot
			x1 := c.PlotX + float64(o.To.Sub(s.From))/float64(s.Width)*slot
			c.Bands = append(c.Bands, chartBand{X: x0, W: max(x1-x0, 1),
				Title: fmt.Sprintf("%s down %s – %s UTC", t.Target, o.From.UTC().Format("Jan 2 15:04"), o.To.UTC().Format("Jan 2 15:04"))})
		}
	}
	c.Title = "Mails received per " + formatWidth(s.Width) + ", by state"
	c.Desc = chartDesc(s)
	return c
}

// niceScale returns a step of 1, 2 or 5 times a power of ten, at least 1,
// and the top of the axis, for at most five steps above zero.
func niceScale(maxN int) (step, top int) {
	if maxN <= 0 {
		return 1, 1
	}
	raw := float64(maxN) / 5
	pow := math.Pow(10, math.Floor(math.Log10(max(raw, 1))))
	step = int(pow)
	for _, m := range []int{1, 2, 5, 10} {
		if float64(m)*pow >= raw {
			step = m * int(pow)
			break
		}
	}
	step = max(step, 1)
	top = (maxN + step - 1) / step * step
	return step, top
}

// textWidth estimates the width of a label: the label face is narrow, so
// 0.6 of the font size per character is generous.
func textWidth(s string, font float64) float64 {
	return float64(len([]rune(s))) * font * 0.6
}

// xTickSteps are the spacings the time axis may use.
var xTickSteps = []time.Duration{
	time.Minute, 2 * time.Minute, 5 * time.Minute, 10 * time.Minute, 15 * time.Minute, 30 * time.Minute,
	time.Hour, 2 * time.Hour, 3 * time.Hour, 6 * time.Hour, 12 * time.Hour,
	24 * time.Hour, 2 * 24 * time.Hour, 7 * 24 * time.Hour, 14 * 24 * time.Hour, 28 * 24 * time.Hour,
	56 * 24 * time.Hour, 91 * 24 * time.Hour, 182 * 24 * time.Hour,
}

// timeTicks picks the narrowest tick step, a multiple of the bucket width,
// whose labels fit side by side with room to spare.
func timeTicks(s query.Stats, plotX, slot float64, size chartSize) []chartTick {
	pxPerNs := slot / float64(s.Width)
	for _, step := range xTickSteps {
		if step < s.Width || step%s.Width != 0 {
			continue
		}
		var ticks []chartTick
		prevEnd := math.Inf(-1)
		fits := true
		first := s.From.Add(time.Duration((step - time.Duration(s.From.UnixNano())%step) % step))
		for t := first; !t.After(s.To); t = t.Add(step) {
			label := tickLabel(t, step)
			x := plotX + float64(t.Sub(s.From))*pxPerNs
			w := textWidth(label, size.Font)
			left := x - w/2
			anchor := "middle"
			if left < 0 {
				left, anchor = x, "start"
			}
			if left+w > size.W {
				left, anchor = x-w, "end"
			}
			if left < prevEnd+size.Font {
				fits = false
				break
			}
			prevEnd = left + w
			ticks = append(ticks, chartTick{X: x, Y: size.H - 8, Label: label, Anchor: anchor})
		}
		if fits && len(ticks) > 0 {
			return ticks
		}
	}
	return nil
}

// tickLabel names a tick: the time of day, the date at midnight or for
// steps of days.
func tickLabel(t time.Time, step time.Duration) string {
	t = t.UTC()
	if step >= 24*time.Hour || (t.Hour() == 0 && t.Minute() == 0) {
		return t.Format("Jan 2")
	}
	return t.Format("15:04")
}

// formatCompact shows a count in at most five characters: 950, 9.5k, 95k,
// 1.2M.
func formatCompact(n int) string {
	switch {
	case n < 1000:
		return strconv.Itoa(n)
	case n < 10_000:
		return trimZero(fmt.Sprintf("%.1f", float64(n)/1000)) + "k"
	case n < 1_000_000:
		return strconv.Itoa(n/1000) + "k"
	case n < 10_000_000:
		return trimZero(fmt.Sprintf("%.1f", float64(n)/1e6)) + "M"
	}
	return strconv.Itoa(n/1_000_000) + "M"
}

func trimZero(s string) string { return strings.TrimSuffix(s, ".0") }

func formatWidth(d time.Duration) string {
	switch {
	case d >= 7*24*time.Hour && d%(7*24*time.Hour) == 0:
		return unitWord(int(d/(7*24*time.Hour)), "week", "weeks")
	case d >= 24*time.Hour && d%(24*time.Hour) == 0:
		return unitWord(int(d/(24*time.Hour)), "day", "days")
	case d >= time.Hour:
		return unitWord(int(d/time.Hour), "hour", "hours")
	}
	return unitWord(int(d/time.Minute), "minute", "minutes")
}

// unitWord writes "minute" for one and "30 minutes" for more: "per
// minute", "per 30 minutes".
func unitWord(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return strconv.Itoa(n) + " " + many
}

// countWord writes "1 mail" or "1,234 mails".
func countWord(n int, one, many string) string {
	if n == 1 {
		return "1 " + one
	}
	return formatCount(n) + " " + many
}

func bucketTitle(b query.Bucket, width time.Duration) string {
	var parts []string
	for _, l := range chartLayers {
		if v := l.count(b.Counts); v > 0 {
			parts = append(parts, formatCount(v)+" "+l.Label)
		}
	}
	what := "no mail"
	if b.Total > 0 {
		what = countWord(b.Total, "mail", "mails") + ": " + strings.Join(parts, ", ")
	}
	layout := "Jan 2 15:04"
	if width >= 24*time.Hour {
		layout = "Jan 2"
	}
	return fmt.Sprintf("%s UTC, %s", b.Start.UTC().Format(layout), what)
}

func chartDesc(s query.Stats) string {
	peak, at := 0, time.Time{}
	for _, b := range s.Buckets {
		if b.Total > peak {
			peak, at = b.Total, b.Start
		}
	}
	d := fmt.Sprintf("%d bars from %s to %s UTC. %s mails in all", len(s.Buckets),
		s.From.UTC().Format("Jan 2 15:04"), s.To.UTC().Format("Jan 2 15:04"), formatCount(s.Totals.Total))
	if peak > 0 {
		d += fmt.Sprintf(", at most %s in the bar of %s", formatCount(peak), at.UTC().Format("Jan 2 15:04"))
	}
	return d + ". The table below the chart has every bar."
}

// formatCount writes a count with thousands separators: 12,345.
func formatCount(n int) string {
	s := strconv.Itoa(n)
	if n < 0 {
		return s
	}
	for i := len(s) - 3; i > 0; i -= 3 {
		s = s[:i] + "," + s[i:]
	}
	return s
}
