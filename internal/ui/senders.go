package ui

import (
	"context"
	"errors"
	"html/template"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/eneuberg/postwerk/internal/query"
	"github.com/eneuberg/postwerk/internal/senders"
)

// The sender keys (D62–D67, D76–D79): a section of the settings page lists
// every sender of every source; a sender's own page shows its keys, policy
// and activity. Senders kept in the database are changed here through
// senders.Store (create, rotate, revoke, policy), each change a POST with the
// CSRF token and the acting account's own password, like the account changes;
// the store writes the change and its event with the account as actor.
// Senders from the file are read-only. A new key is shown once, in the
// response to the POST that made it (D77): never in a session, a flash, a
// URL, a log line or an event.

func (u *UI) senderRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /settings/senders", u.authed(redirectTo("/settings#senders")))
	mux.HandleFunc("POST /settings/senders", u.authed(u.createSender))
	mux.HandleFunc("GET /settings/senders/{name}", u.authed(u.sender))
	mux.HandleFunc("POST /settings/senders/{name}/rotate", u.authed(u.rotateSender))
	mux.HandleFunc("POST /settings/senders/{name}/policy", u.authed(u.setSenderPolicy))
	mux.HandleFunc("POST /settings/senders/{name}/keys/{id}/revoke", u.authed(u.revokeSenderKey))
	mux.HandleFunc("POST /settings/senders/{name}/revoke-all", u.authed(u.revokeAllSenderKeys))
}

// Which form of the sender page an error belongs to.
const (
	formRotate    = "rotate"
	formPolicy    = "policy"
	formRevoke    = "revoke"
	formRevokeAll = "revoke-all"
)

// senderForm is what was typed into a create or policy form, for showing
// it again after an error.
type senderForm struct {
	Name, From, PerMinute, RcptsPerHour, Expires string
}

// sendersSection is the "Sender keys" part of the settings page.
type sendersSection struct {
	// Managed: senders can be created here (POSTWERK_SENDER_KEYS db or
	// both).
	Managed bool
	Senders []senderRow
	Form    senderForm
	Error   string
	Today   string // the earliest expiry day, for the date field
}

// senderRow is one sender as the list and the sender page show it.
type senderRow struct {
	query.SenderOverview
	State   lampWord
	Managed bool
	Keys    []keyRow
	// ActiveKeys counts the keys that can log in now, OpenKeys those not
	// revoked (expired ones too).
	ActiveKeys, OpenKeys int
}

type keyRow struct {
	query.SenderKey
	State lampWord
	// Soon is set when an active key expires within a week.
	Soon string
}

// lampWord is a state shown as lamp and word: Class is a status class
// (st-…), whose lamp and shape follow design-system.md.
type lampWord struct {
	Word, Class string
}

// senderView is a sender's own page.
type senderView struct {
	Sender senderRow
	// Policy is the current policy as the edit form shows it.
	Policy senderForm
	// CanRotate: a key can be added now; RotateReason says why not.
	CanRotate    bool
	RotateReason string
	// Form names the form Error belongs to; Key the key of a refused
	// revoke.
	Form, Key, Error string
	Today            string
	// PolicyLine is the senders-file line of the current policy.
	PolicyLine string
}

// keyView is the page that shows a new key, once.
type keyView struct {
	Issued senders.Issued
	// Rotated: the key was added to an existing sender; Others are its
	// other active keys, to revoke once the application uses the new one.
	Rotated  bool
	Others   []string
	SMTPPort string
}

const expiresSoon = 7 * 24 * time.Hour

func (u *UI) senderRow(o query.SenderOverview, now time.Time) senderRow {
	row := senderRow{SenderOverview: o, Managed: u.opts.Senders != nil && o.Source == senders.KindDB}
	for _, k := range o.Keys {
		kr := keyRow{SenderKey: k, State: keyLamps[k.State()]}
		switch k.State() {
		case query.KeyActive:
			row.ActiveKeys++
			if !k.ExpiresAt.IsZero() && k.ExpiresAt.Sub(now) < expiresSoon {
				kr.Soon = "expires in " + formatAge(k.ExpiresAt.Sub(now))
			}
			fallthrough
		case query.KeyExpired:
			row.OpenKeys++
		}
		row.Keys = append(row.Keys, kr)
	}
	row.State = senderLamps[o.State()]
	return row
}

// The lamps of key and sender states (D79): active is lit like a handed-off
// mail, an expired key is a caution diamond (the application probably fails
// to log in), a revoked key or a sender switched off is a calm bar.
var (
	keyLamps = map[query.KeyState]lampWord{
		query.KeyActive:  {"active", "st-handed_off"},
		query.KeyExpired: {"expired", "st-unknown"},
		query.KeyRevoked: {"revoked", "st-skipped"},
	}
	senderLamps = map[query.SenderState]lampWord{
		query.SenderActive:  {"active", "st-handed_off"},
		query.SenderExpired: {"key expired", "st-unknown"},
		query.SenderOff:     {"no active key", "st-skipped"},
		query.SenderRemoved: {"removed", "st-skipped"},
	}
)

// senderRows lists every sender for the settings page.
func (u *UI) senderRows(ctx context.Context) ([]senderRow, error) {
	list, err := u.opts.Query.Senders(ctx)
	if err != nil {
		return nil, err
	}
	now := u.opts.Now()
	rows := make([]senderRow, 0, len(list))
	for _, o := range list {
		rows = append(rows, u.senderRow(o, now))
	}
	return rows, nil
}

func (u *UI) today() string { return u.opts.Now().UTC().Format(time.DateOnly) }

// senderFor reads the sender in the path, or shows a 404.
func (u *UI) senderFor(w http.ResponseWriter, r *http.Request) (senderRow, bool) {
	o, err := u.opts.Query.Sender(r.Context(), r.PathValue("name"))
	if errors.Is(err, query.ErrNoSender) {
		u.fail(w, r, http.StatusNotFound, "There is no sender with this name.")
		return senderRow{}, false
	}
	if err != nil {
		u.queryFailed(w, r, "sender", err)
		return senderRow{}, false
	}
	return u.senderRow(o, u.opts.Now()), true
}

func (u *UI) sender(w http.ResponseWriter, r *http.Request, sess session) {
	if s, ok := u.senderFor(w, r); ok {
		u.renderSender(w, r, sess, http.StatusOK, senderView{Sender: s, Policy: policyForm(s.Policy)})
	}
}

func (u *UI) renderSender(w http.ResponseWriter, r *http.Request, sess session, status int, v senderView) {
	s := v.Sender
	v.Today = u.today()
	v.PolicyLine = s.Name + ":policy " + s.Policy.String()
	switch {
	case !s.Managed:
	case s.ActiveKeys >= senders.MaxActiveKeys:
		v.RotateReason = "The sender has two active keys. Revoke the one the application no longer uses first."
	default:
		v.CanRotate = true
	}
	pv := pageView(sess, "settings", "Sender "+s.Name, v)
	pv.Flash = u.sessions.takeFlash(sess.id)
	u.render(w, r, status, "sender", pv)
}

func policyForm(p senders.Policy) senderForm {
	f := senderForm{From: strings.Join(p.From(), "\n")}
	if n := p.MaxPerMinute(); n > 0 {
		f.PerMinute = strconv.Itoa(n)
	}
	if n := p.MaxRecipientsPerHour(); n > 0 {
		f.RcptsPerHour = strconv.Itoa(n)
	}
	return f
}

// readPolicy reads the policy fields of a form. The message is safe to
// show: it never quotes a pattern.
func readPolicy(f senderForm) (senders.Policy, string) {
	var from []string
	for _, line := range strings.FieldsFunc(f.From, func(r rune) bool { return r == '\n' || r == '\r' || r == ',' }) {
		if line = strings.TrimSpace(line); line != "" {
			from = append(from, line)
		}
	}
	limit := func(v, what string) (int, string) {
		v = strings.TrimSpace(v)
		if v == "" {
			return 0, ""
		}
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			return 0, what + " must be a whole number, 0 or more (empty: no limit)."
		}
		return n, ""
	}
	per, msg := limit(f.PerMinute, "Messages per minute")
	if msg != "" {
		return senders.Policy{}, msg
	}
	rcpts, msg := limit(f.RcptsPerHour, "Recipients per hour")
	if msg != "" {
		return senders.Policy{}, msg
	}
	p, err := senders.NewPolicy(from, per, rcpts)
	if err != nil {
		return senders.Policy{}, capitalize(err.Error()) + "."
	}
	return p, ""
}

// readExpiry reads an optional expiry day: the key works until the end of
// that day, UTC.
func (u *UI) readExpiry(s string) (time.Time, string) {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}, ""
	}
	d, err := time.Parse(time.DateOnly, s)
	if err != nil {
		return time.Time{}, "The expiry is not a date (YYYY-MM-DD)."
	}
	t := d.AddDate(0, 0, 1)
	if !t.After(u.opts.Now()) {
		return time.Time{}, "The expiry must be today or later."
	}
	return t, ""
}

func formOf(r *http.Request) senderForm {
	return senderForm{
		Name: strings.TrimSpace(r.PostForm.Get("name")), From: r.PostForm.Get("from"),
		PerMinute: r.PostForm.Get("max_per_minute"), RcptsPerHour: r.PostForm.Get("max_rcpt_per_hour"),
		Expires: r.PostForm.Get("expires"),
	}
}

// changeMessage is the status and text for an error of senders.Store, or
// 0 for an error that is not the user's to fix.
func changeMessage(err error) (int, string) {
	switch {
	case errors.Is(err, senders.ErrNameTaken):
		return http.StatusConflict, "A sender with this name exists, in the senders file or in the database. Choose another name."
	case errors.Is(err, senders.ErrTooManyKeys):
		return http.StatusConflict, "The sender has two active keys. Revoke the one the application no longer uses first."
	case errors.Is(err, senders.ErrRevoked):
		return http.StatusConflict, "This key is already revoked."
	case errors.Is(err, senders.ErrNoSender), errors.Is(err, senders.ErrNoKey):
		return http.StatusNotFound, capitalize(err.Error()) + "."
	case errors.Is(err, senders.ErrInvalidName), errors.Is(err, senders.ErrExpiry),
		errors.Is(err, senders.ErrInvalidPolicy):
		return http.StatusBadRequest, capitalize(err.Error()) + "."
	}
	return 0, ""
}

// createSender adds a sender to the database with its first key and shows
// the key once.
func (u *UI) createSender(w http.ResponseWriter, r *http.Request, sess session) {
	f := formOf(r)
	refuse := func(status int, msg string) {
		u.renderSettingsWith(w, r, sess, status, settingsForms{sender: f, senderError: msg})
	}
	if u.opts.Senders == nil {
		refuse(http.StatusConflict, "Senders are kept in the senders file only (POSTWERK_SENDER_KEYS=file). "+
			"Set it to both or db to create senders here.")
		return
	}
	// The input first, so a typo does not cost a password check.
	if !senders.ValidName(f.Name) {
		refuse(http.StatusBadRequest, capitalize(senders.ErrInvalidName.Error())+".")
		return
	}
	p, msg := readPolicy(f)
	if msg != "" {
		refuse(http.StatusBadRequest, msg)
		return
	}
	exp, msg := u.readExpiry(f.Expires)
	if msg != "" {
		refuse(http.StatusBadRequest, msg)
		return
	}
	if status, msg := u.confirm(r, sess); msg != "" {
		refuse(status, msg)
		return
	}
	iss, err := u.opts.Senders.Create(r.Context(), senders.CreateRequest{Name: f.Name, Policy: p, ExpiresAt: exp, Actor: sess.name})
	if err != nil {
		if status, msg := changeMessage(err); status != 0 {
			refuse(status, msg)
			return
		}
		u.senderChangeFailed(w, r, err)
		return
	}
	u.showKey(w, r, sess, keyView{Issued: iss})
}

// showKey renders the page with a new key. It is the response to the POST
// that made the key; the key is nowhere else (D77).
func (u *UI) showKey(w http.ResponseWriter, r *http.Request, sess session, v keyView) {
	v.SMTPPort = u.opts.SMTPPort
	title := "New key for " + v.Issued.Sender
	if !v.Rotated {
		title = "Sender " + v.Issued.Sender + " created"
	}
	u.render(w, r, http.StatusOK, "senderkey", pageView(sess, "settings", title, v))
}

// managedSender reads the sender in the path for a change. A sender from
// the file, a removed one, or any when the database is off, gets its page
// with the reason.
func (u *UI) managedSender(w http.ResponseWriter, r *http.Request, sess session, form string) (senderRow, bool) {
	s, ok := u.senderFor(w, r)
	if !ok {
		return senderRow{}, false
	}
	if !s.Managed {
		msg := "This sender is in the senders file and cannot be changed here. Change its lines in the file; " +
			"Postwerk reads it again within 30 seconds."
		if s.Removed {
			msg = "This sender is in no source any more; there is nothing to change."
		} else if s.Source == senders.KindDB {
			msg = "Senders in the database cannot be changed while POSTWERK_SENDER_KEYS is file."
		}
		u.renderSender(w, r, sess, http.StatusConflict, senderView{Sender: s, Policy: policyForm(s.Policy), Form: form, Error: msg})
		return senderRow{}, false
	}
	return s, true
}

// rotateSender adds a key to a database sender and shows it once.
func (u *UI) rotateSender(w http.ResponseWriter, r *http.Request, sess session) {
	s, ok := u.managedSender(w, r, sess, formRotate)
	if !ok {
		return
	}
	refuse := func(status int, msg string) {
		u.renderSender(w, r, sess, status, senderView{Sender: s, Policy: policyForm(s.Policy), Form: formRotate, Error: msg})
	}
	exp, msg := u.readExpiry(r.PostForm.Get("expires"))
	if msg != "" {
		refuse(http.StatusBadRequest, msg)
		return
	}
	if status, msg := u.confirm(r, sess); msg != "" {
		refuse(status, msg)
		return
	}
	iss, err := u.opts.Senders.Rotate(r.Context(), s.Name, exp, sess.name)
	if err != nil {
		if status, msg := changeMessage(err); status != 0 {
			refuse(status, msg)
			return
		}
		u.senderChangeFailed(w, r, err)
		return
	}
	v := keyView{Issued: iss, Rotated: true}
	for _, k := range s.Keys {
		if k.Active {
			v.Others = append(v.Others, k.ID)
		}
	}
	u.showKey(w, r, sess, v)
}

// setSenderPolicy replaces a database sender's policy.
func (u *UI) setSenderPolicy(w http.ResponseWriter, r *http.Request, sess session) {
	s, ok := u.managedSender(w, r, sess, formPolicy)
	if !ok {
		return
	}
	f := formOf(r)
	refuse := func(status int, msg string) {
		u.renderSender(w, r, sess, status, senderView{Sender: s, Policy: f, Form: formPolicy, Error: msg})
	}
	p, msg := readPolicy(f)
	if msg != "" {
		refuse(http.StatusBadRequest, msg)
		return
	}
	if status, msg := u.confirm(r, sess); msg != "" {
		refuse(status, msg)
		return
	}
	if err := u.opts.Senders.SetPolicy(r.Context(), s.Name, p, sess.name); err != nil {
		if status, msg := changeMessage(err); status != 0 {
			refuse(status, msg)
			return
		}
		u.senderChangeFailed(w, r, err)
		return
	}
	u.sessions.setFlash(sess.id, "Policy of "+s.Name+" saved. It applies from the next mail.")
	http.Redirect(w, r, senderPath(s.Name), http.StatusSeeOther)
}

// revokeSenderKey ends one key of a database sender at once.
func (u *UI) revokeSenderKey(w http.ResponseWriter, r *http.Request, sess session) {
	s, ok := u.managedSender(w, r, sess, formRevoke)
	if !ok {
		return
	}
	id := r.PathValue("id")
	var key *keyRow
	for i := range s.Keys {
		if s.Keys[i].ID == id {
			key = &s.Keys[i]
		}
	}
	if key == nil {
		u.fail(w, r, http.StatusNotFound, "This sender has no key with this id.")
		return
	}
	refuse := func(status int, msg string) {
		u.renderSender(w, r, sess, status, senderView{Sender: s, Policy: policyForm(s.Policy), Form: formRevoke, Key: id, Error: msg})
	}
	if !key.RevokedAt.IsZero() {
		refuse(http.StatusConflict, "This key is already revoked.")
		return
	}
	if status, msg := u.confirm(r, sess); msg != "" {
		refuse(status, msg)
		return
	}
	if err := u.opts.Senders.Revoke(r.Context(), id, sess.name); err != nil {
		if status, msg := changeMessage(err); status != 0 {
			refuse(status, msg)
			return
		}
		u.senderChangeFailed(w, r, err)
		return
	}
	u.sessions.setFlash(sess.id, "Key "+id+" revoked. It no longer logs in; a connection that used it is refused at its next mail.")
	http.Redirect(w, r, senderPath(s.Name), http.StatusSeeOther)
}

// revokeAllSenderKeys revokes every key of a database sender: the way to
// switch a sender off (D76).
func (u *UI) revokeAllSenderKeys(w http.ResponseWriter, r *http.Request, sess session) {
	s, ok := u.managedSender(w, r, sess, formRevokeAll)
	if !ok {
		return
	}
	refuse := func(status int, msg string) {
		u.renderSender(w, r, sess, status, senderView{Sender: s, Policy: policyForm(s.Policy), Form: formRevokeAll, Error: msg})
	}
	var open []string
	for _, k := range s.Keys {
		if k.RevokedAt.IsZero() {
			open = append(open, k.ID)
		}
	}
	if len(open) == 0 {
		refuse(http.StatusConflict, "Every key of this sender is revoked already.")
		return
	}
	if status, msg := u.confirm(r, sess); msg != "" {
		refuse(status, msg)
		return
	}
	done, err := senders.RevokeAll(r.Context(), u.opts.Senders, open, sess.name)
	if err != nil {
		u.opts.Logger.Error("ui: revoking all keys failed", "sender", s.Name, "revoked", len(done), "err", err)
		u.fail(w, r, http.StatusInternalServerError, "Not every key could be revoked; reload the sender's page to see "+
			"which are. Details are in the log.")
		return
	}
	u.sessions.setFlash(sess.id, strconv.Itoa(len(done))+" "+pluralWord(len(done), "key", "keys")+" of "+s.Name+
		" revoked. The sender can no longer log in.")
	http.Redirect(w, r, senderPath(s.Name), http.StatusSeeOther)
}

func (u *UI) senderChangeFailed(w http.ResponseWriter, r *http.Request, err error) {
	u.opts.Logger.Error("ui: changing a sender failed", "err", err)
	u.fail(w, r, http.StatusInternalServerError, "The change could not be saved, so nothing was changed. Details are in the log.")
}

// senderPath is the sender's page; a valid name needs no escaping.
func senderPath(name string) string { return "/settings/senders/" + url.PathEscape(name) }

func pluralWord(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

// senderFuncs are the template functions of the sender pages.
var senderFuncs = template.FuncMap{
	"join": strings.Join,
	// stamp shows a time to the minute, UTC, or "never".
	"stamp": func(t time.Time) string {
		if t.IsZero() {
			return "never"
		}
		return t.UTC().Format("2006-01-02 15:04")
	},
	// revoked counts the revoked keys.
	"revoked": func(keys []keyRow) int {
		n := 0
		for _, k := range keys {
			if !k.RevokedAt.IsZero() {
				n++
			}
		}
		return n
	},
	// limits describes a policy's two limits.
	"limits": func(p senders.Policy) string {
		per, rcpts := p.MaxPerMinute(), p.MaxRecipientsPerHour()
		if per == 0 && rcpts == 0 {
			return "no limits"
		}
		var parts []string
		if per > 0 {
			parts = append(parts, groupDigits(per)+" messages per minute")
		}
		if rcpts > 0 {
			parts = append(parts, groupDigits(rcpts)+" recipients per hour")
		}
		return strings.Join(parts, ", ")
	},
	"group": groupDigits,
}

// groupDigits writes n with thin commas: 2,000.
func groupDigits(n int) string {
	s := strconv.Itoa(n)
	for i := len(s) - 3; i > 0; i -= 3 {
		s = s[:i] + "," + s[i:]
	}
	return s
}
