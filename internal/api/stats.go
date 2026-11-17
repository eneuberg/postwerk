package api

import (
	"errors"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/eneuberg/postwerk/internal/query"
)

// The statistics (D73–D75): counts over a range of time, for admin tokens.

type countsJSON struct {
	Total      int `json:"total"`
	HandedOff  int `json:"handed_off"`
	Partial    int `json:"partial"`
	Failed     int `json:"failed"`
	Unknown    int `json:"unknown"`
	InFlight   int `json:"in_flight"`
	Skipped    int `json:"skipped"`
	Rejected   int `json:"rejected"`
	NotAllowed int `json:"not_allowed"`
	Retries    int `json:"retries"`
	// SuccessRate is null when nothing settled.
	SuccessRate *float64 `json:"success_rate"`
}

func counts(c query.Counts) countsJSON {
	j := countsJSON{Total: c.Total, HandedOff: c.HandedOff, Partial: c.Partial, Failed: c.Failed, Unknown: c.Unknown,
		InFlight: c.InFlight, Skipped: c.Skipped, Rejected: c.Rejected, NotAllowed: c.NotAllowed, Retries: c.Retries}
	if r, ok := c.SuccessRate(); ok {
		j.SuccessRate = &r
	}
	return j
}

type ingressJSON struct {
	AuthFailed  int `json:"auth_failed"`
	Rejected    int `json:"rejected"`
	RateLimited int `json:"rate_limited"`
}

func ingressOf(i query.Ingress) ingressJSON {
	return ingressJSON{AuthFailed: i.AuthFailed, Rejected: i.Rejected, RateLimited: i.RateLimited}
}

type latencyJSON struct {
	Deliveries int   `json:"deliveries"`
	P50MS      int64 `json:"p50_ms"`
	P95MS      int64 `json:"p95_ms"`
}

func latencyOf(l query.Latency) latencyJSON {
	return latencyJSON{Deliveries: l.Count, P50MS: l.P50.Milliseconds(), P95MS: l.P95.Milliseconds()}
}

type bucketJSON struct {
	Start time.Time `json:"start"`
	countsJSON
	Ingress ingressJSON `json:"ingress"`
}

type outageJSON struct {
	From time.Time `json:"from"`
	To   time.Time `json:"to"`
	Open bool      `json:"open"`
}

type targetStatsJSON struct {
	Target     string `json:"target"`
	Configured bool   `json:"configured"`
	Optional   bool   `json:"optional"`
	countsJSON
	Latency     latencyJSON  `json:"latency"`
	DownSeconds int64        `json:"down_seconds"`
	DownNow     bool         `json:"down_now"`
	Outages     []outageJSON `json:"outages"`
}

type senderStatsJSON struct {
	Sender string `json:"sender"`
	countsJSON
	Ingress ingressJSON `json:"ingress"`
}

type statsResponse struct {
	Now           time.Time         `json:"now"`
	Range         string            `json:"range"`
	From          time.Time         `json:"from"`
	To            time.Time         `json:"to"`
	Timezone      string            `json:"timezone"`
	BucketSeconds int64             `json:"bucket_seconds"`
	RetainedFrom  *time.Time        `json:"retained_from,omitempty"`
	Clamped       bool              `json:"clamped"`
	Target        string            `json:"target,omitempty"`
	Sender        string            `json:"sender,omitempty"`
	Totals        countsJSON        `json:"totals"`
	Latency       latencyJSON       `json:"latency"`
	Ingress       ingressJSON       `json:"ingress"`
	Buckets       []bucketJSON      `json:"buckets"`
	Targets       []targetStatsJSON `json:"targets"`
	Senders       []senderStatsJSON `json:"senders"`
}

var statsParams = map[string]bool{"range": true, "from": true, "to": true, "target": true, "sender": true}

// stats returns the statistics for a range (admin only). The parameters
// never hold an address: a target and a sending application's login.
func (a *API) stats(r *http.Request, _ caller) result {
	q, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		return failed(http.StatusBadRequest, "bad_request", "The URL parameters could not be read.")
	}
	for k, v := range q {
		if !statsParams[k] || len(v) != 1 {
			return failed(http.StatusBadRequest, "bad_request", "Known parameters, each once: range, from, to, target, sender.")
		}
	}
	rng, err := query.ParseStatsRange(q.Get("range"), q.Get("from"), q.Get("to"), true)
	var st query.Stats
	if err == nil {
		st, err = a.opts.Query.Stats(r.Context(), query.StatsRequest{Range: rng, Target: q.Get("target"), Sender: q.Get("sender")})
	}
	if errors.Is(err, query.ErrInvalid) {
		// The texts after the prefix are fixed (internal/query), never input.
		return failed(http.StatusBadRequest, "bad_request",
			"The range or filter is not valid ("+strings.TrimPrefix(err.Error(), query.ErrInvalid.Error()+": ")+").")
	}
	if err != nil {
		a.opts.Logger.Error("api: reading the store failed", "endpoint", "stats", "err", err)
		return failed(http.StatusInternalServerError, "internal", "The store could not be read. Details are in the log.")
	}
	resp := statsResponse{
		Now: utc(st.Now), Range: st.Preset, From: utc(st.From), To: utc(st.To), Timezone: "UTC",
		BucketSeconds: int64(st.Width / time.Second), RetainedFrom: optTime(st.Retained), Clamped: st.Clamped,
		Target: st.Target, Sender: st.Sender, Totals: counts(st.Totals), Latency: latencyOf(st.Latency),
		Ingress: ingressOf(st.Ingress), Buckets: []bucketJSON{}, Targets: []targetStatsJSON{}, Senders: []senderStatsJSON{},
	}
	if resp.Range == "" {
		resp.Range = query.CustomRange
	}
	for _, b := range st.Buckets {
		resp.Buckets = append(resp.Buckets, bucketJSON{Start: utc(b.Start), countsJSON: counts(b.Counts), Ingress: ingressOf(b.Ingress)})
	}
	for _, t := range st.Targets {
		tj := targetStatsJSON{Target: t.Target, Configured: t.Configured, Optional: t.Optional, countsJSON: counts(t.Counts),
			Latency: latencyOf(t.Latency), DownSeconds: int64(t.Down / time.Second), DownNow: t.DownNow, Outages: []outageJSON{}}
		for _, o := range t.Outages {
			tj.Outages = append(tj.Outages, outageJSON{From: utc(o.From), To: utc(o.To), Open: o.Open})
		}
		resp.Targets = append(resp.Targets, tj)
	}
	for _, x := range st.Senders {
		resp.Senders = append(resp.Senders, senderStatsJSON{Sender: x.Sender, countsJSON: counts(x.Counts), Ingress: ingressOf(x.Ingress)})
	}
	return result{status: http.StatusOK, body: resp}
}
