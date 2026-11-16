package ui

import (
	"bytes"
	"fmt"
	"html/template"
	"io/fs"
	"net/http"
	"path"
	"strings"
	"time"

	"github.com/eneuberg/postwerk/internal/query"
	"github.com/eneuberg/postwerk/internal/store"
)

// view is what every page template gets.
type view struct {
	Title    string
	Nav      string // the active navigation entry
	Instance string
	User     string
	// FileUser is set when the account is from the tokens file.
	FileUser bool
	CSRF     string // for the logout form and other POST forms
	Flash    string // a one-time notice
	Data     any
}

func parseTemplates() (map[string]*template.Template, error) {
	base, err := template.New("layout.html").Funcs(funcs).ParseFS(templateFS, "templates/layout.html")
	if err != nil {
		return nil, fmt.Errorf("ui: parse layout: %w", err)
	}
	names, err := fs.Glob(templateFS, "templates/*.html")
	if err != nil {
		return nil, err
	}
	pages := map[string]*template.Template{}
	for _, n := range names {
		name := strings.TrimSuffix(path.Base(n), ".html")
		if name == "layout" {
			continue
		}
		t, err := template.Must(base.Clone()).ParseFS(templateFS, n)
		if err != nil {
			return nil, fmt.Errorf("ui: parse %s: %w", n, err)
		}
		pages[name] = t
	}
	return pages, nil
}

// render writes a page. It renders into a buffer first, so a template error
// never leaves half a page.
func (u *UI) render(w http.ResponseWriter, r *http.Request, status int, name string, v view) {
	t := u.pages[name]
	if t == nil {
		u.opts.Logger.Error("ui: no such template", "template", name)
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	v.Instance = u.opts.Instance
	if v.User != "" {
		v.FileUser = u.fileAccount(v.User)
	}
	var buf bytes.Buffer
	if err := t.ExecuteTemplate(&buf, "layout.html", v); err != nil {
		u.opts.Logger.Error("ui: rendering failed", "template", name, "err", err)
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write(buf.Bytes())
}

// pageView is a view for a logged-in page.
func pageView(sess session, nav, title string, data any) view {
	return view{Title: title, Nav: nav, User: sess.name, CSRF: sess.csrf, Data: data}
}

type failView struct {
	Status  int
	Message string
}

// fail shows an error page. msg is a fixed text, never user input.
func (u *UI) fail(w http.ResponseWriter, r *http.Request, status int, msg string) {
	v := view{Title: http.StatusText(status), Data: failView{Status: status, Message: msg}}
	if sess, ok := u.currentSession(r); ok {
		v = pageView(sess, "", http.StatusText(status), v.Data)
	}
	u.render(w, r, status, "error", v)
}

var funcs = template.FuncMap{
	"when":        formatTime,
	"ago":         formatAge,
	"size":        formatSize,
	"status":      statusLabel,
	"statusClass": func(s store.Status) string { return "st-" + string(s) },
	"pseudo":      func(a query.Address) bool { return a.State == query.Pseudonymized },
	"nullSender":  func(a query.Address) bool { return a.State == query.NullSender },
	"date":        func(t time.Time) string { return dateValue(t) },
	"inc":         func(n int) int { return n + 1 },
	"dec":         func(n int) int { return n - 1 },
	"deref":       func(p *int) int { return *p },
	"levelClass":  func(l store.Level) string { return "lv-" + string(l) },
}

// formatTime shows a time in UTC to the second, or a dash.
func formatTime(t time.Time) string {
	if t.IsZero() {
		return "—"
	}
	return t.UTC().Format("2006-01-02 15:04:05") + " UTC"
}

// formatAge shows a duration in the two largest units.
func formatAge(d time.Duration) string {
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%d s", int(d/time.Second))
	case d < time.Hour:
		return fmt.Sprintf("%d min", int(d/time.Minute))
	case d < 24*time.Hour:
		return fmt.Sprintf("%d h %d min", int(d/time.Hour), int(d%time.Hour/time.Minute))
	}
	return fmt.Sprintf("%d d %d h", int(d/(24*time.Hour)), int(d%(24*time.Hour)/time.Hour))
}

// formatSize shows a byte count in B, KiB or MiB.
func formatSize(n int) string {
	switch {
	case n < 1024:
		return fmt.Sprintf("%d B", n)
	case n < 1<<20:
		return fmt.Sprintf("%.1f KiB", float64(n)/1024)
	}
	return fmt.Sprintf("%.1f MiB", float64(n)/(1<<20))
}

func statusLabel(s store.Status) string {
	if s == "" {
		return "none"
	}
	return strings.ReplaceAll(string(s), "_", " ")
}

func dateValue(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.DateOnly)
}
