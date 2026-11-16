package ui

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"net/http"
	"sync"
	"time"
)

// Cookie names.
const (
	sessionCookie = "postwerk_session"
	// loginCookie binds the login form's CSRF token to the browser that
	// loaded the form (double submit), before there is a session.
	loginCookie = "postwerk_login"
	loginMaxAge = time.Hour
)

// session is one login (U6). Values are copied out of the store, so a
// handler never shares one with another request.
type session struct {
	id       string
	name     string
	csrf     string
	created  time.Time
	lastSeen time.Time
	// flash is a one-time notice for the next page, such as "account
	// created" after a redirect.
	flash string
}

// sessions keeps sessions in memory. A restart ends them all.
type sessions struct {
	mu       sync.Mutex
	m        map[string]*session
	now      func() time.Time
	idle     time.Duration
	absolute time.Duration
	max      int
}

func newSessions(now func() time.Time, idle, absolute time.Duration, max int) *sessions {
	return &sessions{m: make(map[string]*session), now: now, idle: idle, absolute: absolute, max: max}
}

// create starts a session with a new random id and CSRF token.
func (s *sessions) create(name string) session {
	now := s.now()
	sess := &session{id: randomToken(), name: name, csrf: randomToken(), created: now, lastSeen: now}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sweep(now)
	for len(s.m) >= s.max {
		var oldest *session
		for _, v := range s.m {
			if oldest == nil || v.lastSeen.Before(oldest.lastSeen) {
				oldest = v
			}
		}
		delete(s.m, oldest.id)
	}
	s.m[sess.id] = sess
	return *sess
}

// get returns a live session and marks it used.
func (s *sessions) get(id string) (session, bool) {
	now := s.now()
	s.mu.Lock()
	defer s.mu.Unlock()
	sess, ok := s.m[id]
	if !ok {
		return session{}, false
	}
	if s.expired(sess, now) {
		delete(s.m, id)
		return session{}, false
	}
	sess.lastSeen = now
	return *sess, true
}

func (s *sessions) delete(id string) {
	s.mu.Lock()
	delete(s.m, id)
	s.mu.Unlock()
}

// deleteName ends every session of the account name (U21).
func (s *sessions) deleteName(name string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for id, sess := range s.m {
		if sess.name == name {
			delete(s.m, id)
			n++
		}
	}
	return n
}

// setFlash keeps a notice for the session's next page.
func (s *sessions) setFlash(id, msg string) {
	s.mu.Lock()
	if sess, ok := s.m[id]; ok {
		sess.flash = msg
	}
	s.mu.Unlock()
}

// takeFlash returns the session's notice and clears it.
func (s *sessions) takeFlash(id string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	sess, ok := s.m[id]
	if !ok {
		return ""
	}
	msg := sess.flash
	sess.flash = ""
	return msg
}

func (s *sessions) len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.m)
}

func (s *sessions) expired(sess *session, now time.Time) bool {
	return now.Sub(sess.lastSeen) >= s.idle || now.Sub(sess.created) >= s.absolute
}

// sweep removes expired sessions; s.mu is held.
func (s *sessions) sweep(now time.Time) {
	for id, sess := range s.m {
		if s.expired(sess, now) {
			delete(s.m, id)
		}
	}
}

// currentSession returns the session named by the request's cookie.
func (u *UI) currentSession(r *http.Request) (session, bool) {
	c, err := r.Cookie(sessionCookie)
	if err != nil || !wellFormedToken(c.Value) {
		return session{}, false
	}
	return u.sessions.get(c.Value)
}

// setCookie sets a cookie that scripts cannot read (there are none anyway)
// and that other sites never send along (U6). Secure only over TLS: the
// admin listener is plain HTTP on loopback behind a tunnel.
func setCookie(w http.ResponseWriter, r *http.Request, name, value, path string, maxAge time.Duration) {
	c := &http.Cookie{
		Name: name, Value: value, Path: path, HttpOnly: true, SameSite: http.SameSiteStrictMode,
		Secure: r.TLS != nil,
	}
	switch {
	case maxAge > 0:
		c.MaxAge = int(maxAge / time.Second)
	case maxAge < 0:
		c.MaxAge = -1
	}
	http.SetCookie(w, c)
}

// randomToken returns 32 random bytes as unpadded base64url (43
// characters).
func randomToken() string {
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

func wellFormedToken(v string) bool {
	if len(v) != 43 {
		return false
	}
	for i := 0; i < len(v); i++ {
		c := v[i]
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_') {
			return false
		}
	}
	return true
}

// equalSecret compares a stored token with a submitted one in constant
// time. An empty stored token never matches.
func equalSecret(want, got string) bool {
	return want != "" && subtle.ConstantTimeCompare([]byte(want), []byte(got)) == 1
}

// rateLimit counts events per key in fixed windows (U16).
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

// allow counts one event for key and reports whether it is within the
// limit.
func (l *rateLimit) allow(key string) bool {
	now := l.now()
	l.mu.Lock()
	defer l.mu.Unlock()
	c := l.counts[key]
	if c == nil || now.Sub(c.start) >= l.window {
		c = &windowCount{start: now}
		l.counts[key] = c
	}
	c.n++
	return c.n <= l.limit
}
