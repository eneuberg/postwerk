package api

import (
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/eneuberg/postwerk/internal/query"
	"github.com/eneuberg/postwerk/internal/senders"
)

// The sender keys (D62–D67, D76–D78), for admin tokens only: list and read
// every sender of every source; create, rotate, revoke and change the policy
// of the senders kept in the database through senders.Store. A change writes
// its own event (sender.created, sender.rotated, sender.revoked,
// sender.policy_changed) with the token name as actor, in the transaction
// that makes it; a refused one is an api.request. A new key is in the
// response that made it and nowhere else: never in an event or a log line.

func (a *API) senderRoutes(mux *http.ServeMux) {
	mux.Handle("/v1/senders", a.byMethod(
		endpoint{name: "senders", method: http.MethodGet, admin: true, fn: a.listSenders},
		endpoint{name: "sender_create", method: http.MethodPost, admin: true, fn: a.createSender}))
	mux.Handle("/v1/senders/{name}",
		a.handle(endpoint{name: "sender", method: http.MethodGet, admin: true, fn: a.getSender}))
	mux.Handle("/v1/senders/{name}/rotate",
		a.handle(endpoint{name: "sender_rotate", method: http.MethodPost, admin: true, fn: a.rotateSender}))
	mux.Handle("/v1/senders/{name}/keys/{id}/revoke",
		a.handle(endpoint{name: "sender_revoke", method: http.MethodPost, admin: true, fn: a.revokeSenderKey}))
	mux.Handle("/v1/senders/{name}/revoke-all",
		a.handle(endpoint{name: "sender_revoke_all", method: http.MethodPost, admin: true, fn: a.revokeAllSenderKeys}))
	mux.Handle("/v1/senders/{name}/policy",
		a.handle(endpoint{name: "sender_policy", method: http.MethodPut, admin: true, fn: a.setSenderPolicy}))
}

// byMethod serves one path with an endpoint per method.
func (a *API) byMethod(eps ...endpoint) http.HandlerFunc {
	handlers := map[string]http.HandlerFunc{}
	var allow []string
	for _, e := range eps {
		handlers[e.method] = a.handle(e)
		allow = append(allow, e.method)
	}
	return func(w http.ResponseWriter, r *http.Request) {
		if h, ok := handlers[r.Method]; ok {
			h(w, r)
			return
		}
		w.Header().Set("Allow", strings.Join(allow, ", "))
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "Use "+strings.Join(allow, " or ")+" for this endpoint.")
	}
}

type policyJSON struct {
	From           []string `json:"from"`
	MaxPerMinute   int      `json:"max_per_minute"`
	MaxRcptPerHour int      `json:"max_rcpt_per_hour"`
}

type senderKeyJSON struct {
	ID         string         `json:"id"`
	State      query.KeyState `json:"state"`
	CreatedAt  *time.Time     `json:"created_at,omitempty"`
	CreatedBy  string         `json:"created_by,omitempty"`
	ExpiresAt  *time.Time     `json:"expires_at,omitempty"`
	RevokedAt  *time.Time     `json:"revoked_at,omitempty"`
	RevokedBy  string         `json:"revoked_by,omitempty"`
	LastUsedAt *time.Time     `json:"last_used_at,omitempty"`
}

type activityJSON struct {
	Mails            int `json:"mails"`
	FailedRecipients int `json:"failed_recipients"`
	AuthFailures     int `json:"auth_failures"`
	Rejected         int `json:"rejected"`
	RateLimited      int `json:"rate_limited"`
}

type senderJSON struct {
	Name string `json:"name"`
	// Source is "file" or "db"; left out for a removed sender.
	Source string            `json:"source,omitempty"`
	State  query.SenderState `json:"state"`
	// ReadOnly: the sender cannot be changed through the API (it is in the
	// file, removed, or the database is off).
	ReadOnly           bool            `json:"read_only"`
	Policy             policyJSON      `json:"policy"`
	Password           bool            `json:"password"`
	PasswordLastUsedAt *time.Time      `json:"password_last_used_at,omitempty"`
	Keys               []senderKeyJSON `json:"keys"`
	LastUsedAt         *time.Time      `json:"last_used_at,omitempty"`
	CreatedAt          *time.Time      `json:"created_at,omitempty"`
	CreatedBy          string          `json:"created_by,omitempty"`
	UpdatedAt          *time.Time      `json:"updated_at,omitempty"`
	UpdatedBy          string          `json:"updated_by,omitempty"`
	// Recent is the activity of the last 24 hours.
	Recent activityJSON `json:"recent"`
}

type sendersResponse struct {
	Since time.Time `json:"since"`
	// Managed: senders can be created through the API
	// (POSTWERK_SENDER_KEYS db or both).
	Managed bool         `json:"managed"`
	Senders []senderJSON `json:"senders"`
}

// issuedJSON is a new key. Key is shown here once and never again.
type issuedJSON struct {
	Sender    string     `json:"sender"`
	KeyID     string     `json:"key_id"`
	Key       string     `json:"key"`
	ExpiresAt *time.Time `json:"expires_at,omitempty"`
}

type revokeAllResponse struct {
	Revoked []string   `json:"revoked"`
	Sender  senderJSON `json:"sender"`
}

func (a *API) senderView(o query.SenderOverview) senderJSON {
	v := senderJSON{
		Name: o.Name, Source: string(o.Source), State: o.State(),
		ReadOnly: a.opts.Senders == nil || o.Source != senders.KindDB,
		Policy: policyJSON{From: o.Policy.From(), MaxPerMinute: o.Policy.MaxPerMinute(),
			MaxRcptPerHour: o.Policy.MaxRecipientsPerHour()},
		Password: o.Password, PasswordLastUsedAt: optTime(o.PasswordLastUsed), Keys: []senderKeyJSON{},
		LastUsedAt: optTime(o.LastUsed), CreatedAt: optTime(o.CreatedAt), CreatedBy: o.CreatedBy,
		UpdatedAt: optTime(o.UpdatedAt), UpdatedBy: o.UpdatedBy,
		Recent: activityJSON{Mails: o.Recent.Mails, FailedRecipients: o.Recent.FailedRecipients,
			AuthFailures: o.Recent.AuthFailures, Rejected: o.Recent.Rejected, RateLimited: o.Recent.RateLimited},
	}
	if v.Policy.From == nil {
		v.Policy.From = []string{}
	}
	for _, k := range o.Keys {
		v.Keys = append(v.Keys, senderKeyJSON{
			ID: k.ID, State: k.State(), CreatedAt: optTime(k.CreatedAt), CreatedBy: k.CreatedBy,
			ExpiresAt: optTime(k.ExpiresAt), RevokedAt: optTime(k.RevokedAt), RevokedBy: k.RevokedBy,
			LastUsedAt: optTime(k.LastUsed),
		})
	}
	return v
}

func (a *API) readFailed(endpoint string, err error) result {
	a.opts.Logger.Error("api: reading the store failed", "endpoint", endpoint, "err", err)
	return failed(http.StatusInternalServerError, "internal", "The store could not be read. Details are in the log.")
}

// listSenders returns every sender of every source, and the removed ones
// with activity in the last 24 hours.
func (a *API) listSenders(r *http.Request, _ caller) result {
	if r.URL.RawQuery != "" {
		return failed(http.StatusBadRequest, "bad_request", "This endpoint takes no URL parameters.")
	}
	list, err := a.opts.Query.Senders(r.Context())
	if err != nil {
		return a.readFailed("senders", err)
	}
	resp := sendersResponse{Since: utc(a.opts.Now().Add(-query.RecentWindow)), Managed: a.opts.Senders != nil,
		Senders: []senderJSON{}}
	for _, o := range list {
		resp.Senders = append(resp.Senders, a.senderView(o))
	}
	return result{status: http.StatusOK, count: len(resp.Senders), body: resp}
}

// sender reads the sender in the path; a result with a status is an error.
func (a *API) sender(r *http.Request, endpoint string) (query.SenderOverview, result) {
	o, err := a.opts.Query.Sender(r.Context(), r.PathValue("name"))
	if errors.Is(err, query.ErrNoSender) {
		return o, failed(http.StatusNotFound, "not_found", "There is no sender with this name.")
	}
	if err != nil {
		return o, a.readFailed(endpoint, err)
	}
	return o, result{}
}

func (a *API) getSender(r *http.Request, _ caller) result {
	if r.URL.RawQuery != "" {
		return failed(http.StatusBadRequest, "bad_request", "This endpoint takes no URL parameters.")
	}
	o, res := a.sender(r, "sender")
	if res.status != 0 {
		return res
	}
	return result{status: http.StatusOK, count: 1, body: a.senderView(o)}
}

// managed reads the sender in the path for a change: it must be kept in
// the database, and the database source must be on.
func (a *API) managed(r *http.Request, endpoint string) (query.SenderOverview, result) {
	if a.opts.Senders == nil {
		return query.SenderOverview{}, failed(http.StatusConflict, "db_off",
			"Senders are kept in the senders file only (POSTWERK_SENDER_KEYS=file).")
	}
	o, res := a.sender(r, endpoint)
	if res.status != 0 {
		return o, res
	}
	if o.Source != senders.KindDB {
		if o.Removed {
			return o, failed(http.StatusNotFound, "not_found", "There is no sender with this name.")
		}
		return o, failed(http.StatusConflict, "file_sender",
			"This sender is in the senders file and cannot be changed through the API; change the file.")
	}
	return o, result{}
}

// senderErrors map the errors of senders.Store to a status and a code.
var senderErrors = []struct {
	err    error
	status int
	code   string
}{
	{senders.ErrInvalidName, http.StatusBadRequest, "invalid_name"},
	{senders.ErrInvalidPolicy, http.StatusBadRequest, "invalid_policy"},
	{senders.ErrExpiry, http.StatusBadRequest, "invalid_expiry"},
	{senders.ErrNoSender, http.StatusNotFound, "not_found"},
	{senders.ErrNoKey, http.StatusNotFound, "not_found"},
	{senders.ErrNameTaken, http.StatusConflict, "name_taken"},
	{senders.ErrTooManyKeys, http.StatusConflict, "too_many_keys"},
	{senders.ErrRevoked, http.StatusConflict, "already_revoked"},
}

// changeFailed maps an error of senders.Store to its response. Their texts
// are safe to show; any other error is logged, never returned.
func (a *API) changeFailed(endpoint string, err error) result {
	for _, e := range senderErrors {
		if errors.Is(err, e.err) {
			return failed(e.status, e.code, capitalize(err.Error())+".")
		}
	}
	a.opts.Logger.Error("api: changing a sender failed", "endpoint", endpoint, "err", err)
	return failed(http.StatusInternalServerError, "internal", "The change could not be saved. Details are in the log.")
}

func capitalize(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

// policyOf checks a policy from a request; nil is the empty policy.
func policyOf(p *policyJSON) (senders.Policy, error) {
	if p == nil {
		return senders.Policy{}, nil
	}
	return senders.NewPolicy(p.From, p.MaxPerMinute, p.MaxRcptPerHour)
}

type createSenderRequest struct {
	Name      string      `json:"name"`
	Policy    *policyJSON `json:"policy"`
	ExpiresAt *time.Time  `json:"expires_at"`
}

type rotateRequest struct {
	ExpiresAt *time.Time `json:"expires_at"`
}

type emptyRequest struct{}

func expiry(t *time.Time) time.Time {
	if t == nil {
		return time.Time{}
	}
	return t.UTC()
}

func issued(iss senders.Issued) issuedJSON {
	return issuedJSON{Sender: iss.Sender, KeyID: iss.KeyID, Key: iss.Key, ExpiresAt: optTime(iss.ExpiresAt)}
}

// createSender adds a sender to the database with its first key; the
// response holds the key, once.
func (a *API) createSender(r *http.Request, c caller) result {
	var req createSenderRequest
	if e := decode(r, &req); e != nil {
		return result{status: statusOf(e), body: e}
	}
	if a.opts.Senders == nil {
		return failed(http.StatusConflict, "db_off", "Senders are kept in the senders file only (POSTWERK_SENDER_KEYS=file).")
	}
	if !senders.ValidName(req.Name) {
		return a.changeFailed("sender_create", senders.ErrInvalidName)
	}
	p, err := policyOf(req.Policy)
	if err != nil {
		return a.changeFailed("sender_create", err)
	}
	iss, err := a.opts.Senders.Create(r.Context(), senders.CreateRequest{Name: req.Name, Policy: p,
		ExpiresAt: expiry(req.ExpiresAt), Actor: c.name})
	if err != nil {
		return a.changeFailed("sender_create", err)
	}
	return result{status: http.StatusCreated, count: 1, recorded: true, body: issued(iss)}
}

// rotateSender adds a key to a database sender; the response holds it,
// once.
func (a *API) rotateSender(r *http.Request, c caller) result {
	var req rotateRequest
	if e := decode(r, &req); e != nil {
		return result{status: statusOf(e), body: e}
	}
	o, res := a.managed(r, "sender_rotate")
	if res.status != 0 {
		return res
	}
	iss, err := a.opts.Senders.Rotate(r.Context(), o.Name, expiry(req.ExpiresAt), c.name)
	if err != nil {
		return a.changeFailed("sender_rotate", err)
	}
	return result{status: http.StatusCreated, count: 1, recorded: true, body: issued(iss)}
}

// changed answers a change with the sender as it is now.
func (a *API) changed(r *http.Request, name string) senderJSON {
	o, err := a.opts.Query.Sender(r.Context(), name)
	if err != nil {
		// The change is made and recorded; only the answer is short.
		a.opts.Logger.Error("api: reading the sender after the change failed", "err", err)
		return senderJSON{Name: name, Keys: []senderKeyJSON{}, Policy: policyJSON{From: []string{}}}
	}
	return a.senderView(o)
}

// revokeSenderKey ends one key of a database sender at once.
func (a *API) revokeSenderKey(r *http.Request, c caller) result {
	var req emptyRequest
	if e := decode(r, &req); e != nil {
		return result{status: statusOf(e), body: e}
	}
	o, res := a.managed(r, "sender_revoke")
	if res.status != 0 {
		return res
	}
	id := r.PathValue("id")
	var found bool
	for _, k := range o.Keys {
		if k.ID == id {
			found = true
		}
	}
	if !found {
		return failed(http.StatusNotFound, "not_found", "This sender has no key with this id.")
	}
	if err := a.opts.Senders.Revoke(r.Context(), id, c.name); err != nil {
		return a.changeFailed("sender_revoke", err)
	}
	return result{status: http.StatusOK, count: 1, recorded: true, body: a.changed(r, o.Name)}
}

// revokeAllSenderKeys revokes every key of a database sender that is not
// revoked yet: the way to switch a sender off (D76).
func (a *API) revokeAllSenderKeys(r *http.Request, c caller) result {
	var req emptyRequest
	if e := decode(r, &req); e != nil {
		return result{status: statusOf(e), body: e}
	}
	o, res := a.managed(r, "sender_revoke_all")
	if res.status != 0 {
		return res
	}
	var open []string
	for _, k := range o.Keys {
		if k.RevokedAt.IsZero() {
			open = append(open, k.ID)
		}
	}
	if len(open) == 0 {
		return failed(http.StatusConflict, "nothing_to_revoke", "Every key of this sender is revoked already.")
	}
	done, err := senders.RevokeAll(r.Context(), a.opts.Senders, open, c.name)
	if err != nil {
		a.opts.Logger.Error("api: revoking all keys failed", "sender", o.Name, "revoked", len(done), "err", err)
		return failed(http.StatusInternalServerError, "internal", "Revoked "+strconv.Itoa(len(done))+" of "+
			strconv.Itoa(len(open))+" keys; read the sender to see which. Details are in the log.")
	}
	return result{status: http.StatusOK, count: 1, recorded: true,
		body: revokeAllResponse{Revoked: append([]string{}, done...), Sender: a.changed(r, o.Name)}}
}

// setSenderPolicy replaces a database sender's policy.
func (a *API) setSenderPolicy(r *http.Request, c caller) result {
	var req policyJSON
	if e := decode(r, &req); e != nil {
		return result{status: statusOf(e), body: e}
	}
	o, res := a.managed(r, "sender_policy")
	if res.status != 0 {
		return res
	}
	p, err := policyOf(&req)
	if err != nil {
		return a.changeFailed("sender_policy", err)
	}
	if err := a.opts.Senders.SetPolicy(r.Context(), o.Name, p, c.name); err != nil {
		return a.changeFailed("sender_policy", err)
	}
	return result{status: http.StatusOK, count: 1, recorded: true, body: a.changed(r, o.Name)}
}
