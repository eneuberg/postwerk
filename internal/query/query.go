// Package query is the read logic behind the UI and the JSON query API
// (U10–U12, U15, H2): overview, mail lists, search and the detail of one mail.
// It sits on top of the store, which keeps all SQL, and decides what a
// reader may see: addresses are masked here, by access level, so no caller
// can forget it. Nothing here returns message content; the store never hands
// it out for reading.
package query

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/eneuberg/postwerk/internal/addrhash"
	"github.com/eneuberg/postwerk/internal/store"
)

// Access is what a reader may see of addresses. The zero value is the safe
// one.
type Access int

const (
	// Masked shows addresses as j•••@e•••.com (role read).
	Masked Access = iota
	// Full shows addresses in full (role admin).
	Full
)

// Limits.
const (
	// PageSize is the number of mails per list page.
	PageSize = 50
	// MaxPage bounds the page number of a list.
	MaxPage = 2000
	// MaxResults bounds the mails one search returns.
	MaxResults = 100
	// MaxEvents bounds the events shown for one mail.
	MaxEvents = 500
	// ProblemCount is the number of recent problems on the overview.
	ProblemCount = 10
	// RecentWindow is the period the overview counts.
	RecentWindow = 24 * time.Hour
	// maxQuery bounds a search query (the longest Message-ID).
	maxQuery = 998
)

// Errors for callers to tell apart; their texts are safe to show.
var (
	ErrNotFound     = errors.New("no such mail")
	ErrInvalid      = errors.New("invalid filter")
	ErrEmptyQuery   = errors.New("empty search")
	ErrQueryTooLong = errors.New("search text too long")
)

// Options configure a Service.
type Options struct {
	Store *store.Store
	// Hasher finds pseudonymized addresses; nil when no hash key is
	// configured (then only plain addresses are found).
	Hasher *addrhash.Hasher
	// Targets are the configured target names, in configuration order.
	Targets []string
	// Optional are the configured targets with OPTIONAL=true. A mail's
	// state leaves them out (design.md section 4).
	Optional []string
	// Health returns the state of the configured targets (D45); nil when
	// nobody delivers, as in tests.
	Health func() []TargetHealth
	// Now returns the current time; nil means time.Now.
	Now func() time.Time
}

// Service answers read queries.
type Service struct {
	opts Options
}

// New returns a Service.
func New(opts Options) *Service {
	if opts.Now == nil {
		opts.Now = time.Now
	}
	return &Service{opts: opts}
}

// Targets returns the configured target names.
func (s *Service) Targets() []string { return slices.Clone(s.opts.Targets) }

// RecipientStatuses are the stored statuses in the order the overview shows
// them.
var RecipientStatuses = []store.Status{
	store.Queued, store.Sending, store.HandedOff, store.Failed, store.Unknown, store.Skipped,
}

// States are the derived states a list can be filtered by, in display
// order.
var States = []store.Status{
	store.Queued, store.Sending, store.Unknown, store.HandedOff, store.Partial, store.Failed, store.Skipped,
}

// ProblemStates are the derived states the overview lists as problems.
var ProblemStates = []store.Status{store.Failed, store.Unknown, store.Partial}

// TargetHealth is whether the delivery worker can send to a target.
type TargetHealth struct {
	Target string
	Down   bool
	// Since is when the target went down, or came up (or the worker
	// started).
	Since time.Time
	// Reason is "auth" or "unreachable" while the target is down.
	Reason string
}

// StatusCount is one cell of the overview.
type StatusCount struct {
	Status store.Status
	Count  int
}

// TargetOverview is one target on the overview.
type TargetOverview struct {
	Target string
	// Configured is false for a target that has deliveries in the store but
	// is no longer configured: nobody works on its queue.
	Configured bool
	// Optional is set for a configured target with OPTIONAL=true.
	Optional bool
	// Health is the target's state; zero (no Since) when unknown, as for a
	// target that is not configured.
	Health TargetHealth
	// Recent counts the recipients of mails received in RecentWindow, by
	// status, in RecipientStatuses order.
	Recent      []StatusCount
	RecentTotal int
	Queued      int
	// OldestQueued is the received time of the oldest mail queued here;
	// zero if none. OldestQueuedAge is its age.
	OldestQueued    time.Time
	OldestQueuedAge time.Duration
}

// Overview is the state of every target and the recent problems.
type Overview struct {
	Now      time.Time
	Since    time.Time
	Targets  []TargetOverview
	Problems []store.MailSummary
}

// Overview returns the overview page's data.
func (s *Service) Overview(ctx context.Context) (Overview, error) {
	now := s.opts.Now()
	o := Overview{Now: now, Since: now.Add(-RecentWindow)}
	stats, err := s.opts.Store.TargetStatsSince(ctx, o.Since)
	if err != nil {
		return o, err
	}
	byName := map[string]store.TargetStats{}
	for _, st := range stats {
		byName[st.Target] = st
	}
	names := slices.Clone(s.opts.Targets)
	for _, st := range stats {
		if !slices.Contains(names, st.Target) {
			names = append(names, st.Target)
		}
	}
	health := map[string]TargetHealth{}
	if s.opts.Health != nil {
		for _, h := range s.opts.Health() {
			health[h.Target] = h
		}
	}
	for _, n := range names {
		st := byName[n]
		t := TargetOverview{Target: n, Configured: slices.Contains(s.opts.Targets, n), Queued: st.QueuedMails,
			Optional: slices.Contains(s.opts.Optional, n), Health: health[n]}
		for _, status := range RecipientStatuses {
			c := st.Recent[status]
			t.Recent = append(t.Recent, StatusCount{Status: status, Count: c})
			t.RecentTotal += c
		}
		if !st.OldestQueued.IsZero() {
			t.OldestQueued = st.OldestQueued
			t.OldestQueuedAge = max(now.Sub(st.OldestQueued), 0)
		}
		o.Targets = append(o.Targets, t)
	}
	o.Problems, err = s.opts.Store.ListMails(ctx, store.MailFilter{States: ProblemStates, Limit: ProblemCount,
		Optional: s.opts.Optional})
	return o, err
}

// ListFilter selects a page of mails. Zero values select everything.
type ListFilter struct {
	State  string
	Target string
	// From and To are UTC days: received on or after From and on or before
	// To.
	From, To time.Time
	// Page counts from 1.
	Page int
}

// ListPage is one page of a mail list.
type ListPage struct {
	Filter  ListFilter
	Rows    []store.MailSummary
	HasNext bool
}

var targetRe = regexp.MustCompile(`^[a-z0-9_]{1,32}$`)

// List returns one page of mails, newest first.
func (s *Service) List(ctx context.Context, f ListFilter) (ListPage, error) {
	if f.Page == 0 {
		f.Page = 1
	}
	if f.Page < 1 || f.Page > MaxPage {
		return ListPage{}, fmt.Errorf("%w: page", ErrInvalid)
	}
	mf := store.MailFilter{Limit: PageSize + 1, Offset: (f.Page - 1) * PageSize, Optional: s.opts.Optional}
	if f.State != "" {
		st := store.Status(f.State)
		if !slices.Contains(States, st) {
			return ListPage{}, fmt.Errorf("%w: state", ErrInvalid)
		}
		mf.States = []store.Status{st}
	}
	if f.Target != "" {
		if !targetRe.MatchString(f.Target) {
			return ListPage{}, fmt.Errorf("%w: target", ErrInvalid)
		}
		mf.Target = f.Target
	}
	if !f.From.IsZero() {
		mf.From = day(f.From)
	}
	if !f.To.IsZero() {
		mf.To = day(f.To).AddDate(0, 0, 1)
	}
	if !mf.From.IsZero() && !mf.To.IsZero() && !mf.From.Before(mf.To) {
		return ListPage{}, fmt.Errorf("%w: from is after to", ErrInvalid)
	}
	rows, err := s.opts.Store.ListMails(ctx, mf)
	if err != nil {
		return ListPage{}, err
	}
	p := ListPage{Filter: f, Rows: rows}
	if len(rows) > PageSize {
		p.Rows, p.HasNext = rows[:PageSize], true
	}
	return p, nil
}

func day(t time.Time) time.Time {
	t = t.UTC()
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
}

// SearchBy is what a search query is matched against.
type SearchBy string

// Search kinds. They are also the text of ui.search events, so they must
// stay free of anything the user typed.
const (
	ByAny       SearchBy = "any"
	ByID        SearchBy = "id"
	ByRef       SearchBy = "ref"
	ByMessageID SearchBy = "message_id"
	ByAddress   SearchBy = "address"
)

// SearchKinds are the valid kinds, in display order.
var SearchKinds = []SearchBy{ByAny, ByID, ByRef, ByMessageID, ByAddress}

// ValidSearchBy reports whether by is a known kind.
func ValidSearchBy(by SearchBy) bool { return slices.Contains(SearchKinds, by) }

// Search finds mails by id, X-Postwerk-Ref, Message-ID or address (K3: an
// address also matches after pseudonymization). ByAny tries all of them;
// the address only if the query contains "@". The query must never be
// logged or stored by the caller.
func (s *Service) Search(ctx context.Context, by SearchBy, q string) ([]store.MailSummary, error) {
	if !ValidSearchBy(by) {
		return nil, fmt.Errorf("%w: search kind", ErrInvalid)
	}
	q = strings.TrimSpace(q)
	switch {
	case q == "":
		return nil, ErrEmptyQuery
	case len(q) > maxQuery:
		return nil, ErrQueryTooLong
	case !utf8.ValidString(q):
		return nil, fmt.Errorf("%w: search text", ErrInvalid)
	}
	var ids []string
	add := func(found []string, err error) error {
		if err != nil {
			return err
		}
		for _, id := range found {
			if !slices.Contains(ids, id) {
				ids = append(ids, id)
			}
		}
		return nil
	}
	st := s.opts.Store
	if by == ByID || by == ByAny {
		if _, err := st.Mail(ctx, q); err == nil {
			ids = append(ids, q)
		} else if !errors.Is(err, store.ErrNotFound) {
			return nil, err
		}
	}
	if by == ByRef || by == ByAny {
		if err := add(st.MailIDsByRef(ctx, q, MaxResults)); err != nil {
			return nil, err
		}
	}
	if by == ByMessageID || by == ByAny {
		// Stored as in the header, usually with angle brackets; find it
		// with or without them.
		variants := []string{q}
		if inner, ok := strings.CutPrefix(q, "<"); ok {
			if inner, ok := strings.CutSuffix(inner, ">"); ok && inner != "" {
				variants = append(variants, inner)
			}
		} else {
			variants = append(variants, "<"+q+">")
		}
		if err := add(st.MailIDsByMessageID(ctx, variants, MaxResults)); err != nil {
			return nil, err
		}
	}
	if by == ByAddress || (by == ByAny && strings.Contains(q, "@")) {
		normalized, hashes := addrhash.Normalize(q), []string(nil)
		if s.opts.Hasher != nil {
			normalized, hashes = s.opts.Hasher.Lookup(q)
		}
		if err := add(st.MailIDsByAddress(ctx, normalized, hashes, MaxResults)); err != nil {
			return nil, err
		}
	}
	if len(ids) > MaxResults {
		ids = ids[:MaxResults]
	}
	if len(ids) == 0 {
		return nil, nil
	}
	return st.ListMails(ctx, store.MailFilter{IDs: ids, Limit: MaxResults, Optional: s.opts.Optional})
}

// AddressState says how an address is shown.
type AddressState int

// Address states.
const (
	// Plain: Text is the address in full.
	Plain AddressState = iota
	// MaskedAddr: Text is the masked address.
	MaskedAddr
	// Pseudonymized: the address was replaced by a keyed hash; Text is
	// empty.
	Pseudonymized
	// NullSender: the envelope sender was empty (MAIL FROM:<>).
	NullSender
)

// Address is an address as a reader may see it.
type Address struct {
	Text  string
	State AddressState
}

func showAddress(addr string, pseudonymized bool, access Access) Address {
	switch {
	case addr == "":
		return Address{State: NullSender}
	case pseudonymized:
		return Address{State: Pseudonymized}
	case access == Full:
		return Address{Text: addr, State: Plain}
	}
	return Address{Text: Mask(addr), State: MaskedAddr}
}

// Mask hides most of an address: the first character of the local part, the
// first character of the domain and its top-level domain stay
// (jane@example.com → j•••@e•••.com).
func Mask(addr string) string {
	local, domain, ok := cutLast(addr, "@")
	if !ok {
		return first(addr) + dots
	}
	return first(local) + dots + "@" + maskDomain(domain)
}

const dots = "•••"

// maskDomain keeps the first character and the top-level domain
// (example.com → e•••.com).
func maskDomain(domain string) string {
	m := first(domain) + dots
	if i := strings.LastIndex(domain, "."); i > 0 && i < len(domain)-1 {
		m += domain[i:]
	}
	return m
}

// encodedAddress matches an address folded into a Message-ID's left part,
// as bounce and tracking ids do (jane=example.com, jane%example.com).
var encodedAddress = regexp.MustCompile(`[=%][A-Za-z0-9-]+\.[A-Za-z]{2,}`)

// MaskMessageID hides what a Message-ID may say about hosts and people: the
// part after the last "@" is masked like an address's domain, and the part
// before it too if it looks like an address (it holds an "@" or an encoded
// address). Angle brackets stay. <4711.abc@shop.example.com> →
// <4711.abc@s•••.com>; <jane@example.com@mx.example.org> → <j•••@m•••.org>.
// A value without "@" is masked as a whole.
func MaskMessageID(id string) string {
	inner := id
	open, closed := strings.HasPrefix(inner, "<"), strings.HasSuffix(inner, ">")
	if open {
		inner = inner[1:]
	}
	if closed && inner != "" {
		inner = inner[:len(inner)-1]
	}
	left, right, ok := cutLast(inner, "@")
	var m string
	switch {
	case !ok:
		m = first(inner) + dots
	case strings.Contains(left, "@") || encodedAddress.MatchString(left):
		m = first(left) + dots + "@" + maskDomain(right)
	default:
		m = left + "@" + maskDomain(right)
	}
	if open {
		m = "<" + m
	}
	if closed {
		m += ">"
	}
	return m
}

func cutLast(s, sep string) (before, after string, found bool) {
	if i := strings.LastIndex(s, sep); i >= 0 {
		return s[:i], s[i+len(sep):], true
	}
	return s, "", false
}

func first(s string) string {
	r, n := utf8.DecodeRuneInString(s)
	if n == 0 || r == utf8.RuneError {
		return ""
	}
	return string(r)
}

// RecipientDetail is one recipient of one delivery.
type RecipientDetail struct {
	Position int
	Address  Address
	Status   store.Status
	LastCode int
	LastText string
}

// DeliveryDetail is one delivery of a mail.
type DeliveryDetail struct {
	Target      string
	Status      store.Status
	Attempts    int
	NextAttempt time.Time
	DataSent    time.Time
	LastCode    int
	LastText    string
	Recipients  []RecipientDetail
}

// MailDetail is everything the store keeps about a mail, except its
// content.
type MailDetail struct {
	ID              string
	ReceivedAt      time.Time
	Sender          string
	Size            int
	Ref             string
	MessageID       string // masked unless Full (MaskMessageID)
	EnvelopeFrom    Address
	Params          store.Params
	Stored          bool      // the encrypted message is still in the store
	RawPurgedAt     time.Time // zero while stored
	PseudonymizedAt time.Time // zero while addresses are plain
	Status          store.Status
	Recipients      []Address
	Deliveries      []DeliveryDetail
	Events          []store.Event
	EventsTruncated bool
}

// Mail returns the detail of one mail; addresses as access allows.
func (s *Service) Mail(ctx context.Context, id string, access Access) (MailDetail, error) {
	st := s.opts.Store
	m, err := st.Mail(ctx, id)
	if errors.Is(err, store.ErrNotFound) {
		return MailDetail{}, ErrNotFound
	}
	if err != nil {
		return MailDetail{}, err
	}
	pseudo := !m.PseudonymizedAt.IsZero()
	d := MailDetail{
		ID: m.ID, ReceivedAt: m.ReceivedAt, Sender: m.Sender, Size: m.Size, Ref: m.Ref, MessageID: m.MessageID,
		EnvelopeFrom: showAddress(m.EnvelopeFrom, pseudo, access), Params: m.Params, Stored: m.HasRaw,
		RawPurgedAt: m.RawPurgedAt, PseudonymizedAt: m.PseudonymizedAt,
	}
	if access != Full && d.MessageID != "" {
		d.MessageID = MaskMessageID(d.MessageID)
	}
	for _, r := range m.Recipients {
		d.Recipients = append(d.Recipients, showAddress(r, pseudo, access))
	}
	ds, err := st.Deliveries(ctx, id)
	if err != nil {
		return MailDetail{}, err
	}
	for _, dl := range ds {
		dd := DeliveryDetail{
			Target: dl.Target, Status: dl.Status, Attempts: dl.Attempts, NextAttempt: dl.NextAttempt,
			DataSent: dl.DataSent, LastCode: dl.LastCode, LastText: dl.LastText,
		}
		for _, r := range dl.Recipients {
			rd := RecipientDetail{Position: r.Position, Status: r.Status, LastCode: r.LastCode, LastText: r.LastText}
			if r.Position >= 0 && r.Position < len(d.Recipients) {
				rd.Address = d.Recipients[r.Position]
			}
			dd.Recipients = append(dd.Recipients, rd)
		}
		d.Deliveries = append(d.Deliveries, dd)
	}
	d.Status = store.MailStatus(ds, func(t string) bool { return slices.Contains(s.opts.Optional, t) })
	d.Events, err = st.MailEvents(ctx, id, MaxEvents+1)
	if err != nil {
		return MailDetail{}, err
	}
	if len(d.Events) > MaxEvents {
		d.Events, d.EventsTruncated = d.Events[:MaxEvents], true
	}
	return d, nil
}
