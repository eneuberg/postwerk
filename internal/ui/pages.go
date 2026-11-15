package ui

import (
	"net/http"
)

func (u *UI) pageRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /{$}", u.authed(u.overview))
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

func (u *UI) queryFailed(w http.ResponseWriter, r *http.Request, page string, err error) {
	u.opts.Logger.Error("ui: reading the store failed", "page", page, "err", err)
	u.fail(w, r, http.StatusInternalServerError, "The store could not be read. Details are in the log.")
}
