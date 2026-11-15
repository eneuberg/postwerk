// Package ingress is Postwerk's SMTP server (I1–I8).
//
// Every transaction needs AUTH PLAIN with a known sender. A mail is answered
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
	"strings"
	"sync"
	"time"

	"github.com/emersion/go-sasl"
	"github.com/emersion/go-smtp"

	"github.com/eneuberg/postwerk/internal/auth"
	"github.com/eneuberg/postwerk/internal/config"
	"github.com/eneuberg/postwerk/internal/id"
	"github.com/eneuberg/postwerk/internal/message"
	"github.com/eneuberg/postwerk/internal/seal"
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
)

// DefaultMaxConnections is the default limit of concurrent connections.
const DefaultMaxConnections = 100

// replyTooManyConns is written to a connection over the limit, then it is
// closed. It is sent before the greeting, as RFC 5321 section 3.1 allows.
const replyTooManyConns = "421 4.7.0 Too many connections, try again later\r\n"

// Fixed replies. Nothing else is ever sent to the client.
var (
	errAuthRequired = &smtp.SMTPError{Code: 530, EnhancedCode: smtp.EnhancedCode{5, 7, 0}, Message: "Authentication required"}
	errAuthFailed   = &smtp.SMTPError{Code: 535, EnhancedCode: smtp.EnhancedCode{5, 7, 8}, Message: "Authentication failed"}
	errAuthTooMany  = &smtp.SMTPError{Code: 421, EnhancedCode: smtp.EnhancedCode{4, 7, 0}, Message: "Too many failed authentication attempts, closing connection"}
	errBadAddress   = &smtp.SMTPError{Code: 553, EnhancedCode: smtp.EnhancedCode{5, 1, 3}, Message: "Invalid address"}
	errTooBig       = &smtp.SMTPError{Code: 552, EnhancedCode: smtp.EnhancedCode{5, 3, 4}, Message: "Message too big"}
	errTooManyRcpt  = &smtp.SMTPError{Code: 452, EnhancedCode: smtp.EnhancedCode{4, 5, 3}, Message: "Too many recipients"}
	errBadSequence  = &smtp.SMTPError{Code: 503, EnhancedCode: smtp.EnhancedCode{5, 5, 1}, Message: "Bad sequence of commands"}
	errStorage      = &smtp.SMTPError{Code: 451, EnhancedCode: smtp.EnhancedCode{4, 3, 0}, Message: "Temporary local problem, try again later"}
	errReadFailed   = &smtp.SMTPError{Code: 451, EnhancedCode: smtp.EnhancedCode{4, 4, 2}, Message: "Error while reading the message"}
	errLineTooLong  = &smtp.SMTPError{Code: 500, EnhancedCode: smtp.EnhancedCode{5, 5, 2}, Message: "Line too long"}
)

// Options configure a Server.
type Options struct {
	Store *store.Store
	// Sealer seals every message before it is stored. Required.
	Sealer  *seal.Sealer
	Senders *auth.Senders
	Targets []config.Target
	// Hostname is the name in the greeting.
	Hostname        string
	TLSCertificate  *tls.Certificate // nil: no STARTTLS
	AllowInsecure   bool             // AUTH without TLS
	MaxMessageBytes int64
	MaxRecipients   int
	// MaxConnections limits concurrent connections; 0 means
	// DefaultMaxConnections.
	MaxConnections int
	// Wake is called after each accepted mail; it must not block.
	Wake   func()
	Logger *slog.Logger
	Now    func() time.Time
}

// Server is the SMTP ingress.
type Server struct {
	opts Options
	srv  *smtp.Server

	mu    sync.Mutex
	conns map[*trackedConn]struct{}
}

// New creates a Server.
func New(opts Options) *Server {
	if opts.Sealer == nil {
		panic("ingress: Options.Sealer is required")
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
	s := &Server{opts: opts, conns: make(map[*trackedConn]struct{})}
	srv := smtp.NewServer(smtp.BackendFunc(s.newSession))
	srv.Domain = opts.Hostname
	srv.MaxMessageBytes = opts.MaxMessageBytes
	srv.MaxRecipients = opts.MaxRecipients
	srv.MaxLineLength = maxLineLength
	srv.ReadTimeout = readTimeout
	srv.WriteTimeout = writeTimeout
	srv.AllowInsecureAuth = opts.AllowInsecure
	srv.ErrorLog = errorLog{opts.Logger}
	if opts.TLSCertificate != nil {
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
// closes in the moment between commit and reply.
func (s *Server) Shutdown(ctx context.Context) error {
	err := s.srv.Shutdown(ctx)
	if err != nil && ctx.Err() != nil {
		s.mu.Lock()
		for c := range s.conns {
			_ = c.Conn.Close()
		}
		s.mu.Unlock()
	}
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
		tc := &trackedConn{Conn: c, srv: l.srv}
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
	mu           sync.Mutex
	authFailures int
	closeAfter   bool // close after the next write
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
	return &session{srv: s, conn: tracked(c), remote: remote}, nil
}

// session is one SMTP session. With BDAT, Data runs on another goroutine than
// the other methods, so the fields are guarded by mu.
type session struct {
	srv    *Server
	conn   *trackedConn
	remote string

	mu      sync.Mutex
	sender  string // set after a successful AUTH
	from    string
	hasFrom bool
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
	if (identity == "" || identity == username) && s.srv.opts.Senders.Verify(username, password) {
		s.mu.Lock()
		s.sender = username
		s.mu.Unlock()
		return nil
	}
	return s.authFailure()
}

func (s *session) authFailure() error {
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
		return nil, false, p.session.authFailure() // malformed response
	}
	return challenge, done, nil
}

func (s *session) Mail(from string, opts *smtp.MailOptions) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.sender == "" {
		return errAuthRequired
	}
	if !validAddress(from) {
		return errBadAddress
	}
	if opts != nil && opts.Size > s.srv.opts.MaxMessageBytes {
		return errTooBig
	}
	s.from, s.hasFrom, s.to = from, true, nil
	return nil
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
		return errBadAddress
	}
	if len(s.to) >= s.srv.opts.MaxRecipients {
		return errTooManyRcpt
	}
	s.to = append(s.to, to)
	return nil
}

func (s *session) Data(r io.Reader) error {
	s.mu.Lock()
	sender, from, to := s.sender, s.from, slices.Clone(s.to)
	ok := s.sender != "" && s.hasFrom && len(s.to) > 0
	s.mu.Unlock()
	if sender == "" {
		return errAuthRequired
	}
	if !ok {
		return errBadSequence
	}

	limit := s.srv.opts.MaxMessageBytes
	raw, err := io.ReadAll(io.LimitReader(r, limit+1))
	if err != nil {
		if errors.Is(err, smtp.ErrDataTooLarge) {
			return errTooBig
		}
		if errors.Is(err, smtp.ErrTooLongLine) {
			return errLineTooLong
		}
		s.srv.opts.Logger.Warn("reading message failed", "remote", s.remote, "sender", sender)
		return errReadFailed
	}
	if int64(len(raw)) > limit {
		return errTooBig
	}

	mailID, err := s.srv.accept(sender, from, to, raw)
	if err != nil {
		s.srv.opts.Logger.Error("storing mail failed, replied 451", "sender", sender, "err", err)
		return errStorage
	}
	return &smtp.SMTPError{Code: 250, EnhancedCode: smtp.EnhancedCode{2, 0, 0}, Message: "queued as " + mailID}
}

func (s *session) Reset() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.from, s.hasFrom, s.to = "", false, nil
}

func (s *session) Logout() error { return nil }

// accept processes and stores one mail. It returns the new id.
func (s *Server) accept(sender, from string, to []string, raw []byte) (string, error) {
	now := s.opts.Now()
	mailID := id.NewAt(now)
	msg := message.Process(raw)
	sealed, err := s.opts.Sealer.Seal(mailID, msg.Message)
	if err != nil {
		return "", err
	}

	m := store.NewMail{
		ID: mailID, ReceivedAt: now, Sender: sender, EnvelopeFrom: from, Recipients: to,
		MessageID: msg.MessageID, Ref: msg.Ref, Raw: sealed, Size: len(msg.Message),
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
// "@", no control characters, and none of the characters that would need a
// quoted local part (space, '"', backslash, '<', '>', '(', ')', ',', ';'). The
// SMTP library removes the quotes of a quoted local part, so such an address
// could not be passed on unchanged: written into the RCPT or MAIL command
// towards a target, a '>' would end the path early and the rest would be read
// as parameters, and a recipient allow-list would check a different address
// than the target sees. The empty (null) sender is allowed here; Rcpt rejects
// it separately.
func validAddress(a string) bool {
	if len(a) > maxAddressLen || strings.Count(a, "@") > 1 {
		return false
	}
	for i := 0; i < len(a); i++ {
		c := a[i]
		if c < 0x20 || c == 0x7f || strings.IndexByte(" \"\\<>(),;", c) >= 0 {
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
