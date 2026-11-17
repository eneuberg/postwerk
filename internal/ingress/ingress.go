// Package ingress is Postwerk's SMTP server (I1–I8).
//
// Every transaction needs AUTH PLAIN with a known sender (internal/senders
// checks the login; ingress depends on its Authenticator only). A mail is answered
// with 250 only after it, its recipients, its deliveries and their events are
// committed; the message is sealed (encrypted at rest) before it is stored.
// The client only ever sees fixed reply texts chosen here; internal errors
// are logged, never sent.
package ingress

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/emersion/go-sasl"
	"github.com/emersion/go-smtp"

	"github.com/eneuberg/postwerk/internal/config"
	"github.com/eneuberg/postwerk/internal/fold"
	"github.com/eneuberg/postwerk/internal/id"
	"github.com/eneuberg/postwerk/internal/message"
	"github.com/eneuberg/postwerk/internal/seal"
	"github.com/eneuberg/postwerk/internal/senders"
	"github.com/eneuberg/postwerk/internal/senders/limit"
	"github.com/eneuberg/postwerk/internal/store"
)

// Fixed limits.
const (
	maxAuthFailures = 3
	maxAddressLen   = 320
	maxLineLength   = 2000 // twice RFC 5321's 1000, like the SMTP library's default
	readTimeout     = 2 * time.Minute
	writeTimeout    = time.Minute
	// rejectTimeout bounds writing the 421 to a connection over the limit.
	rejectTimeout = 2 * time.Second
	// authTimeout: a connection that has not logged in by then is closed.
	authTimeout = 30 * time.Second
)

// DefaultMaxConnections is the default limit of concurrent connections.
const DefaultMaxConnections = 100

// Events of refused logins and commands (D51). They are folded: at most one
// event per kind, sender and reason in each RefusalWindow, with the count as
// text. They never carry a password, an address or message content.
const (
	// KindAuthFailed: failed AUTH attempts; Sender is the login name only
	// if it is in the senders file. Text "failures=<n>".
	KindAuthFailed = "ingress.auth_failed"
	// KindRejected: commands of a logged-in sender refused for a reason
	// in its text ("reason=too_large rejected=<n>").
	KindRejected = "ingress.rejected"
	// KindRateLimited: commands of a logged-in sender refused by its
	// policy's rate limit (D64), text "limit=<max_per_minute|max_rcpt_per_hour> limited=<n>".
	KindRateLimited = "ingress.rate_limited"
)

// RefusalWindow is how long refusals of one kind, sender and reason are
// counted before their event is written.
const RefusalWindow = time.Minute

// Reasons of an ingress.rejected event.
const (
	ReasonTooLarge          = "too_large"           // 552 5.3.4
	ReasonBadAddress        = "bad_address"         // 553 5.1.3
	ReasonNeedsSMTPUTF8     = "needs_smtputf8"      // 553 5.6.7
	ReasonTooManyRecipients = "too_many_recipients" // 452 4.5.3
	ReasonLineTooLong       = "line_too_long"       // 500 5.5.2
	ReasonFromNotAllowed    = "from_not_allowed"    // 550 5.7.1 (D63)
)

// Limits of an ingress.rate_limited event.
const (
	LimitPerMinute   = "max_per_minute"
	LimitRcptPerHour = "max_rcpt_per_hour"
)

// replyTooManyConns is written to a connection over the limit, then it is
// closed. It is sent before the greeting, as RFC 5321 section 3.1 allows.
const replyTooManyConns = "421 4.7.0 Too many connections, try again later\r\n"

// Fixed replies. Nothing else is ever sent to the client.
var (
	errAuthRequired = &smtp.SMTPError{Code: 530, EnhancedCode: smtp.EnhancedCode{5, 7, 0}, Message: "Authentication required"}
	errAuthFailed   = &smtp.SMTPError{Code: 535, EnhancedCode: smtp.EnhancedCode{5, 7, 8}, Message: "Authentication failed"}
	errAuthTooMany  = &smtp.SMTPError{Code: 421, EnhancedCode: smtp.EnhancedCode{4, 7, 0}, Message: "Too many failed authentication attempts, closing connection"}
	errAuthBusy     = &smtp.SMTPError{Code: 454, EnhancedCode: smtp.EnhancedCode{4, 7, 0}, Message: "Temporary authentication failure, try again later"}
	errBadAddress   = &smtp.SMTPError{Code: 553, EnhancedCode: smtp.EnhancedCode{5, 1, 3}, Message: "Invalid address"}
	errNeedsUTF8    = &smtp.SMTPError{Code: 553, EnhancedCode: smtp.EnhancedCode{5, 6, 7}, Message: "Non-ASCII address requires SMTPUTF8"}
	errTooBig       = &smtp.SMTPError{Code: 552, EnhancedCode: smtp.EnhancedCode{5, 3, 4}, Message: "Message too big"}
	errTooManyRcpt  = &smtp.SMTPError{Code: 452, EnhancedCode: smtp.EnhancedCode{4, 5, 3}, Message: "Too many recipients"}
	errBadParams    = &smtp.SMTPError{Code: 501, EnhancedCode: smtp.EnhancedCode{5, 5, 4}, Message: "Invalid parameters"}
	errBadSequence  = &smtp.SMTPError{Code: 503, EnhancedCode: smtp.EnhancedCode{5, 5, 1}, Message: "Bad sequence of commands"}
	errStorage      = &smtp.SMTPError{Code: 451, EnhancedCode: smtp.EnhancedCode{4, 3, 0}, Message: "Temporary local problem, try again later"}
	errNoMemory     = &smtp.SMTPError{Code: 451, EnhancedCode: smtp.EnhancedCode{4, 3, 1}, Message: "Insufficient system resources, try again later"}
	errReadFailed   = &smtp.SMTPError{Code: 451, EnhancedCode: smtp.EnhancedCode{4, 4, 2}, Message: "Error while reading the message"}
	errLineTooLong  = &smtp.SMTPError{Code: 500, EnhancedCode: smtp.EnhancedCode{5, 5, 2}, Message: "Line too long"}
	errNoStorage    = &smtp.SMTPError{Code: 452, EnhancedCode: smtp.EnhancedCode{4, 3, 1}, Message: "Insufficient system storage, try again later"}
	errFromDenied   = &smtp.SMTPError{Code: 550, EnhancedCode: smtp.EnhancedCode{5, 7, 1}, Message: "Sender address not allowed for this login"}
	errRateLimited  = &smtp.SMTPError{Code: 451, EnhancedCode: smtp.EnhancedCode{4, 7, 1}, Message: "Rate limit exceeded, try again later"}
)

// reasons maps the fixed replies counted as ingress.rejected to their reason.
var reasons = map[*smtp.SMTPError]string{
	errTooBig:      ReasonTooLarge,
	errBadAddress:  ReasonBadAddress,
	errNeedsUTF8:   ReasonNeedsSMTPUTF8,
	errTooManyRcpt: ReasonTooManyRecipients,
	errLineTooLong: ReasonLineTooLong,
	errFromDenied:  ReasonFromNotAllowed,
}

// Authenticator checks a login (internal/senders.Authenticator).
type Authenticator interface {
	// Authenticate returns senders.ErrBusy when no check slot came free
	// in time, any other error for a failed login.
	Authenticate(name, password string) (senders.Principal, error)
	// Refresh returns the principal with its current policy, or false
	// when its credential was revoked, expired or removed.
	Refresh(p senders.Principal) (senders.Principal, bool)
	// Known reports whether name is a sender, for naming it in an event.
	Known(name string) bool
}

// RateLimiter counts what a sender may do (internal/senders/limit).
type RateLimiter interface {
	Allow(key string, r limit.Rate) bool
}

// Options configure a Server.
type Options struct {
	Store *store.Store
	// Sealer seals every message before it is stored. Required.
	Sealer *seal.Sealer
	// Auth checks logins. Required.
	Auth Authenticator
	// Limiter enforces each sender's rate limits; nil: no limits.
	Limiter RateLimiter
	Targets []config.Target
	// Hostname is the name in the greeting.
	Hostname       string
	TLSCertificate *tls.Certificate // nil: no STARTTLS (unless GetCertificate is set)
	// GetCertificate, if set, supplies the certificate for each STARTTLS
	// handshake instead of TLSCertificate (tlsreload.Reloader, I16).
	GetCertificate func(*tls.ClientHelloInfo) (*tls.Certificate, error)
	AllowInsecure  bool // AUTH without TLS
	// MaxMessageBytes is the largest message accepted, offered as SIZE and
	// counted as a target receives it (message.WireSize). The caller
	// leaves room for the header fields delivery adds (I17).
	MaxMessageBytes int64
	MaxRecipients   int
	// MaxConnections limits concurrent connections; 0 means
	// DefaultMaxConnections.
	MaxConnections int
	// MaxInflightBytes is the memory budget for messages being received:
	// each DATA reserves MaxMessageBytes (or the declared SIZE, if smaller)
	// before it reads. 0 means no budget beyond MaxConnections.
	MaxInflightBytes int64
	// Wake is called after each accepted mail; it must not block.
	Wake func()
	// Space, if set, is asked before a mail is taken (MAIL) and before it
	// is stored (with its size): false means the data volume is below its
	// reserve, and the client gets 452 4.3.1 (D50).
	Space  interface{ Allow(size int64) bool }
	Logger *slog.Logger
	Now    func() time.Time

	// Only tests set these; zero means the fixed values above.
	authTimeout   time.Duration
	refusalWindow time.Duration
}

// Server is the SMTP ingress.
type Server struct {
	opts   Options
	srv    *smtp.Server
	budget budget
	// refused folds refusals into events (D51).
	refused *fold.Counter[refusal]

	mu    sync.Mutex
	conns map[*trackedConn]struct{}
}

// New creates a Server.
func New(opts Options) *Server {
	if opts.Sealer == nil {
		panic("ingress: Options.Sealer is required")
	}
	if opts.Auth == nil {
		panic("ingress: Options.Auth is required")
	}
	if opts.MaxConnections <= 0 {
		opts.MaxConnections = DefaultMaxConnections
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.Logger == nil {
		opts.Logger = slog.New(slog.DiscardHandler)
	}
	if opts.Wake == nil {
		opts.Wake = func() {}
	}
	if opts.authTimeout <= 0 {
		opts.authTimeout = authTimeout
	}
	if opts.refusalWindow <= 0 {
		opts.refusalWindow = RefusalWindow
	}
	s := &Server{opts: opts, conns: make(map[*trackedConn]struct{}), budget: budget{max: opts.MaxInflightBytes}}
	s.refused = fold.New(opts.refusalWindow, s.writeRefusal)
	srv := smtp.NewServer(smtp.BackendFunc(s.newSession))
	srv.Domain = opts.Hostname
	srv.MaxMessageBytes = opts.MaxMessageBytes
	// The recipient limit is checked in Rcpt, with the same reply the
	// library would send, so that the refusal is counted (D51).
	srv.MaxLineLength = maxLineLength
	srv.ReadTimeout = readTimeout
	srv.WriteTimeout = writeTimeout
	srv.AllowInsecureAuth = opts.AllowInsecure
	// The library always offers 8BITMIME. SMTPUTF8 is offered too, and both
	// are passed on to every target that offers them (store.Params). DSN
	// and REQUIRETLS are not offered: Postwerk could not keep their promise
	// towards every target.
	srv.EnableSMTPUTF8 = true
	srv.ErrorLog = errorLog{opts.Logger}
	switch {
	case opts.GetCertificate != nil:
		srv.TLSConfig = &tls.Config{GetCertificate: opts.GetCertificate, MinVersion: tls.VersionTLS12}
	case opts.TLSCertificate != nil:
		srv.TLSConfig = &tls.Config{
			Certificates: []tls.Certificate{*opts.TLSCertificate},
			MinVersion:   tls.VersionTLS12,
		}
	}
	s.srv = srv
	return s
}

// Serve accepts connections on l until Shutdown.
func (s *Server) Serve(l net.Listener) error {
	err := s.srv.Serve(trackingListener{Listener: l, srv: s})
	if errors.Is(err, smtp.ErrServerClosed) {
		return nil
	}
	return err
}

// Shutdown stops accepting connections and waits for open sessions to end.
// When ctx ends first, the remaining connections are closed. A session that
// is storing a mail finishes its commit first (the store waits for it), so a
// sender never misses a 250 for a stored mail except when the connection
// closes in the moment between commit and reply. The refusals counted so
// far are written last.
func (s *Server) Shutdown(ctx context.Context) error {
	err := s.srv.Shutdown(ctx)
	if err != nil && ctx.Err() != nil {
		s.mu.Lock()
		for c := range s.conns {
			_ = c.Conn.Close()
		}
		s.mu.Unlock()
	}
	s.refused.Close()
	if errors.Is(err, smtp.ErrServerClosed) {
		return nil
	}
	return err
}

// errorLog sends the SMTP library's own messages to slog. They describe
// connection problems and carry no message content.
type errorLog struct{ log *slog.Logger }

func (e errorLog) Printf(format string, v ...any) {
	e.log.Warn("smtp server", "detail", strings.TrimSpace(fmt.Sprintf(format, v...)))
}
func (e errorLog) Println(v ...any) {
	e.log.Warn("smtp server", "detail", strings.TrimSpace(fmt.Sprintln(v...)))
}

// trackingListener wraps every connection so a session can close it after the
// reply to the third failed AUTH, count failures across STARTTLS, and so
// Shutdown can close connections that outlive its deadline. It also enforces
// the connection limit: a connection over it gets a fixed 421 and is closed.
type trackingListener struct {
	net.Listener
	srv *Server
}

func (l trackingListener) Accept() (net.Conn, error) {
	for {
		c, err := l.Listener.Accept()
		if err != nil {
			return nil, err
		}
		tc := &trackedConn{Conn: c, srv: l.srv, authBy: time.Now().Add(l.srv.opts.authTimeout)}
		l.srv.mu.Lock()
		full := len(l.srv.conns) >= l.srv.opts.MaxConnections
		if !full {
			l.srv.conns[tc] = struct{}{}
		}
		l.srv.mu.Unlock()
		if !full {
			return tc, nil
		}
		l.srv.opts.Logger.Warn("smtp connection refused, too many connections",
			"remote", c.RemoteAddr().String(), "limit", l.srv.opts.MaxConnections)
		// A fresh socket takes a short write without blocking; the deadline
		// covers the odd case where it does not.
		_ = c.SetWriteDeadline(time.Now().Add(rejectTimeout))
		_, _ = io.WriteString(c, replyTooManyConns)
		_ = c.Close()
	}
}

type trackedConn struct {
	net.Conn
	srv          *Server
	authBy       time.Time   // the login must be done by then
	authed       atomic.Bool // a login succeeded
	mu           sync.Mutex
	authFailures int
	closeAfter   bool // close after the next write
}

// SetReadDeadline caps every read before the login at authBy. When it
// passes, the read times out and the SMTP library answers with its fixed
// "421 4.4.2 Idle timeout" and closes the connection. TLS sets its
// deadlines through here as well, so a stalled STARTTLS handshake is cut
// too.
func (c *trackedConn) SetReadDeadline(t time.Time) error {
	return c.Conn.SetReadDeadline(c.capped(t))
}

func (c *trackedConn) SetDeadline(t time.Time) error {
	if err := c.Conn.SetWriteDeadline(t); err != nil {
		return err
	}
	return c.Conn.SetReadDeadline(c.capped(t))
}

func (c *trackedConn) capped(t time.Time) time.Time {
	if !c.authed.Load() && (t.IsZero() || t.After(c.authBy)) {
		return c.authBy
	}
	return t
}

func (c *trackedConn) Close() error {
	c.srv.mu.Lock()
	delete(c.srv.conns, c)
	c.srv.mu.Unlock()
	return c.Conn.Close()
}

func (c *trackedConn) Write(b []byte) (int, error) {
	n, err := c.Conn.Write(b)
	c.mu.Lock()
	closeNow := c.closeAfter
	c.mu.Unlock()
	if closeNow {
		_ = c.Close()
	}
	return n, err
}

// authFailed counts a failure and reports whether the limit is reached; then
// the connection closes after the next reply.
func (c *trackedConn) authFailed() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.authFailures++
	if c.authFailures >= maxAuthFailures {
		c.closeAfter = true
		return true
	}
	return false
}

func tracked(c *smtp.Conn) *trackedConn {
	nc := c.Conn()
	if tc, ok := nc.(*tls.Conn); ok {
		nc = tc.NetConn()
	}
	t, _ := nc.(*trackedConn)
	return t
}

func (s *Server) newSession(c *smtp.Conn) (smtp.Session, error) {
	remote := ""
	if a := c.Conn().RemoteAddr(); a != nil {
		remote = a.String()
	}
	return &session{srv: s, conn: tracked(c), smtpConn: c, remote: remote}, nil
}

// session is one SMTP session. With BDAT, Data runs on another goroutine than
// the other methods, so the fields are guarded by mu.
type session struct {
	srv      *Server
	conn     *trackedConn
	smtpConn *smtp.Conn
	remote   string

	mu        sync.Mutex
	sender    string // set after a successful AUTH
	principal senders.Principal
	// audit is where the mail comes from, taken at MAIL (on the command
	// goroutine, so it does not race with the connection's own state).
	audit   store.MailAudit
	from    string
	hasFrom bool
	params  store.Params
	to      []string
}

var _ smtp.AuthSession = (*session)(nil)

func (s *session) AuthMechanisms() []string { return []string{sasl.Plain} }

func (s *session) Auth(mech string) (sasl.Server, error) {
	if mech != sasl.Plain {
		return nil, smtp.ErrAuthUnknownMechanism
	}
	return &plainServer{inner: sasl.NewPlainServer(s.checkPlain), session: s}, nil
}

func (s *session) checkPlain(identity, username, password string) error {
	name := username
	if identity != "" && identity != username {
		name = "" // same cost as any other failed login, never a match
	}
	p, err := s.srv.opts.Auth.Authenticate(name, password)
	if errors.Is(err, senders.ErrBusy) {
		return errAuthBusy
	}
	if err != nil || name == "" {
		return s.authFailure(username)
	}
	s.mu.Lock()
	s.sender, s.principal = p.Sender, p
	s.mu.Unlock()
	if s.conn != nil {
		s.conn.authed.Store(true)
	}
	return nil
}

// authFailure counts a failed login. The name goes into the event only if
// it is in the senders file: a password typed into the name field must not
// reach the event log, and the keys stay bounded by the file.
func (s *session) authFailure(name string) error {
	if name != "" && !s.srv.opts.Auth.Known(name) {
		name = ""
	}
	s.srv.refused.Add(refusal{kind: KindAuthFailed, sender: name})
	s.srv.opts.Logger.Warn("smtp auth failed", "remote", s.remote)
	if s.conn != nil && s.conn.authFailed() {
		return errAuthTooMany
	}
	return errAuthFailed
}

// plainServer turns every error of the PLAIN exchange into a fixed reply.
type plainServer struct {
	inner   sasl.Server
	session *session
}

func (p *plainServer) Next(response []byte) ([]byte, bool, error) {
	challenge, done, err := p.inner.Next(response)
	if err != nil {
		var se *smtp.SMTPError
		if errors.As(err, &se) {
			return nil, false, se
		}
		return nil, false, p.session.authFailure("") // malformed response
	}
	return challenge, done, nil
}

func (s *session) Mail(from string, opts *smtp.MailOptions) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.sender == "" {
		return errAuthRequired
	}
	// The credential may have been revoked since the login, and the
	// policy changed (D62).
	p, ok := s.srv.opts.Auth.Refresh(s.principal)
	if !ok {
		s.srv.opts.Logger.Warn("smtp session ended, the credential was revoked or removed", "sender", s.sender,
			"remote", s.remote)
		s.sender, s.principal = "", senders.Principal{}
		return errAuthRequired
	}
	s.principal = p
	if !validAddress(from) {
		return s.reject(errBadAddress)
	}
	if !s.srv.spaceFor(0) {
		return errNoStorage
	}
	var params store.Params
	if opts != nil {
		// The library answers a SIZE= over srv.MaxMessageBytes itself
		// (552 5.3.4) before Mail runs, so this is not reached and the
		// refusal is not counted (D51). It stays in case that changes.
		if opts.Size > s.srv.opts.MaxMessageBytes {
			return s.reject(errTooBig)
		}
		params = store.Params{Body: string(opts.Body), SMTPUTF8: opts.UTF8, DeclaredSize: opts.Size}
		// The library refuses other BODY values; this keeps the store's
		// CHECK constraint safe if that ever changes.
		if params.Body != "" && params.Body != store.Body7Bit && params.Body != store.Body8BitMIME {
			return errBadParams
		}
	}
	if !params.SMTPUTF8 && !isASCII(from) {
		return s.reject(errNeedsUTF8)
	}
	if !p.Policy.AllowsFrom(from) {
		return s.reject(errFromDenied)
	}
	if !s.allow("msg", p.Policy.MessageRate()) {
		return s.limited(LimitPerMinute)
	}
	s.from, s.hasFrom, s.params, s.to = from, true, params, nil
	s.audit = s.connAudit()
	return nil
}

// allow takes one from the sender's limit of kind; without a Limiter
// everything is allowed. s.mu must be held.
func (s *session) allow(kind string, r limit.Rate) bool {
	if s.srv.opts.Limiter == nil || r.Unlimited() {
		return true
	}
	return s.srv.opts.Limiter.Allow(kind+":"+s.sender, r)
}

// limited counts a command refused by a rate limit (D64). s.mu must be held.
func (s *session) limited(which string) error {
	s.srv.refused.Add(refusal{kind: KindRateLimited, sender: s.sender, reason: which})
	return errRateLimited
}

// connAudit describes the connection and the login for the mail record
// (D65). s.mu must be held.
func (s *session) connAudit() store.MailAudit {
	a := store.MailAudit{KeyID: s.principal.KeyID, KeySource: string(s.principal.Source), AuthMech: sasl.Plain}
	if host, _, err := net.SplitHostPort(s.remote); err == nil {
		a.ClientIP = host
	}
	if s.smtpConn != nil {
		a.EHLO = s.smtpConn.Hostname()
		if st, ok := s.smtpConn.TLSConnectionState(); ok {
			a.TLSVersion, a.TLSCipher = tls.VersionName(st.Version), tls.CipherSuiteName(st.CipherSuite)
		}
	}
	return a
}

func (s *session) Rcpt(to string, _ *smtp.RcptOptions) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.sender == "" {
		return errAuthRequired
	}
	if !s.hasFrom {
		return errBadSequence
	}
	if to == "" || !validAddress(to) {
		return s.reject(errBadAddress)
	}
	if !s.params.SMTPUTF8 && !isASCII(to) {
		return s.reject(errNeedsUTF8)
	}
	if len(s.to) >= s.srv.opts.MaxRecipients {
		return s.reject(errTooManyRcpt)
	}
	if !s.srv.spaceFor(0) {
		return errNoStorage
	}
	if !s.allow("rcpt", s.principal.Policy.RecipientRate()) {
		return s.limited(LimitRcptPerHour)
	}
	s.to = append(s.to, to)
	return nil
}

func (s *session) Data(r io.Reader) error {
	s.mu.Lock()
	sender, from, params, to, audit := s.sender, s.from, s.params, slices.Clone(s.to), s.audit
	ok := s.sender != "" && s.hasFrom && len(s.to) > 0
	s.mu.Unlock()
	if sender == "" {
		return errAuthRequired
	}
	if !ok {
		return errBadSequence
	}

	limit := s.srv.opts.MaxMessageBytes
	// Reserve memory before reading. The SMTP library has already sent
	// 354; returning now makes it read and discard the message without
	// buffering it, and then send the 451.
	reserved := limit
	if params.DeclaredSize > 0 && params.DeclaredSize < limit {
		reserved = params.DeclaredSize
	}
	if !s.srv.budget.take(reserved) {
		s.srv.opts.Logger.Warn("message refused, memory budget for messages in flight exhausted",
			"sender", sender, "budget", s.srv.budget.max)
		return errNoMemory
	}
	defer func() { s.srv.budget.give(reserved) }()

	raw, err := io.ReadAll(io.LimitReader(r, reserved+1))
	if err == nil && int64(len(raw)) > reserved && reserved < limit {
		// Larger than declared: grow the reservation to the full limit.
		if !s.srv.budget.take(limit - reserved) {
			s.srv.opts.Logger.Warn("message refused, larger than declared and memory budget exhausted",
				"sender", sender, "budget", s.srv.budget.max)
			return errNoMemory
		}
		reserved = limit
		var rest []byte
		rest, err = io.ReadAll(io.LimitReader(r, limit+1-int64(len(raw))))
		raw = append(raw, rest...)
	}
	if err != nil {
		if errors.Is(err, smtp.ErrDataTooLarge) {
			return s.rejectAs(sender, errTooBig)
		}
		if errors.Is(err, smtp.ErrTooLongLine) {
			return s.rejectAs(sender, errLineTooLong)
		}
		s.srv.opts.Logger.Warn("reading message failed", "remote", s.remote, "sender", sender)
		return errReadFailed
	}
	// Counted as a target receives it: an LF without CR becomes CRLF on
	// the way out (I17).
	if int64(message.WireSize(raw)) > limit {
		return s.rejectAs(sender, errTooBig)
	}
	// The volume may have filled while the message was read; the stored
	// row is about as large as the message.
	if !s.srv.spaceFor(int64(len(raw))) {
		return errNoStorage
	}

	mailID, err := s.srv.accept(sender, from, to, params, audit, raw)
	if err != nil {
		s.srv.opts.Logger.Error("storing mail failed, replied 451", "sender", sender, "err", err)
		return errStorage
	}
	return &smtp.SMTPError{Code: 250, EnhancedCode: smtp.EnhancedCode{2, 0, 0}, Message: "queued as " + mailID}
}

// refusal is the key under which refusals are folded into one event.
type refusal struct{ kind, sender, reason string }

// reject counts a refused command of the logged-in sender and returns its
// fixed reply. s.mu must be held.
func (s *session) reject(e *smtp.SMTPError) error { return s.rejectAs(s.sender, e) }

// rejectAs counts a refused command of sender and returns its fixed reply.
func (s *session) rejectAs(sender string, e *smtp.SMTPError) error {
	s.srv.refused.Add(refusal{kind: KindRejected, sender: sender, reason: reasons[e]})
	return e
}

// writeRefusal writes one folded refusal event. It runs on a timer or at
// shutdown, outside any session, so it has its own deadline.
func (s *Server) writeRefusal(r refusal, n int) {
	text := "failures=" + strconv.Itoa(n)
	switch r.kind {
	case KindRejected:
		text = "reason=" + r.reason + " rejected=" + strconv.Itoa(n)
	case KindRateLimited:
		text = "limit=" + r.reason + " limited=" + strconv.Itoa(n)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err := s.opts.Store.Append(ctx, store.Event{
		Time: s.opts.Now(), Kind: r.kind, Level: store.Warn, Sender: r.sender, Text: text,
	})
	if err != nil {
		s.opts.Logger.Error("writing a refusal event failed", "kind", r.kind, "count", n, "err", err)
	}
}

// spaceFor reports whether the data volume has room for a mail of size
// bytes above its reserve (D50). Without Space it always has. A refusal is
// not logged one by one: the disk.low event and its log line say it once.
func (s *Server) spaceFor(size int64) bool {
	return s.opts.Space == nil || s.opts.Space.Allow(size)
}

// budget counts the bytes reserved for messages being received.
type budget struct {
	mu   sync.Mutex
	used int64
	max  int64 // 0: unlimited
}

// take reserves n bytes and reports whether that fit.
func (b *budget) take(n int64) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.max > 0 && b.used+n > b.max {
		return false
	}
	b.used += n
	return true
}

func (b *budget) give(n int64) {
	b.mu.Lock()
	b.used -= n
	b.mu.Unlock()
}

func (b *budget) inUse() int64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.used
}

func (s *session) Reset() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.from, s.hasFrom, s.params, s.to, s.audit = "", false, store.Params{}, nil, store.MailAudit{}
}

func (s *session) Logout() error { return nil }

// accept processes and stores one mail. It returns the new id.
func (s *Server) accept(sender, from string, to []string, params store.Params, audit store.MailAudit, raw []byte) (string, error) {
	now := s.opts.Now()
	mailID := id.NewAt(now)
	msg := message.Process(raw)
	sealed, err := s.opts.Sealer.Seal(mailID, msg.Message)
	if err != nil {
		return "", err
	}

	m := store.NewMail{
		ID: mailID, ReceivedAt: now, Sender: sender, EnvelopeFrom: from, Recipients: to,
		MessageID: msg.MessageID, Ref: msg.Ref, Raw: sealed, Size: len(msg.Message), Params: params, Audit: audit,
	}
	known := make(map[string]bool)
	for _, t := range s.opts.Targets {
		known[t.Name] = true
		d := store.NewDelivery{Target: t.Name, Status: store.Queued}
		switch {
		case slices.Contains(msg.Skip, t.Name):
			d.Status, d.Kind, d.Level, d.Text = store.Skipped, store.KindDeliverySkipped, store.Info,
				"skipped by the sender (X-Postwerk-Skip)"
		case !t.Allowed(to):
			d.Status, d.Kind, d.Level, d.Text = store.Skipped, store.KindDeliveryNotAllowed, store.Warn,
				"a recipient is not in the target's allow-list"
		}
		m.Deliveries = append(m.Deliveries, d)
	}

	// The store needs no context from the session: a commit that has
	// started must finish, so the sender's 250 matches what is stored.
	if err := s.opts.Store.Accept(context.Background(), m); err != nil {
		return "", err
	}
	s.opts.Wake()

	for _, name := range msg.Skip {
		if !known[name] {
			if !validTargetName(name) {
				name = "<invalid>"
			}
			s.opts.Logger.Warn("X-Postwerk-Skip names an unknown target", "mail_id", mailID, "sender", sender, "name", name)
		}
	}
	return mailID, nil
}

// validAddress checks an envelope address: at most 320 bytes, at most one
// "@", valid UTF-8, no control characters (C0, DEL, C1, Unicode line and
// paragraph separators), and none of the characters that would need a
// quoted local part (space, '"', backslash, '<', '>', '(', ')', ',', ';'). The
// SMTP library removes the quotes of a quoted local part, so such an address
// could not be passed on unchanged: written into the RCPT or MAIL command
// towards a target, a '>' would end the path early and the rest would be read
// as parameters, and a recipient allow-list would check a different address
// than the target sees. Non-ASCII characters (RFC 6531) are allowed here;
// Mail and Rcpt require SMTPUTF8 for them. The empty (null) sender is
// allowed here; Rcpt rejects it separately.
func validAddress(a string) bool {
	if len(a) > maxAddressLen || strings.Count(a, "@") > 1 || !utf8.ValidString(a) {
		return false
	}
	for _, r := range a {
		if r < 0x20 || r == 0x7f || (r >= 0x80 && r < 0xa0) || r == 0x2028 || r == 0x2029 ||
			r < utf8.RuneSelf && strings.IndexByte(" \"\\<>(),;", byte(r)) >= 0 {
			return false
		}
	}
	return true
}

// isASCII reports whether s has only 7-bit characters.
func isASCII(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] >= utf8.RuneSelf {
			return false
		}
	}
	return true
}

func validTargetName(n string) bool {
	if n == "" || len(n) > 32 {
		return false
	}
	for i := 0; i < len(n); i++ {
		c := n[i]
		if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '_') {
			return false
		}
	}
	return true
}
