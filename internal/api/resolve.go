package api

import (
	"errors"
	"net/http"

	"github.com/eneuberg/postwerk/internal/query"
	"github.com/eneuberg/postwerk/internal/resolve"
	"github.com/eneuberg/postwerk/internal/store"
)

// The admin actions on one delivery (D53–D55), for admin tokens only. A
// change writes its own events, delivery.closed or delivery.resend with the
// token name as actor, in the transaction that makes it; a refused request
// is an api.request like any other.

type closeRequest struct {
	Resolution store.Resolution `json:"resolution"`
	Note       string           `json:"note"`
}

type resendRequest struct {
	AcceptDuplicate bool `json:"accept_duplicate"`
}

type actionResponse struct {
	ID     string `json:"id"`
	Target string `json:"target"`
	// Recipients are the positions the action changed.
	Recipients []int    `json:"recipients"`
	Delivery   delivery `json:"delivery"`
}

// actionTarget reads and checks the path; a malformed one is a 404, like
// an unknown mail.
func actionTarget(r *http.Request) (id, target string, ok bool) {
	id, target = r.PathValue("id"), r.PathValue("target")
	return id, target, mailIDRe.MatchString(id) && targetRe.MatchString(target)
}

func (a *API) closeDelivery(r *http.Request, c caller) result {
	id, target, ok := actionTarget(r)
	if !ok {
		return failed(http.StatusNotFound, "not_found", resolve.Message(store.ErrNoDelivery))
	}
	var req closeRequest
	if e := decode(r, &req); e != nil {
		return result{status: statusOf(e), body: e}
	}
	positions, err := a.opts.Resolve.Close(r.Context(), id, target, req.Resolution, req.Note, c.name, resolve.ViaAPI)
	return a.actionResult(r, id, target, positions, err)
}

func (a *API) resendDelivery(r *http.Request, c caller) result {
	id, target, ok := actionTarget(r)
	if !ok {
		return failed(http.StatusNotFound, "not_found", resolve.Message(store.ErrNoDelivery))
	}
	var req resendRequest
	if e := decode(r, &req); e != nil {
		return result{status: statusOf(e), body: e}
	}
	positions, err := a.opts.Resolve.Resend(r.Context(), id, target, req.AcceptDuplicate, c.name, resolve.ViaAPI)
	return a.actionResult(r, id, target, positions, err)
}

// conflicts are the error codes of a 409, by cause.
var conflicts = []struct {
	err  error
	code string
}{
	{store.ErrInProgress, "in_progress"},
	{store.ErrNothingToClose, "nothing_to_close"},
	{store.ErrConfirmQueued, "confirm_queued"},
	{store.ErrNothingToResend, "nothing_to_resend"},
	{store.ErrRawGone, "message_removed"},
	{store.ErrDuplicateRisk, "duplicate_risk"},
	{resolve.ErrNotConfigured, "target_not_configured"},
}

func (a *API) actionResult(r *http.Request, id, target string, positions []int, err error) result {
	if err != nil {
		msg := resolve.Message(err)
		switch {
		case msg == "":
			a.opts.Logger.Error("api: the action failed", "err", err)
			return failed(http.StatusInternalServerError, "internal", "The change could not be saved. Details are in the log.")
		case errors.Is(err, store.ErrNoDelivery):
			return failed(http.StatusNotFound, "not_found", msg)
		}
		for _, c := range conflicts {
			if errors.Is(err, c.err) {
				return failed(http.StatusConflict, c.code, msg)
			}
		}
		return failed(http.StatusBadRequest, "bad_request", msg)
	}
	resp := actionResponse{ID: id, Target: target, Recipients: positions}
	// The change is made and recorded; the delivery is read back for the
	// answer. Should that read fail, the answer says so without the
	// delivery.
	m, err := a.opts.Query.Mail(r.Context(), id, query.Full)
	if err == nil {
		for _, d := range m.Deliveries {
			if d.Target == target {
				resp.Delivery = deliveryView(d)
			}
		}
	} else {
		a.opts.Logger.Error("api: reading the delivery after the change failed", "err", err)
	}
	return result{status: http.StatusOK, count: 1, recorded: true, body: resp}
}
