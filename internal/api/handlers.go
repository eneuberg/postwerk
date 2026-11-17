package api

import (
	"errors"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"time"

	"github.com/eneuberg/postwerk/internal/auth"
	"github.com/eneuberg/postwerk/internal/query"
	"github.com/eneuberg/postwerk/internal/store"
)

func (a *API) routes(mux *http.ServeMux) {
	// Lookups, for read and admin tokens (H5).
	mux.Handle("/v1/search", a.handle(endpoint{name: "search", method: http.MethodPost, fn: a.search}))
	mux.Handle("/v1/mails/{id}", a.handle(endpoint{name: "mail", method: http.MethodGet, fn: a.mail}))
	// Lists, for admin tokens only (H6).
	mux.Handle("/v1/mails", a.handle(endpoint{name: "mails", method: http.MethodGet, admin: true, fn: a.mails}))
	mux.Handle("/v1/overview", a.handle(endpoint{name: "overview", method: http.MethodGet, admin: true, fn: a.overview}))
	// Changes, for admin tokens only (D53–D55).
	mux.Handle("/v1/mails/{id}/targets/{target}/close",
		a.handle(endpoint{name: "close", method: http.MethodPost, admin: true, fn: a.closeDelivery}))
	mux.Handle("/v1/mails/{id}/targets/{target}/resend",
		a.handle(endpoint{name: "resend", method: http.MethodPost, admin: true, fn: a.resendDelivery}))
}

func access(c caller) query.Access {
	if c.role == auth.RoleAdmin {
		return query.Full
	}
	return query.Masked
}

type searchRequest struct {
	By    query.SearchBy `json:"by"`
	Query string         `json:"query"`
}

type searchResponse struct {
	Mails []mailSummary `json:"mails"`
	// Limit is the most mails one search returns to this token; Truncated
	// says that more matched.
	Limit     int  `json:"limit"`
	Truncated bool `json:"truncated"`
}

// search finds mails by id, ref, Message-ID or address (H5). The query is
// never logged, written to an event or echoed back.
func (a *API) search(r *http.Request, c caller) result {
	var req searchRequest
	if e := decode(r, &req); e != nil {
		return result{status: statusOf(e), body: e}
	}
	if req.By == "" {
		req.By = query.ByAny
	}
	if !query.ValidSearchBy(req.By) {
		return failed(http.StatusBadRequest, "bad_request", "by must be any, id, ref, message_id or address.")
	}
	limit := query.MaxResults
	if c.role != auth.RoleAdmin {
		limit = ReadMaxResults
	}
	rows, err := a.opts.Query.Search(r.Context(), req.By, req.Query)
	var res result
	switch {
	case errors.Is(err, query.ErrEmptyQuery):
		res = failed(http.StatusBadRequest, "bad_request", "query is empty.")
	case errors.Is(err, query.ErrQueryTooLong):
		res = failed(http.StatusBadRequest, "bad_request", "query is too long.")
	case errors.Is(err, query.ErrInvalid):
		res = failed(http.StatusBadRequest, "bad_request", "query is not valid.")
	case err != nil:
		a.opts.Logger.Error("api: reading the store failed", "endpoint", "search", "err", err)
		res = failed(http.StatusInternalServerError, "internal", "The store could not be read. Details are in the log.")
	default:
		resp := searchResponse{Mails: []mailSummary{}, Limit: limit}
		if len(rows) > limit {
			rows, resp.Truncated = rows[:limit], true
		}
		for _, m := range rows {
			resp.Mails = append(resp.Mails, summary(m))
		}
		res = result{status: http.StatusOK, count: len(resp.Mails), body: resp}
	}
	res.by = req.By
	return res
}

var (
	mailIDRe = regexp.MustCompile(`^pw_[0-9A-Za-z_]{1,64}$`)
	targetRe = regexp.MustCompile(`^[a-z0-9_]{1,32}$`)
)

// mail returns one mail (H5): addresses in full only for admin.
func (a *API) mail(r *http.Request, c caller) result {
	if r.URL.RawQuery != "" {
		return failed(http.StatusBadRequest, "bad_request", "This endpoint takes no URL parameters.")
	}
	id := r.PathValue("id")
	if !mailIDRe.MatchString(id) {
		return failed(http.StatusNotFound, "not_found", "There is no mail with this id.")
	}
	m, err := a.opts.Query.Mail(r.Context(), id, access(c))
	if errors.Is(err, query.ErrNotFound) {
		return failed(http.StatusNotFound, "not_found", "There is no mail with this id. It may have been deleted by retention.")
	}
	if err != nil {
		a.opts.Logger.Error("api: reading the store failed", "endpoint", "mail", "err", err)
		return failed(http.StatusInternalServerError, "internal", "The store could not be read. Details are in the log.")
	}
	return result{status: http.StatusOK, count: 1, viewed: m.ID, body: detail(m, c.role == auth.RoleAdmin)}
}

type listResponse struct {
	Page    int           `json:"page"`
	HasNext bool          `json:"has_next"`
	Mails   []mailSummary `json:"mails"`
}

var listParams = map[string]bool{"state": true, "target": true, "sender": true, "key_id": true, "from": true, "to": true,
	"page": true}

// mails lists mails with filters, newest first, query.PageSize per page
// (H6, admin only). Filters never hold an address, so they may be URL
// parameters.
func (a *API) mails(r *http.Request, _ caller) result {
	q, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		return failed(http.StatusBadRequest, "bad_request", "The URL parameters could not be read.")
	}
	for k, v := range q {
		if !listParams[k] || len(v) != 1 {
			return failed(http.StatusBadRequest, "bad_request",
				"Known parameters, each once: state, target, sender, key_id, from, to, page.")
		}
	}
	f := query.ListFilter{State: q.Get("state"), Target: q.Get("target"), Sender: q.Get("sender"), KeyID: q.Get("key_id")}
	for _, d := range []struct {
		name string
		to   *time.Time
	}{{"from", &f.From}, {"to", &f.To}} {
		if v := q.Get(d.name); v != "" {
			t, err := time.Parse(time.DateOnly, v)
			if err != nil {
				return failed(http.StatusBadRequest, "bad_request", d.name+" must be a date, YYYY-MM-DD.")
			}
			*d.to = t
		}
	}
	if p := q.Get("page"); p != "" {
		n, err := strconv.Atoi(p)
		if err != nil || n < 1 {
			return failed(http.StatusBadRequest, "bad_request", "page must be a whole number from 1.")
		}
		f.Page = n
	}
	page, err := a.opts.Query.List(r.Context(), f)
	if errors.Is(err, query.ErrInvalid) {
		return failed(http.StatusBadRequest, "bad_request", "The filter is not valid ("+filterProblem(err)+").")
	}
	if err != nil {
		a.opts.Logger.Error("api: reading the store failed", "endpoint", "mails", "err", err)
		return failed(http.StatusInternalServerError, "internal", "The store could not be read. Details are in the log.")
	}
	resp := listResponse{Page: page.Filter.Page, HasNext: page.HasNext, Mails: []mailSummary{}}
	for _, m := range page.Rows {
		resp.Mails = append(resp.Mails, summary(m))
	}
	return result{status: http.StatusOK, count: len(resp.Mails), body: resp}
}

// filterProblem names what was wrong, from a fixed set of texts.
func filterProblem(err error) string {
	for _, p := range []string{"page", "state", "target", "sender", "key", "from is after to"} {
		if err.Error() == query.ErrInvalid.Error()+": "+p {
			return p
		}
	}
	return "unknown value"
}

// overview returns every target's state and the recent problems (H6, admin
// only).
func (a *API) overview(r *http.Request, _ caller) result {
	if r.URL.RawQuery != "" {
		return failed(http.StatusBadRequest, "bad_request", "This endpoint takes no URL parameters.")
	}
	o, err := a.opts.Query.Overview(r.Context())
	if err != nil {
		a.opts.Logger.Error("api: reading the store failed", "endpoint", "overview", "err", err)
		return failed(http.StatusInternalServerError, "internal", "The store could not be read. Details are in the log.")
	}
	resp := overviewResponse{Now: utc(o.Now), Since: utc(o.Since), Targets: []targetOverview{}, Problems: []mailSummary{}}
	for _, t := range o.Targets {
		to := targetOverview{
			Target: t.Target, Configured: t.Configured, Recent: map[store.Status]int{}, RecentTotal: t.RecentTotal,
			Queued: t.Queued, OldestQueuedAt: optTime(t.OldestQueued), Optional: t.Optional,
		}
		if !t.Health.Since.IsZero() {
			to.State, to.StateSince, to.DownReason = "up", optTime(t.Health.Since), t.Health.Reason
			if t.Health.Down {
				to.State = "down"
			}
		}
		for _, sc := range t.Recent {
			to.Recent[sc.Status] = sc.Count
		}
		if !t.OldestQueued.IsZero() {
			secs := int64(t.OldestQueuedAge / time.Second)
			to.OldestQueuedAgeSeconds = &secs
		}
		resp.Targets = append(resp.Targets, to)
	}
	for _, m := range o.Problems {
		resp.Problems = append(resp.Problems, summary(m))
	}
	return result{status: http.StatusOK, count: len(resp.Problems), body: resp}
}

func statusOf(e *apiError) int {
	if e.Error == "too_large" {
		return http.StatusRequestEntityTooLarge
	}
	return http.StatusBadRequest
}
