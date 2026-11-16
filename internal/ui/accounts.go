package ui

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/eneuberg/postwerk/internal/auth"
	"github.com/eneuberg/postwerk/internal/store"
)

// Account pages (U17–U21). Every account is an admin; every change needs
// the acting account's own current password, ends the sessions of the
// account it changes, and is written together with its event.
func (u *UI) accountRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /accounts", u.authed(u.accounts))
	mux.HandleFunc("POST /accounts", u.authed(u.createAccount))
	mux.HandleFunc("GET /accounts/{name}", u.authed(u.account))
	mux.HandleFunc("POST /accounts/{name}/password", u.authed(u.setPassword))
	mux.HandleFunc("POST /accounts/{name}/delete", u.authed(u.deleteAccount))
}

type accountRow struct {
	Name     string
	FromFile bool
	// Shadowed: a database account whose name is also in the tokens file,
	// which wins at login.
	Shadowed  bool
	Self      bool
	CreatedAt time.Time
	CreatedBy string
	UpdatedAt time.Time
	UpdatedBy string
}

type accountsView struct {
	Accounts []accountRow
	Name     string // the name typed into the create form
	Error    string
	Min, Max int
}

type accountView struct {
	Account       accountRow
	PasswordError string
	DeleteError   string
	Min, Max      int
}

// accountRows lists the tokens file's accounts and the database's, by name.
func (u *UI) accountRows(ctx context.Context, self string) ([]accountRow, error) {
	db, err := u.opts.Accounts.UIAccounts(ctx)
	if err != nil {
		return nil, err
	}
	var rows []accountRow
	if u.opts.Tokens != nil {
		for _, n := range u.opts.Tokens.Names() {
			rows = append(rows, accountRow{Name: n, FromFile: true, Self: n == self})
		}
	}
	for _, a := range db {
		rows = append(rows, accountRow{
			Name: a.Name, Shadowed: u.fileAccount(a.Name), Self: a.Name == self,
			CreatedAt: a.CreatedAt, CreatedBy: a.CreatedBy, UpdatedAt: a.UpdatedAt, UpdatedBy: a.UpdatedBy,
		})
	}
	slices.SortStableFunc(rows, func(a, b accountRow) int { return strings.Compare(a.Name, b.Name) })
	return rows, nil
}

func (u *UI) accounts(w http.ResponseWriter, r *http.Request, sess session) {
	u.renderAccounts(w, r, sess, http.StatusOK, "", "")
}

func (u *UI) renderAccounts(w http.ResponseWriter, r *http.Request, sess session, status int, name, msg string) {
	rows, err := u.accountRows(r.Context(), sess.name)
	if err != nil {
		u.queryFailed(w, r, "accounts", err)
		return
	}
	v := pageView(sess, "accounts", "Accounts", accountsView{
		Accounts: rows, Name: name, Error: msg, Min: auth.MinPasswordChars, Max: auth.MaxPasswordChars,
	})
	v.Flash = u.sessions.takeFlash(sess.id)
	u.render(w, r, status, "accounts", v)
}

// accountFor returns the row of the account in the path, or false after
// a 404.
func (u *UI) accountFor(w http.ResponseWriter, r *http.Request, sess session) (accountRow, bool) {
	name := r.PathValue("name")
	if auth.ValidName(name) {
		rows, err := u.accountRows(r.Context(), sess.name)
		if err != nil {
			u.queryFailed(w, r, "account", err)
			return accountRow{}, false
		}
		// The file row comes first for a shadowed name.
		for _, row := range rows {
			if row.Name == name {
				return row, true
			}
		}
	}
	u.fail(w, r, http.StatusNotFound, "There is no account with this name.")
	return accountRow{}, false
}

func (u *UI) account(w http.ResponseWriter, r *http.Request, sess session) {
	if a, ok := u.accountFor(w, r, sess); ok {
		u.renderAccount(w, r, sess, http.StatusOK, accountView{Account: a})
	}
}

func (u *UI) renderAccount(w http.ResponseWriter, r *http.Request, sess session, status int, v accountView) {
	v.Min, v.Max = auth.MinPasswordChars, auth.MaxPasswordChars
	pv := pageView(sess, "accounts", "Account "+v.Account.Name, v)
	pv.Flash = u.sessions.takeFlash(sess.id)
	u.render(w, r, status, "account", pv)
}

// createAccount adds a database account (U17).
func (u *UI) createAccount(w http.ResponseWriter, r *http.Request, sess session) {
	name := strings.TrimSpace(r.PostForm.Get("name"))
	switch {
	case !auth.ValidName(name):
		u.renderAccounts(w, r, sess, http.StatusBadRequest, name,
			"Names have 1 to 64 characters: lower-case letters, digits, '.', '_' and '-', starting with a letter or digit.")
		return
	case u.fileAccount(name):
		u.renderAccounts(w, r, sess, http.StatusBadRequest, name, "This name is in the tokens file. Choose another name.")
		return
	}
	if msg := newPasswordProblem(name, r); msg != "" {
		u.renderAccounts(w, r, sess, http.StatusBadRequest, name, msg)
		return
	}
	if status, msg := u.confirm(r, sess); msg != "" {
		u.renderAccounts(w, r, sess, status, name, msg)
		return
	}
	hash, err := u.hashPassword(r)
	if err == nil {
		err = u.opts.Accounts.CreateUIAccount(r.Context(), name, hash, sess.name, u.opts.Now())
	}
	switch {
	case errors.Is(err, store.ErrAccountExists):
		u.renderAccounts(w, r, sess, http.StatusConflict, name, "An account with this name exists.")
		return
	case err != nil:
		u.changeFailed(w, r, err)
		return
	}
	u.sessions.setFlash(sess.id, "Account "+name+" created.")
	http.Redirect(w, r, "/accounts", http.StatusSeeOther)
}

// setPassword sets a new password for a database account, the acting
// account's own included (U18, U21).
func (u *UI) setPassword(w http.ResponseWriter, r *http.Request, sess session) {
	a, ok := u.accountFor(w, r, sess)
	if !ok {
		return
	}
	if a.FromFile {
		u.renderAccount(w, r, sess, http.StatusBadRequest, accountView{Account: a,
			PasswordError: "This account is in the tokens file; change its token there."})
		return
	}
	if msg := newPasswordProblem(a.Name, r); msg != "" {
		u.renderAccount(w, r, sess, http.StatusBadRequest, accountView{Account: a, PasswordError: msg})
		return
	}
	if status, msg := u.confirm(r, sess); msg != "" {
		u.renderAccount(w, r, sess, status, accountView{Account: a, PasswordError: msg})
		return
	}
	hash, err := u.hashPassword(r)
	if err == nil {
		err = u.opts.Accounts.SetUIPassword(r.Context(), a.Name, hash, sess.name, u.opts.Now())
	}
	if err != nil {
		u.changeFailed(w, r, err)
		return
	}
	u.sessions.deleteName(a.Name)
	if a.Name == sess.name {
		setCookie(w, r, sessionCookie, "", "/", -1)
		http.Redirect(w, r, "/login?done=password", http.StatusSeeOther)
		return
	}
	u.sessions.setFlash(sess.id, "New password for "+a.Name+" set. Its sessions were ended.")
	http.Redirect(w, r, "/accounts", http.StatusSeeOther)
}

// deleteAccount deletes a database account; never the last one (U20).
func (u *UI) deleteAccount(w http.ResponseWriter, r *http.Request, sess session) {
	a, ok := u.accountFor(w, r, sess)
	if !ok {
		return
	}
	if a.FromFile {
		u.renderAccount(w, r, sess, http.StatusBadRequest, accountView{Account: a,
			DeleteError: "This account is in the tokens file; remove its line there."})
		return
	}
	if status, msg := u.confirm(r, sess); msg != "" {
		u.renderAccount(w, r, sess, status, accountView{Account: a, DeleteError: msg})
		return
	}
	err := u.opts.Accounts.DeleteUIAccount(r.Context(), a.Name, sess.name, u.opts.Now())
	switch {
	case errors.Is(err, store.ErrLastAccount):
		u.renderAccount(w, r, sess, http.StatusConflict, accountView{Account: a,
			DeleteError: "This is the last account in the database, so it cannot be deleted. Create another account first."})
		return
	case err != nil:
		u.changeFailed(w, r, err)
		return
	}
	u.sessions.deleteName(a.Name)
	if a.Name == sess.name {
		setCookie(w, r, sessionCookie, "", "/", -1)
		http.Redirect(w, r, "/login?done=deleted", http.StatusSeeOther)
		return
	}
	u.sessions.setFlash(sess.id, "Account "+a.Name+" deleted. Its sessions were ended.")
	http.Redirect(w, r, "/accounts", http.StatusSeeOther)
}

// newPasswordProblem checks the new password and its repetition (U19).
func newPasswordProblem(name string, r *http.Request) string {
	pw := r.PostForm.Get("password")
	if err := auth.CheckPassword(name, pw); err != nil {
		return capitalize(err.Error()) + "."
	}
	if pw != r.PostForm.Get("confirm") {
		return "The two new passwords are not the same."
	}
	return ""
}

func capitalize(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

// confirm checks the acting account's own current password (U18). It
// returns the status and message to show, or an empty message.
func (u *UI) confirm(r *http.Request, sess session) (int, string) {
	res, err := u.checkPassword(r, sess.name, r.PostForm.Get("current"), "confirm")
	switch {
	case errors.Is(err, errThrottled):
		return http.StatusTooManyRequests, "Too many wrong passwords. Please try again in " + waitText(res.wait) + "."
	case errors.Is(err, errBusy):
		return http.StatusServiceUnavailable, "Too many password checks at once. Please try again in a moment."
	case err != nil:
		u.opts.Logger.Error("ui: checking the password failed", "err", err)
		return http.StatusInternalServerError, "Your password could not be checked. Details are in the log."
	case !res.ok:
		return http.StatusForbidden, "Your current password is wrong. Nothing was changed."
	}
	return 0, ""
}

// hashPassword hashes the new password in a login slot: it costs as much
// as a check.
func (u *UI) hashPassword(r *http.Request) (string, error) {
	timer := time.NewTimer(u.opts.LoginWait)
	defer timer.Stop()
	select {
	case u.loginSlots <- struct{}{}:
	case <-timer.C:
		return "", errBusy
	case <-r.Context().Done():
		return "", r.Context().Err()
	}
	defer func() { <-u.loginSlots }()
	return auth.Hash(r.PostForm.Get("password"), u.opts.Iterations)
}

func (u *UI) changeFailed(w http.ResponseWriter, r *http.Request, err error) {
	if errors.Is(err, store.ErrNoAccount) { // deleted meanwhile
		u.fail(w, r, http.StatusNotFound, "There is no account with this name.")
		return
	}
	if errors.Is(err, errBusy) {
		u.fail(w, r, http.StatusServiceUnavailable, "Too many password checks at once. Nothing was changed. Please try again in a moment.")
		return
	}
	u.opts.Logger.Error("ui: changing an account failed", "err", err)
	u.fail(w, r, http.StatusInternalServerError, "The change could not be saved, so nothing was changed. Details are in the log.")
}
