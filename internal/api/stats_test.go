package api

import (
	"strings"
	"testing"
	"time"

	"github.com/eneuberg/postwerk/internal/store"
)

func TestStats(t *testing.T) {
	h := newHarness(t)
	now := h.clock.Now()
	h.seed("pw_01", now.Add(-50*time.Minute), []string{"a@example.test"}, []store.Status{store.HandedOff})
	h.seed("pw_02", now.Add(-20*time.Minute), []string{"b@example.test", "c@example.test"},
		[]store.Status{store.HandedOff, store.Failed})
	h.seed("pw_03", now.Add(-3*time.Hour), []string{"d@example.test"}, []store.Status{store.Failed})

	// read tokens may not see it.
	if r := h.do("GET", "/v1/stats", readBearer, ""); r.StatusCode != 403 {
		t.Fatalf("read: %d %s", r.StatusCode, r.body)
	}

	var s statsResponse
	r := h.do("GET", "/v1/stats?range=1h", adminBearer, "")
	if r.StatusCode != 200 {
		t.Fatalf("1h: %d %s", r.StatusCode, r.body)
	}
	r.json(t, &s)
	if s.Range != "1h" || s.Timezone != "UTC" || s.BucketSeconds != 60 || len(s.Buckets) != 60 ||
		!s.To.Equal(now) || !s.From.Equal(now.Add(-time.Hour)) {
		t.Errorf("range %s %s %s–%s, %d s, %d buckets", s.Range, s.Timezone, s.From, s.To, s.BucketSeconds, len(s.Buckets))
	}
	if s.Totals.Total != 2 || s.Totals.HandedOff != 1 || s.Totals.Partial != 1 || s.Totals.SuccessRate == nil || *s.Totals.SuccessRate != 0.5 {
		t.Errorf("totals %+v", s.Totals)
	}
	if len(s.Targets) != 1 || s.Targets[0].Target != "mailjet" || s.Targets[0].Latency.Deliveries != 2 ||
		s.Targets[0].Latency.P95MS != 2000 || s.Targets[0].Outages == nil {
		t.Errorf("targets %+v", s.Targets)
	}
	if len(s.Senders) != 1 || s.Senders[0].Sender != "shop" || s.Senders[0].Total != 2 {
		t.Errorf("senders %+v", s.Senders)
	}
	if !strings.Contains(r.body, `"success_rate":0.5`) || !strings.Contains(r.body, `"in_flight":0`) {
		t.Errorf("body %s", r.body)
	}

	r = h.do("GET", "/v1/stats?from=2026-11-15T08:00&sender=shop&target=mailjet", adminBearer, "")
	r.json(t, &s)
	if r.StatusCode != 200 || s.Range != "custom" || s.Totals.Total != 3 || s.Totals.Failed != 1 || s.Sender != "shop" {
		t.Errorf("custom: %d %+v", r.StatusCode, s.Totals)
	}
	// An empty range has its buckets and null rates.
	r = h.do("GET", "/v1/stats?range=custom&from=2026-11-14&to=2026-11-14", adminBearer, "")
	r.json(t, &s)
	if r.StatusCode != 200 || s.Totals.Total != 0 || s.Totals.SuccessRate != nil || len(s.Buckets) != 48 ||
		!strings.Contains(r.body, `"success_rate":null`) {
		t.Errorf("empty: %d %s", r.StatusCode, r.body)
	}

	for q, want := range map[string]string{
		"range=2h":                      "range",
		"range=24h&from=2026-11-01":     "from and to need range=custom",
		"range=custom":                  "from",
		"from=2026-11-15&to=2026-11-14": "from is not before to",
		"from=tomorrow":                 "from",
		"target=MAIL":                   "target",
		"sender=a%40b.test":             "sender",
		"range=1h&range=6h":             "Known parameters",
		"state=failed":                  "Known parameters",
		"from=2026-11-16":               "from is not before now",
	} {
		r := h.do("GET", "/v1/stats?"+q, adminBearer, "")
		if r.StatusCode != 400 || !strings.Contains(r.body, want) {
			t.Errorf("%s: %d %s", q, r.StatusCode, r.body)
		}
	}
	evs := h.apiEvents()
	if !contains(evs, "api.forbidden support stats") || !contains(evs, "api.request support.admin stats status=200 results=0") ||
		!contains(evs, "api.request support.admin stats status=400 results=0") {
		t.Errorf("events %q", evs)
	}
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}
