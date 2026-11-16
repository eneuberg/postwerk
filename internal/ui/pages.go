package ui

import (
	"errors"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"time"

	"github.com/eneuberg/postwerk/internal/query"
	"github.com/eneuberg/postwerk/internal/store"
)

func (u *UI) pageRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /{$}", u.authed(u.overview))
	mux.HandleFunc("GET /mails", u.authed(u.mails))
	// The search form is on the mails page and posts to it.
	mux.HandleFunc("POST /mails", u.authed(u.search))
	mux.HandleFunc("GET /mails/{id}", u.authed(u.mail))
	// The old search page; its query, if any, is dropped.
	mux.HandleFunc("GET /search", u.authed(redirectTo("/mails")))
}

// overview shows every target and the recent problems (U10).
func (u *UI) overview(w http.ResponseWriter, r *http.Request, sess session) {
	o, err := u.opts.Query.Overview(r.Context())
	if err != nil {
		u.queryFailed(w, r, "overview", err)
		return
	}
	u.render(w, r, http.StatusOK, "overview", pageView(sess, "overview", "Overview", o))
}

type mailsView struct {
	Search  searchView
	List    query.ListPage
	States  []store.Status
	Targets []string
	// The filter as given, for the form.
	State, Target, From, To string
	Filtered                bool
	PrevURL, NextURL        string
	Error                   string
}

// mails lists mails with filters and pages (U10). Filters are GET
// parameters: they never hold an address.
func (u *UI) mails(w http.ResponseWriter, r *http.Request, sess session) {
	q := r.URL.Query()
	v := mailsView{
		Search: newSearchView(query.ByAny),
		States: query.States, Targets: u.opts.Query.Targets(),
		State: q.Get("state"), Target: q.Get("target"), From: q.Get("from"), To: q.Get("to"),
	}
	v.Filtered = v.State != "" || v.Target != "" || v.From != "" || v.To != ""
	if v.Target != "" && !slices.Contains(v.Targets, v.Target) {
		v.Targets = append(v.Targets, v.Target) // a target no longer configured
	}
	f := query.ListFilter{State: v.State, Target: v.Target}
	var bad bool
	f.From, bad = parseDay(v.From)
	if !bad {
		f.To, bad = parseDay(v.To)
	}
	if p := q.Get("page"); p != "" && !bad {
		n, err := strconv.Atoi(p)
		f.Page, bad = n, err != nil || n < 1
	}
	if bad {
		v.Error = "The filter is not valid. Dates are YYYY-MM-DD."
		u.render(w, r, http.StatusBadRequest, "mails", pageView(sess, "mails", "Mails", v))
		return
	}
	list, err := u.opts.Query.List(r.Context(), f)
	if errors.Is(err, query.ErrInvalid) {
		v.Error = "The filter is not valid: " + filterProblem(err)
		u.render(w, r, http.StatusBadRequest, "mails", pageView(sess, "mails", "Mails", v))
		return
	}
	if err != nil {
		u.queryFailed(w, r, "mails", err)
		return
	}
	v.List = list
	pageURL := func(n int) string {
		p := url.Values{}
		for _, kv := range [][2]string{{"state", v.State}, {"target", v.Target}, {"from", v.From}, {"to", v.To}} {
			if kv[1] != "" {
				p.Set(kv[0], kv[1])
			}
		}
		if n > 1 {
			p.Set("page", strconv.Itoa(n))
		}
		if len(p) == 0 {
			return "/mails"
		}
		return "/mails?" + p.Encode()
	}
	if list.Filter.Page > 1 {
		v.PrevURL = pageURL(list.Filter.Page - 1)
	}
	if list.HasNext {
		v.NextURL = pageURL(list.Filter.Page + 1)
	}
	u.render(w, r, http.StatusOK, "mails", pageView(sess, "mails", "Mails", v))
}

// filterProblem names what was wrong, from a fixed set of texts.
func filterProblem(err error) string {
	for _, p := range []string{"page", "state", "target", "from is after to"} {
		if err.Error() == query.ErrInvalid.Error()+": "+p {
			return p + "."
		}
	}
	return "unknown value."
}

func parseDay(s string) (time.Time, bool) {
	if s == "" {
		return time.Time{}, false
	}
	t, err := time.Parse(time.DateOnly, s)
	return t, err != nil
}

var mailIDRe = regexp.MustCompile(`^pw_[0-9A-Za-z_]{1,64}$`)

// mail shows one mail (U10, U12) and records who opened it (U24): the
// page is shown only once its ui.view event is in the event log.
func (u *UI) mail(w http.ResponseWriter, r *http.Request, sess session) {
	id := r.PathValue("id")
	if !mailIDRe.MatchString(id) {
		u.fail(w, r, http.StatusNotFound, "There is no mail with this id.")
		return
	}
	m, err := u.opts.Query.Mail(r.Context(), id, query.Full)
	if errors.Is(err, query.ErrNotFound) {
		u.fail(w, r, http.StatusNotFound, "There is no mail with this id. It may have been deleted by retention.")
		return
	}
	if err != nil {
		u.queryFailed(w, r, "mail", err)
		return
	}
	err = u.opts.Events.Append(r.Context(), store.Event{
		Time: u.opts.Now(), Kind: store.KindUIView, Level: store.Info, MailID: m.ID, Actor: sess.name,
	})
	if err != nil {
		u.opts.Logger.Error("ui: writing the view event failed; page not shown", "err", err)
		u.fail(w, r, http.StatusServiceUnavailable, "Opening the mail could not be recorded, so it is not shown. Please try again later.")
		return
	}
	u.render(w, r, http.StatusOK, "mail", pageView(sess, "mails", "Mail "+m.ID, m))
}

type searchKind struct {
	Value query.SearchBy
	Label string
}

var searchKinds = []searchKind{
	{query.ByAny, "Anything"},
	{query.ByID, "Mail id (pw_…)"},
	{query.ByRef, "X-Postwerk-Ref"},
	{query.ByMessageID, "Message-ID"},
	{query.ByAddress, "Recipient or sender address"},
}

// searchView is the search form at the top of the mails page and, after a
// search, its outcome in place of the list.
type searchView struct {
	Kinds []searchKind
	By    query.SearchBy
	// Done: the page shows the outcome of a search, Results or Error,
	// instead of the list.
	Done    bool
	Results []store.MailSummary
	Error   string
	Max     int
}

func newSearchView(by query.SearchBy) searchView {
	return searchView{Kinds: searchKinds, By: by, Max: query.MaxResults}
}

// search runs a search from a POST body (U11) and shows the mails page
// with the results in place of the list. The query is never logged, never
// written to an event and never echoed back into the page; the event names
// only who searched and by what.
func (u *UI) search(w http.ResponseWriter, r *http.Request, sess session) {
	v := newSearchView(query.SearchBy(r.PostForm.Get("by")))
	v.Done = true
	show := func(status int) {
		u.render(w, r, status, "mails", pageView(sess, "mails", "Search results", mailsView{Search: v}))
	}
	if !query.ValidSearchBy(v.By) {
		v.By, v.Error = query.ByAny, "Choose what to search by."
		show(http.StatusBadRequest)
		return
	}
	if !u.searches.allow(sess.name) {
		v.Error = "Too many searches. Please wait a minute."
		show(http.StatusTooManyRequests)
		return
	}
	if err := u.event(r.Context(), store.KindUISearch, store.Info, sess.name, string(v.By)); err != nil {
		u.opts.Logger.Error("ui: writing the search event failed; search refused", "err", err)
		u.fail(w, r, http.StatusServiceUnavailable, "The search could not be recorded, so it was not run. Please try again later.")
		return
	}
	results, err := u.opts.Query.Search(r.Context(), v.By, r.PostForm.Get("q"))
	switch {
	case errors.Is(err, query.ErrEmptyQuery):
		v.Error = "Enter something to search for."
	case errors.Is(err, query.ErrQueryTooLong):
		v.Error = "The search text is too long."
	case errors.Is(err, query.ErrInvalid):
		v.Error = "The search text is not valid."
	case err != nil:
		u.queryFailed(w, r, "search", err)
		return
	}
	if v.Error != "" {
		show(http.StatusBadRequest)
		return
	}
	v.Results = results
	show(http.StatusOK)
}

func (u *UI) queryFailed(w http.ResponseWriter, r *http.Request, page string, err error) {
	u.opts.Logger.Error("ui: reading the store failed", "page", page, "err", err)
	u.fail(w, r, http.StatusInternalServerError, "The store could not be read. Details are in the log.")
}
