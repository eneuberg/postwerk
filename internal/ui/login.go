package ui

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"time"

	"github.com/eneuberg/postwerk/internal/auth"
	"github.com/eneuberg/postwerk/internal/store"
)

type loginView struct {
	CSRF   string
	Error  string
	Notice string
}

// Notices the login page shows after a redirect (?done=…); fixed texts.
var loginNotices = map[string]string{
	"password": "Your password was changed. Log in with the new one.",
	"deleted":  "Your account was deleted.",
}

// loginForm shows the login form with a fresh CSRF token bound to a cookie.
func (u *UI) loginForm(w http.ResponseWriter, r *http.Request) {
	if _, ok := u.currentSession(r); ok {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	u.renderLogin(w, r, http.StatusOK, "", loginNotices[r.URL.Query().Get("done")])
}

func (u *UI) renderLogin(w http.ResponseWriter, r *http.Request, status int, msg, notice string) {
	token := randomToken()
	setCookie(w, r, loginCookie, token, "/login", loginMaxAge)
	u.render(w, r, status, "login", view{Title: "Log in", Data: loginView{CSRF: token, Error: msg, Notice: notice}})
}

// errThrottled and errBusy end a password check before it starts.
var (
	errBusy      = errors.New("too many checks at once")
	errThrottled = errors.New("too many failed checks")
)

// checkResult is the outcome of one password check.
type checkResult struct {
	ok bool
	// known is set when the name is an account, so an event may name it.
	known bool
	// wait is set with errThrottled: how long to wait.
	wait time.Duration
}

// checkPassword checks name and password for a login or for confirming a
// change (U5, U22). A name or source that failed too often waits first,
// whatever the name. Otherwise every check costs the same, for file
// accounts, database accounts and unknown names: one PBKDF2 at the
// checker's cost, in one of the login slots. why is the text of the
// failure event ("" for a login, "confirm" for a change).
func (u *UI) checkPassword(r *http.Request, name, password, why string) (checkResult, error) {
	nameKey, sourceKey := throttleName(name), sourceOf(r)
	if wait := max(u.nameFails.wait(nameKey), u.sourceFails.wait(sourceKey)); wait > 0 {
		return checkResult{wait: wait}, errThrottled
	}
	timer := time.NewTimer(u.opts.LoginWait)
	select {
	case u.loginSlots <- struct{}{}:
		timer.Stop()
	case <-timer.C:
		u.opts.Logger.Warn("ui password check refused, too many at once", "limit", cap(u.loginSlots))
		return checkResult{}, errBusy
	case <-r.Context().Done():
		return checkResult{}, r.Context().Err()
	}
	res, err := u.verify(r.Context(), name, password)
	<-u.loginSlots
	if err != nil {
		return checkResult{}, err
	}
	if res.ok {
		u.nameFails.reset(nameKey)
		return res, nil
	}
	// Name the account only if it exists: a password pasted into the
	// name field must not reach the event log.
	actor := ""
	if res.known {
		actor = name
	}
	ctx := r.Context()
	if err := u.event(ctx, store.KindUILoginFailed, store.Warn, actor, why); err != nil {
		u.opts.Logger.Error("ui: writing the failed-login event failed", "err", err)
	}
	for _, t := range []struct {
		th   *throttle
		key  string
		what string
	}{{u.nameFails, nameKey, "account"}, {u.sourceFails, sourceKey, "source"}} {
		if t.th.fail(t.key) > 0 {
			if err := u.event(ctx, store.KindUILoginThrottled, store.Warn, actor, t.what); err != nil {
				u.opts.Logger.Error("ui: writing the throttle event failed", "err", err)
			}
		}
	}
	return res, nil
}

// verify runs the one PBKDF2 check. The tokens file wins over a database
// account of the same name.
func (u *UI) verify(ctx context.Context, name, password string) (checkResult, error) {
	u.checks.Add(1)
	if u.fileAccount(name) {
		_, ok := u.opts.Tokens.Verify(name, password)
		u.checker.Pad(u.opts.Tokens.Iterations())
		return checkResult{ok: ok, known: true}, nil
	}
	var hash string
	if auth.ValidName(name) {
		a, err := u.opts.Accounts.UIAccount(ctx, name)
		switch {
		case err == nil:
			hash = a.Hash
		case !errors.Is(err, store.ErrNoAccount):
			return checkResult{}, err
		}
	}
	return checkResult{ok: u.checker.Verify(hash, password), known: hash != ""}, nil
}

// throttleName is the throttle key of a typed name; every name that cannot
// be an account shares one key.
func throttleName(name string) string {
	if !auth.ValidName(name) {
		return "?"
	}
	return name
}

func sourceOf(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// waitText says how long to wait, in whole minutes.
func waitText(d time.Duration) string {
	m := int((d + time.Minute - 1) / time.Minute)
	if m <= 1 {
		return "a minute"
	}
	return fmt.Sprintf("%d minutes", m)
}

// login checks name and password (or token) (U5, U22).
func (u *UI) login(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		u.fail(w, r, http.StatusBadRequest, "The form could not be read.")
		return
	}
	c, err := r.Cookie(loginCookie)
	if err != nil || !equalSecret(c.Value, r.PostForm.Get("csrf")) {
		u.renderLogin(w, r, http.StatusForbidden, "The login form has expired. Please try again.", "")
		return
	}
	name := r.PostForm.Get("name")
	res, err := u.checkPassword(r, name, r.PostForm.Get("password"), "")
	switch {
	case errors.Is(err, errThrottled):
		u.renderLogin(w, r, http.StatusTooManyRequests,
			"Too many failed logins. Please try again in "+waitText(res.wait)+".", "")
		return
	case errors.Is(err, errBusy):
		u.renderLogin(w, r, http.StatusServiceUnavailable, "Too many logins at once. Please try again in a moment.", "")
		return
	case errors.Is(err, context.Canceled):
		return
	case err != nil:
		u.opts.Logger.Error("ui: reading the account failed", "err", err)
		u.fail(w, r, http.StatusInternalServerError, "The login could not be checked. Details are in the log.")
		return
	case !res.ok:
		u.renderLogin(w, r, http.StatusUnauthorized, "Name or password is wrong.", "")
		return
	}
	source := "database"
	if u.fileAccount(name) {
		source = "file"
	}
	// The login counts only once it is in the audit log.
	if err := u.event(r.Context(), store.KindUILogin, store.Info, name, source); err != nil {
		u.opts.Logger.Error("ui: writing the login event failed; login refused", "err", err)
		u.fail(w, r, http.StatusServiceUnavailable, "The login could not be recorded. Please try again later.")
		return
	}
	sess := u.sessions.create(name)
	setCookie(w, r, sessionCookie, sess.id, "/", 0)
	setCookie(w, r, loginCookie, "", "/login", -1)
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// logout ends the session.
func (u *UI) logout(w http.ResponseWriter, r *http.Request, sess session) {
	u.sessions.delete(sess.id)
	setCookie(w, r, sessionCookie, "", "/", -1)
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}
