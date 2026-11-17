package ui

import (
	"errors"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/eneuberg/postwerk/internal/query"
	"github.com/eneuberg/postwerk/internal/resolve"
	"github.com/eneuberg/postwerk/internal/store"
)

// The two admin actions on a delivery (D53–D55): close and resend. Each is
// a POST from the mail page with the session's CSRF token and the acting
// account's own current password, like the account changes; the store
// writes the change and its events in one transaction.

const (
	actionClose  = "close"
	actionResend = "resend"
)

var targetRe = regexp.MustCompile(`^[a-z0-9_]{1,32}$`)

// mailView is the mail page: the mail and what an admin can do with each
// of its deliveries.
type mailView struct {
	Mail query.MailDetail
	// Actions holds one entry per delivery, in the order of Mail.Deliveries.
	Actions []deliveryActions
	MaxNote int
	// SenderKnown: the mail's sender is in a source, so its page exists.
	SenderKnown bool
}

type deliveryActions struct {
	Close, Resend resolve.Availability
	// Closed counts the recipients an admin closed, by resolution.
	Confirmed, GivenUp int
	// Error is the message of a refused action on this delivery; Failed
	// names which form it belongs to.
	Error, Failed string
	// Show: there is something to say about the actions. A delivery whose
	// recipients are all final has nothing to resolve, and the page shows
	// no actions for it.
	Show bool
}

// showMail renders the mail page with the given status. act, if not nil,
// is the refused action to show at its delivery. The page is shown only
// once its ui.view event is in the event log (U24).
func (u *UI) showMail(w http.ResponseWriter, r *http.Request, sess session, id string, status int, act *actionError) {
	m, err := u.opts.Query.Mail(r.Context(), id, query.Full)
	if errors.Is(err, query.ErrNotFound) {
		u.fail(w, r, http.StatusNotFound, "There is no mail with this id. It may have been deleted by retention.")
		return
	}
	if err != nil {
		u.queryFailed(w, r, "mail", err)
		return
	}
	err = u.opts.Events.Append(r.Context(), store.Event{
		Time: u.opts.Now(), Kind: store.KindUIView, Level: store.Info, MailID: m.ID, Actor: sess.name,
	})
	if err != nil {
		u.opts.Logger.Error("ui: writing the view event failed; page not shown", "err", err)
		u.fail(w, r, http.StatusServiceUnavailable, "Opening the mail could not be recorded, so it is not shown. Please try again later.")
		return
	}
	v := mailView{Mail: m, MaxNote: resolve.MaxNote, SenderKnown: slices.Contains(u.opts.Query.SenderNames(), m.Sender)}
	for _, d := range m.Deliveries {
		var a deliveryActions
		a.Close, a.Resend = u.opts.Resolve.Available(d, m.Stored)
		for _, rc := range d.Recipients {
			switch rc.Resolution {
			case store.Confirmed:
				a.Confirmed++
			case store.GivenUp:
				a.GivenUp++
			}
		}
		if act != nil && act.target == d.Target {
			a.Error, a.Failed = act.msg, act.action
		}
		a.Show = a.Error != "" || a.Close.OK || a.Resend.OK ||
			a.Close.Reason != resolve.Message(store.ErrNothingToClose) ||
			a.Resend.Reason != resolve.Message(store.ErrNothingToResend)
		v.Actions = append(v.Actions, a)
	}
	pv := pageView(sess, "mails", "Mail "+m.ID, v)
	pv.Flash = u.sessions.takeFlash(sess.id)
	u.render(w, r, status, "mail", pv)
}

type actionError struct {
	target, action, msg string
}

// resolveAction handles POST /mails/{id}/targets/{target}/{close,resend}.
func (u *UI) resolveAction(action string) func(http.ResponseWriter, *http.Request, session) {
	return func(w http.ResponseWriter, r *http.Request, sess session) {
		id, target := r.PathValue("id"), r.PathValue("target")
		if !mailIDRe.MatchString(id) || !targetRe.MatchString(target) {
			u.fail(w, r, http.StatusNotFound, "There is no delivery of this mail to this target.")
			return
		}
		refuse := func(status int, msg string) {
			u.showMail(w, r, sess, id, status, &actionError{target: target, action: action, msg: msg})
		}
		res := store.Resolution(r.PostForm.Get("resolution"))
		note := r.PostForm.Get("note")
		if action == actionClose {
			// The input first, so a typo does not cost a password check.
			if res != store.Confirmed && res != store.GivenUp {
				refuse(http.StatusBadRequest, resolve.Message(resolve.ErrResolution))
				return
			}
			if _, err := resolve.CheckNote(note); err != nil {
				refuse(http.StatusBadRequest, resolve.Message(err))
				return
			}
		}
		if status, msg := u.confirm(r, sess); msg != "" {
			refuse(status, msg)
			return
		}
		var positions []int
		var err error
		if action == actionClose {
			positions, err = u.opts.Resolve.Close(r.Context(), id, target, res, note, sess.name, resolve.ViaUI)
		} else {
			positions, err = u.opts.Resolve.Resend(r.Context(), id, target, r.PostForm.Get("duplicate") == "yes",
				sess.name, resolve.ViaUI)
		}
		if err != nil {
			msg := resolve.Message(err)
			switch {
			case msg == "":
				u.opts.Logger.Error("ui: the action failed", "action", action, "err", err)
				u.fail(w, r, http.StatusInternalServerError, "The change could not be saved, so nothing was changed. Details are in the log.")
			case errors.Is(err, store.ErrNoDelivery):
				u.fail(w, r, http.StatusNotFound, msg)
			default:
				refuse(http.StatusConflict, msg)
			}
			return
		}
		n := strconv.Itoa(len(positions)) + " recipient"
		if len(positions) != 1 {
			n += "s"
		}
		if action == actionClose {
			u.sessions.setFlash(sess.id, "Delivery to "+target+" closed as "+strings.ReplaceAll(string(res), "_", " ")+" ("+n+").")
		} else {
			u.sessions.setFlash(sess.id, "Delivery to "+target+" queued again ("+n+").")
		}
		http.Redirect(w, r, "/mails/"+id, http.StatusSeeOther)
	}
}
