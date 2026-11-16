// Package api serves Postwerk's read-only JSON query API on its own
// listener (H2, H4–H9; docs/api.md).
//
// Another application in the cluster asks "what happened to the mail for
// this id, ref, Message-ID or address?" with a bearer token. A read token can
// only look mails up, one search or one mail at a time, with addresses
// masked; an admin token can also list mails and see the per-target
// overview, with addresses in full. Nothing here returns message content,
// the subject or any header but Message-ID: all reads go through
// internal/query, which never hands content out and masks by access level.
//
// Every response goes through one outer layer: security headers, an optional
// Host allow-list, a refusal of browser requests and a global rate limit.
// Every endpoint then checks the method, the content type and the body
// size, the bearer token and a per-token rate limit. Every authenticated
// request and every failed authentication is an event; search terms never
// appear in a URL, an event or a log line.
package api

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"log/slog"
	"mime"
	"net"
	"net/http"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/eneuberg/postwerk/internal/auth"
	"github.com/eneuberg/postwerk/internal/query"
	"github.com/eneuberg/postwerk/internal/store"
)

// Event kinds of the API (H7). Their text holds only fixed words, counts and
// status codes, never a search term, an id or an address.
const (
	// KindRequest is one authenticated request: the token name as actor,
	// the endpoint, HTTP status and number of mails returned as text.
	KindRequest = "api.request"
	// KindForbidden is a request for an endpoint the token's role may not
	// use (warn).
	KindForbidden = "api.forbidden"
	// KindAuthFailed is a missing, malformed or wrong token (warn). The
	// actor is the token name only if it is in the tokens file.
	KindAuthFailed = "api.auth_failed"
	// KindView is a mail's detail returned to a token: the token name as
	// actor and the mail's id, like ui.view. It takes the place of
	// api.request for that response; it has no text.
	KindView = "api.view"
)

// Defaults (H8).
const (
	// DefaultPerToken is the number of requests per minute per token name.
	DefaultPerToken = 60
	// DefaultGlobal is the number of requests per minute for all callers
	// together, authenticated or not.
	DefaultGlobal = 600
	// DefaultCheckWait is how long a request waits for a token check slot.
	DefaultCheckWait = 5 * time.Second
	// ReadMaxResults bounds the mails one search returns to a read token;
	// admin tokens get up to query.MaxResults.
	ReadMaxResults = 20
	// maxBody bounds every request body; a search is tiny.
	maxBody = 4 << 10
	// maxAuthorization bounds the Authorization header we look at.
	maxAuthorization = 1200
)

// ContentSecurityPolicy is sent with every response: nothing may be loaded
// or framed, should a browser ever render one.
const ContentSecurityPolicy = "default-src 'none'; frame-ancestors 'none'; base-uri 'none'"

// Verifier checks API tokens (auth.Tokens).
type Verifier interface {
	Verify(name, token string) (auth.Role, bool)
	Known(name string) bool
}

// EventLog takes the API's events (store.Store).
type EventLog interface {
	Append(ctx context.Context, e store.Event) error
}

// Options configure the API.
type Options struct {
	Query  *query.Service
	Tokens Verifier
	Events EventLog
	// Hosts, if not empty, are the only Host names (without port) the API
	// answers to (POSTWERK_API_HOSTS). Empty: any.
	Hosts  []string
	Logger *slog.Logger
	// Now returns the current time; nil means time.Now.
	Now func() time.Time

	// Zero values take the defaults above; tests change them.
	PerToken   int
	Global     int
	CheckSlots int // default runtime.GOMAXPROCS(0)
	CheckWait  time.Duration
}

// API is the query API's HTTP handler.
type API struct {
	opts     Options
	perToken *rateLimit
	global   *rateLimit
	slots    chan struct{}
	cache    *goodTokens
	handler  http.Handler
}

// New returns the API handler.
func New(opts Options) (*API, error) {
	if opts.Query == nil || opts.Tokens == nil || opts.Events == nil {
		return nil, errors.New("api: Query, Tokens and Events are required")
	}
	if opts.Logger == nil {
		opts.Logger = slog.New(slog.DiscardHandler)
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.PerToken <= 0 {
		opts.PerToken = DefaultPerToken
	}
	if opts.Global <= 0 {
		opts.Global = DefaultGlobal
	}
	if opts.CheckSlots <= 0 {
		opts.CheckSlots = runtime.GOMAXPROCS(0)
	}
	if opts.CheckWait <= 0 {
		opts.CheckWait = DefaultCheckWait
	}
	a := &API{
		opts:     opts,
		perToken: newRateLimit(opts.Now, opts.PerToken, time.Minute),
		global:   newRateLimit(opts.Now, opts.Global, time.Minute),
		slots:    make(chan struct{}, opts.CheckSlots),
		cache:    newGoodTokens(),
	}
	mux := http.NewServeMux()
	a.routes(mux)
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		writeError(w, http.StatusNotFound, "not_found", "No such endpoint (docs/api.md).")
	})
	a.handler = a.outer(mux)
	return a, nil
}

// ServeHTTP implements http.Handler.
func (a *API) ServeHTTP(w http.ResponseWriter, r *http.Request) { a.handler.ServeHTTP(w, r) }

// NewServer returns an HTTP server for the API listener with tight timeouts
// (H9). Its own error log goes to logger; it never holds request content.
func NewServer(addr string, h http.Handler, logger *slog.Logger) *http.Server {
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	return &http.Server{
		Addr:              addr,
		Handler:           h,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		// A request waits up to CheckWait for a token check slot and then
		// runs PBKDF2.
		WriteTimeout:   20 * time.Second,
		IdleTimeout:    60 * time.Second,
		MaxHeaderBytes: 8 << 10,
		ErrorLog:       slog.NewLogLogger(logger.Handler(), slog.LevelWarn),
	}
}

// outer sets the headers of every response, checks the Host, refuses
// browsers and applies the global rate limit (H9).
func (a *API) outer(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", ContentSecurityPolicy)
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Cache-Control", "no-store")
		h.Set("X-Frame-Options", "DENY")
		if !a.hostAllowed(r.Host) {
			writeError(w, http.StatusForbidden, "host_not_allowed", "This host name is not allowed (POSTWERK_API_HOSTS).")
			return
		}
		// The API is for other services, never for browsers: no CORS, and a
		// request a browser marks as cross-site or from a page is refused.
		if r.Header.Get("Origin") != "" || r.Header.Get("Sec-Fetch-Site") != "" || r.Header.Get("Sec-Fetch-Mode") != "" {
			writeError(w, http.StatusForbidden, "browser_refused", "Browser requests are not accepted.")
			return
		}
		if ok, wait := a.global.allow(""); !ok {
			tooMany(w, wait)
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, maxBody)
		next.ServeHTTP(w, r)
	})
}

func (a *API) hostAllowed(hostport string) bool {
	if len(a.opts.Hosts) == 0 {
		return true
	}
	host := hostport
	if h, _, err := net.SplitHostPort(hostport); err == nil {
		host = h
	} else if strings.HasPrefix(host, "[") && strings.HasSuffix(host, "]") {
		host = host[1 : len(host)-1]
	}
	return host != "" && slices.Contains(a.opts.Hosts, strings.ToLower(host))
}

// caller is an authenticated request's token.
type caller struct {
	name string
	role auth.Role
}

// endpoint describes one route for the checks in handle.
type endpoint struct {
	name   string // in events: fixed, never user input
	method string
	admin  bool // only for admin tokens
	fn     func(r *http.Request, c caller) result
}

// result is what an endpoint returns: the HTTP status, the number of mails
// in it, the search kind for searches, and the JSON body (an *apiError
// unless the status is 2xx).
type result struct {
	status int
	count  int
	by     query.SearchBy
	// viewed is the id of the mail whose detail is returned (2xx only).
	viewed string
	body   any
}

func failed(status int, code, msg string) result {
	return result{status: status, body: &apiError{Error: code, Message: msg}}
}

// apiError is the JSON body of every error.
type apiError struct {
	Error   string `json:"error"`
	Message string `json:"message"`
}

// handle runs the checks every endpoint shares, in this order: method,
// content type and body, token, per-token rate limit, role. Then it runs
// the endpoint, writes the request event and only then the response, so
// nothing is returned that the audit log misses.
func (a *API) handle(e endpoint) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != e.method {
			w.Header().Set("Allow", e.method)
			writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "Use "+e.method+" for this endpoint.")
			return
		}
		if status, code, msg := checkBody(r, e.method); status != 0 {
			writeError(w, status, code, msg)
			return
		}
		c, ok := a.authenticate(w, r)
		if !ok {
			return
		}
		if ok, wait := a.perToken.allow(c.name); !ok {
			tooMany(w, wait)
			return
		}
		if e.admin && c.role != auth.RoleAdmin {
			if err := a.event(r.Context(), KindForbidden, store.Warn, c.name, e.name); err != nil {
				a.opts.Logger.Error("api: writing the event failed", "kind", KindForbidden, "err", err)
			}
			writeError(w, http.StatusForbidden, "forbidden", "This token's role may not use this endpoint.")
			return
		}
		res := e.fn(r, c)
		ev := store.Event{Time: a.opts.Now(), Kind: KindView, Level: store.Info, Actor: c.name, MailID: res.viewed}
		if res.viewed == "" {
			ev.Kind, ev.Text = KindRequest, e.name
			if res.by != "" {
				ev.Text += " by=" + string(res.by)
			}
			ev.Text += " status=" + strconv.Itoa(res.status) + " results=" + strconv.Itoa(res.count)
		}
		if err := a.opts.Events.Append(r.Context(), ev); err != nil {
			a.opts.Logger.Error("api: writing the request event failed; nothing returned", "endpoint", e.name, "err", err)
			w.Header().Set("Retry-After", "5")
			writeError(w, http.StatusServiceUnavailable, "unavailable",
				"The request could not be recorded, so nothing is returned. Try again later.")
			return
		}
		writeJSON(w, res.status, res.body)
	}
}

// checkBody: a GET has no body; a POST has a JSON body. Neither takes URL
// parameters unless the endpoint reads them itself (GET /v1/mails).
func checkBody(r *http.Request, method string) (int, string, string) {
	if method == http.MethodGet {
		if r.ContentLength > 0 || len(r.TransferEncoding) > 0 {
			return http.StatusBadRequest, "bad_request", "A GET request has no body."
		}
		return 0, "", ""
	}
	if r.URL.RawQuery != "" {
		return http.StatusBadRequest, "bad_request", "Put the request in the JSON body, not in the URL."
	}
	mt, params, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mt != "application/json" {
		return http.StatusUnsupportedMediaType, "unsupported_media_type", "Send Content-Type: application/json."
	}
	if cs, ok := params["charset"]; ok && !strings.EqualFold(cs, "utf-8") {
		return http.StatusUnsupportedMediaType, "unsupported_media_type", "The body must be UTF-8."
	}
	if r.ContentLength > maxBody {
		return http.StatusRequestEntityTooLarge, "too_large", "The body is too large."
	}
	return 0, "", ""
}

// decode reads exactly one JSON object with known fields into v.
func decode(r *http.Request, v any) *apiError {
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			return &apiError{"too_large", "The body is too large."}
		}
		return &apiError{"bad_request", "The body is not a valid JSON object for this endpoint."}
	}
	if dec.More() {
		return &apiError{"bad_request", "The body holds more than one JSON value."}
	}
	if _, err := dec.Token(); err == nil {
		return &apiError{"bad_request", "The body holds more than one JSON value."}
	}
	return nil
}

// authenticate reads "Authorization: Bearer <name>.<token>" and checks it.
// A wrong token costs one PBKDF2 check, whatever was sent; a token that was
// right before costs one HMAC. At most CheckSlots checks run at once.
func (a *API) authenticate(w http.ResponseWriter, r *http.Request) (caller, bool) {
	name, token, ok := parseBearer(r.Header.Get("Authorization"))
	if !ok {
		a.authFailed(w, r, "")
		return caller{}, false
	}
	if role, ok := a.cache.lookup(name, token); ok {
		return caller{name: name, role: role}, true
	}
	timer := time.NewTimer(a.opts.CheckWait)
	select {
	case a.slots <- struct{}{}:
		timer.Stop()
	case <-timer.C:
		a.opts.Logger.Warn("api request refused, too many token checks at once", "limit", cap(a.slots))
		w.Header().Set("Retry-After", "1")
		writeError(w, http.StatusServiceUnavailable, "unavailable", "Too many token checks at once. Try again in a moment.")
		return caller{}, false
	case <-r.Context().Done():
		return caller{}, false
	}
	role, ok := a.opts.Tokens.Verify(name, token)
	<-a.slots
	if !ok {
		a.authFailed(w, r, name)
		return caller{}, false
	}
	a.cache.store(name, token, role)
	return caller{name: name, role: role}, true
}

// authFailed writes the event and the 401. The name goes into the event only
// if it is in the tokens file: a token pasted into the name part must not
// reach the event log.
func (a *API) authFailed(w http.ResponseWriter, r *http.Request, name string) {
	actor := ""
	if name != "" && a.opts.Tokens.Known(name) {
		actor = name
	}
	if err := a.event(r.Context(), KindAuthFailed, store.Warn, actor, ""); err != nil {
		a.opts.Logger.Error("api: writing the failed-auth event failed", "err", err)
	}
	w.Header().Set("WWW-Authenticate", `Bearer realm="postwerk"`)
	writeError(w, http.StatusUnauthorized, "unauthorized", "A valid bearer token is required.")
}

// parseBearer splits "Bearer <name>.<token>". The token is base64url and has
// no dot, so the last dot separates them; names may contain dots.
func parseBearer(h string) (name, token string, ok bool) {
	if len(h) > maxAuthorization {
		return "", "", false
	}
	scheme, cred, ok := strings.Cut(h, " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") {
		return "", "", false
	}
	cred = strings.TrimSpace(cred)
	i := strings.LastIndexByte(cred, '.')
	if i <= 0 || i == len(cred)-1 {
		return "", "", false
	}
	name, token = cred[:i], cred[i+1:]
	if !auth.ValidName(name) {
		return "", "", false
	}
	return name, token, true
}

// BearerToken returns the value a client sends after "Bearer ".
func BearerToken(name, token string) string { return name + "." + token }

// event writes one of the API's events.
func (a *API) event(ctx context.Context, kind string, level store.Level, actor, text string) error {
	return a.opts.Events.Append(ctx, store.Event{Time: a.opts.Now(), Kind: kind, Level: level, Actor: actor, Text: text})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
}

func writeError(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, apiError{Error: code, Message: msg})
}

func tooMany(w http.ResponseWriter, wait time.Duration) {
	secs := max(int((wait+time.Second-1)/time.Second), 1)
	w.Header().Set("Retry-After", strconv.Itoa(secs))
	writeError(w, http.StatusTooManyRequests, "rate_limited", "Too many requests. Retry after the time in Retry-After.")
}

// rateLimit counts requests per key in fixed windows.
type rateLimit struct {
	mu     sync.Mutex
	now    func() time.Time
	limit  int
	window time.Duration
	counts map[string]*windowCount
}

type windowCount struct {
	start time.Time
	n     int
}

func newRateLimit(now func() time.Time, limit int, window time.Duration) *rateLimit {
	return &rateLimit{now: now, limit: limit, window: window, counts: make(map[string]*windowCount)}
}

// allow counts one request for key. If it is over the limit, it returns
// false and the time until the window ends.
func (l *rateLimit) allow(key string) (bool, time.Duration) {
	now := l.now()
	l.mu.Lock()
	defer l.mu.Unlock()
	c := l.counts[key]
	if c == nil || now.Sub(c.start) >= l.window {
		c = &windowCount{start: now}
		l.counts[key] = c
	}
	if c.n >= l.limit {
		return false, c.start.Add(l.window).Sub(now)
	}
	c.n++
	return true, 0
}

// goodTokens remembers an HMAC of each token that passed a full check, under
// a random key held in memory, so a client that calls often costs one HMAC
// per request instead of PBKDF2. Only the right token takes that path; a
// wrong one always pays the full check. The tokens file is read once at
// start, so an entry never goes stale.
type goodTokens struct {
	key  []byte
	mu   sync.Mutex
	macs map[string]goodToken
}

type goodToken struct {
	mac  []byte
	role auth.Role
}

func newGoodTokens() *goodTokens {
	g := &goodTokens{key: make([]byte, 32), macs: make(map[string]goodToken)}
	_, _ = rand.Read(g.key)
	return g
}

func (g *goodTokens) mac(token string) []byte {
	m := hmac.New(sha256.New, g.key)
	m.Write([]byte(token))
	return m.Sum(nil)
}

func (g *goodTokens) lookup(name, token string) (auth.Role, bool) {
	g.mu.Lock()
	t, ok := g.macs[name]
	g.mu.Unlock()
	if !ok || !hmac.Equal(t.mac, g.mac(token)) {
		return "", false
	}
	return t.role, true
}

func (g *goodTokens) store(name, token string, role auth.Role) {
	m := g.mac(token)
	g.mu.Lock()
	g.macs[name] = goodToken{mac: m, role: role}
	g.mu.Unlock()
}
