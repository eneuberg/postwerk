// Package ui serves Postwerk's internal web UI on the admin listener
// (H3, U1–U16; docs/ui.md).
//
// Every response goes through the same outer layer: strict security headers,
// a Host check against DNS rebinding and a bound on the request body. Every
// page needs a session from a token login; every POST needs the session's
// CSRF token and must not come from another site. The pages read through
// internal/query, which masks addresses by role; nothing here touches
// message content. Searches are POSTs, and neither the query nor an address
// is ever logged or written to an event.
package ui

import (
	"context"
	"embed"
	"errors"
	"html/template"
	"log/slog"
	"net"
	"net/http"
	"runtime"
	"slices"
	"strings"
	"time"

	"github.com/eneuberg/postwerk/internal/auth"
	"github.com/eneuberg/postwerk/internal/query"
	"github.com/eneuberg/postwerk/internal/store"
)

//go:embed templates/*.html
var templateFS embed.FS

//go:embed static
var staticFS embed.FS

// Defaults (U5, U6, U16).
const (
	DefaultIdleTimeout       = 30 * time.Minute
	DefaultMaxSessionAge     = 8 * time.Hour
	DefaultLoginWait         = 5 * time.Second
	DefaultSearchesPerMinute = 30
	// maxSessions bounds the sessions kept in memory; the least recently
	// used one goes first.
	maxSessions = 1000
	// maxBody bounds every request body; the forms are tiny.
	maxBody = 16 << 10
)

// ContentSecurityPolicy is sent with every response (U9). The UI has no
// JavaScript; styles and images come from the UI itself.
const ContentSecurityPolicy = "default-src 'none'; style-src 'self'; img-src 'self'; form-action 'self'; " +
	"frame-ancestors 'none'; base-uri 'none'"

// Verifier checks UI tokens (auth.Tokens).
type Verifier interface {
	Verify(name, token string) (auth.Role, bool)
	Known(name string) bool
}

// EventLog takes the UI's own events (store.Store).
type EventLog interface {
	Append(ctx context.Context, e store.Event) error
}

// Options configure the UI.
type Options struct {
	Query  *query.Service
	Tokens Verifier
	Events EventLog
	// Hosts are extra host names (POSTWERK_ADMIN_HOSTS) besides localhost,
	// 127.0.0.1 and ::1.
	Hosts []string
	// Instance is shown in the page header.
	Instance string
	Logger   *slog.Logger
	// Now returns the current time; nil means time.Now.
	Now func() time.Time

	// Zero values take the defaults above; tests shorten them.
	IdleTimeout       time.Duration
	MaxSessionAge     time.Duration
	LoginSlots        int // default runtime.GOMAXPROCS(0)
	LoginWait         time.Duration
	SearchesPerMinute int
}

// UI is the web UI's HTTP handler.
type UI struct {
	opts       Options
	sessions   *sessions
	loginSlots chan struct{}
	searches   *rateLimit
	pages      map[string]*template.Template
	handler    http.Handler
}

// New returns the UI.
func New(opts Options) (*UI, error) {
	if opts.Query == nil || opts.Tokens == nil || opts.Events == nil {
		return nil, errors.New("ui: Query, Tokens and Events are required")
	}
	if opts.Logger == nil {
		opts.Logger = slog.New(slog.DiscardHandler)
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.IdleTimeout <= 0 {
		opts.IdleTimeout = DefaultIdleTimeout
	}
	if opts.MaxSessionAge <= 0 {
		opts.MaxSessionAge = DefaultMaxSessionAge
	}
	if opts.LoginSlots <= 0 {
		opts.LoginSlots = runtime.GOMAXPROCS(0)
	}
	if opts.LoginWait <= 0 {
		opts.LoginWait = DefaultLoginWait
	}
	if opts.SearchesPerMinute <= 0 {
		opts.SearchesPerMinute = DefaultSearchesPerMinute
	}
	pages, err := parseTemplates()
	if err != nil {
		return nil, err
	}
	u := &UI{
		opts:       opts,
		sessions:   newSessions(opts.Now, opts.IdleTimeout, opts.MaxSessionAge, maxSessions),
		loginSlots: make(chan struct{}, opts.LoginSlots),
		searches:   newRateLimit(opts.Now, opts.SearchesPerMinute, time.Minute),
		pages:      pages,
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /static/{file}", serveStatic)
	mux.HandleFunc("GET /login", u.loginForm)
	mux.HandleFunc("POST /login", u.login)
	mux.HandleFunc("POST /logout", u.authed(u.logout))
	u.pageRoutes(mux)

	cop := http.NewCrossOriginProtection()
	cop.SetDenyHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u.fail(w, r, http.StatusForbidden, "This request came from another site and was refused.")
	}))
	u.handler = u.outer(cop.Handler(mux))
	return u, nil
}

// ServeHTTP implements http.Handler.
func (u *UI) ServeHTTP(w http.ResponseWriter, r *http.Request) { u.handler.ServeHTTP(w, r) }

// NewServer returns an HTTP server for the admin listener with tight
// timeouts. Its own error log goes to logger; it never holds request
// content.
func NewServer(addr string, h http.Handler, logger *slog.Logger) *http.Server {
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	return &http.Server{
		Addr:              addr,
		Handler:           h,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		// A login waits up to LoginWait for a slot and then runs PBKDF2.
		WriteTimeout:   30 * time.Second,
		IdleTimeout:    60 * time.Second,
		MaxHeaderBytes: 16 << 10,
		ErrorLog:       slog.NewLogLogger(logger.Handler(), slog.LevelWarn),
	}
}

// outer sets the security headers on every response, refuses unknown
// hosts and bounds the request body (U7, U9).
func (u *UI) outer(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", ContentSecurityPolicy)
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Cache-Control", "no-store")
		h.Set("X-Frame-Options", "DENY")
		if !u.hostAllowed(r.Host) {
			// Plain text: the page template is not for unknown hosts.
			http.Error(w, "Forbidden: this host name is not allowed (docs/ui.md).", http.StatusForbidden)
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, maxBody)
		next.ServeHTTP(w, r)
	})
}

// hostAllowed checks the Host header against the loopback names and the
// configured hosts, ignoring the port: a tunnel may use any local port, and
// DNS rebinding works through a name the attacker controls, never through
// these (U7).
func (u *UI) hostAllowed(hostport string) bool {
	host := hostport
	if h, _, err := net.SplitHostPort(hostport); err == nil {
		host = h
	} else if strings.HasPrefix(host, "[") && strings.HasSuffix(host, "]") {
		host = host[1 : len(host)-1]
	}
	host = strings.ToLower(host)
	switch host {
	case "":
		return false
	case "localhost", "127.0.0.1", "::1":
		return true
	}
	return slices.Contains(u.opts.Hosts, host)
}

// authed runs fn with the request's session, or sends the browser to the
// login page. A POST must carry the session's CSRF token (U8).
func (u *UI) authed(fn func(http.ResponseWriter, *http.Request, session)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sess, ok := u.currentSession(r)
		if !ok {
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		}
		if r.Method == http.MethodPost {
			if err := r.ParseForm(); err != nil {
				u.fail(w, r, http.StatusBadRequest, "The form could not be read.")
				return
			}
			if !equalSecret(sess.csrf, r.PostForm.Get("csrf")) {
				u.fail(w, r, http.StatusForbidden, "The form has expired or did not come from this page. Reload the page and try again.")
				return
			}
		}
		fn(w, r, sess)
	}
}

// staticTypes are the files under static/ and their types; nothing else is
// served.
var staticTypes = map[string]string{
	"ui.css":   "text/css; charset=utf-8",
	"logo.svg": "image/svg+xml",
}

func serveStatic(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("file")
	ctype, ok := staticTypes[name]
	if !ok {
		http.NotFound(w, r)
		return
	}
	b, err := staticFS.ReadFile("static/" + name)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", ctype)
	_, _ = w.Write(b)
}

// event writes one of the UI's own events.
func (u *UI) event(ctx context.Context, kind string, level store.Level, actor, text string) error {
	return u.opts.Events.Append(ctx, store.Event{Time: u.opts.Now(), Kind: kind, Level: level, Actor: actor, Text: text})
}
