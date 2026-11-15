package ui

import (
	"net/http"
	"time"

	"github.com/eneuberg/postwerk/internal/store"
)

type loginView struct {
	CSRF  string
	Error string
}

// loginForm shows the login form with a fresh CSRF token bound to a cookie.
func (u *UI) loginForm(w http.ResponseWriter, r *http.Request) {
	if _, ok := u.currentSession(r); ok {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	u.renderLogin(w, r, http.StatusOK, "")
}

func (u *UI) renderLogin(w http.ResponseWriter, r *http.Request, status int, msg string) {
	token := randomToken()
	setCookie(w, r, loginCookie, token, "/login", loginMaxAge)
	u.render(w, r, status, "login", view{Title: "Log in", Data: loginView{CSRF: token, Error: msg}})
}

// login checks name and token (U5). Every attempt costs one full check,
// whatever was typed; at most LoginSlots run at once.
func (u *UI) login(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		u.fail(w, r, http.StatusBadRequest, "The form could not be read.")
		return
	}
	c, err := r.Cookie(loginCookie)
	if err != nil || !equalSecret(c.Value, r.PostForm.Get("csrf")) {
		u.renderLogin(w, r, http.StatusForbidden, "The login form has expired. Please try again.")
		return
	}
	name, token := r.PostForm.Get("name"), r.PostForm.Get("token")

	timer := time.NewTimer(u.opts.LoginWait)
	select {
	case u.loginSlots <- struct{}{}:
		timer.Stop()
	case <-timer.C:
		u.opts.Logger.Warn("ui login refused, too many token checks at once", "limit", cap(u.loginSlots))
		u.renderLogin(w, r, http.StatusServiceUnavailable, "Too many logins at once. Please try again in a moment.")
		return
	case <-r.Context().Done():
		return
	}
	role, ok := u.opts.Tokens.Verify(name, token)
	<-u.loginSlots

	if !ok {
		// Name the token only if it exists: a token pasted into the name
		// field must not reach the event log.
		actor := ""
		if u.opts.Tokens.Known(name) {
			actor = name
		}
		if err := u.event(r.Context(), store.KindUILoginFailed, store.Warn, actor, ""); err != nil {
			u.opts.Logger.Error("ui: writing the failed-login event failed", "err", err)
		}
		u.renderLogin(w, r, http.StatusUnauthorized, "Name or token is wrong.")
		return
	}
	// The login counts only once it is in the audit log.
	if err := u.event(r.Context(), store.KindUILogin, store.Info, name, string(role)); err != nil {
		u.opts.Logger.Error("ui: writing the login event failed; login refused", "err", err)
		u.fail(w, r, http.StatusServiceUnavailable, "The login could not be recorded. Please try again later.")
		return
	}
	sess := u.sessions.create(name, role)
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
