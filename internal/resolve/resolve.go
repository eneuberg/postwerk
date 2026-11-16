// Package resolve holds the two admin actions on a delivery (D53–D55):
// close it with a resolution, or resend it while the stored message still
// exists. The UI and the JSON API both go through it, so they check the same
// rules and write the same events; the rules themselves are checked again in
// the store's transaction, where they cannot race with a worker.
//
// Postwerk takes on as little as it can here: a resend is possible only
// while retention still keeps the message for its own reasons, and a close
// removes the message at once when nothing else needs it. Nothing here
// keeps or extends content.
package resolve

import (
	"context"
	"errors"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/eneuberg/postwerk/internal/query"
	"github.com/eneuberg/postwerk/internal/store"
)

// MaxNote bounds the admin's note, in characters.
const MaxNote = 200

// Where an action came from, in the event text.
const (
	ViaUI  = "ui"
	ViaAPI = "api"
)

// Errors of the input; the store's errors (store.ErrInProgress and the
// others) pass through. Message gives the text to show for any of them.
var (
	ErrResolution    = errors.New("resolve: unknown resolution")
	ErrNoteTooLong   = errors.New("resolve: note too long")
	ErrNoteInvalid   = errors.New("resolve: note not plain text")
	ErrNotConfigured = errors.New("resolve: target not configured")
)

// Options configure a Service.
type Options struct {
	Store *store.Store
	// Targets are the configured target names: a resend to a target that
	// is not configured would wait for a worker that does not exist.
	Targets []string
	// Wake tells the delivery workers that work is due; may be nil.
	Wake func()
	// Now returns the current time; nil means time.Now.
	Now func() time.Time
}

// Service runs the actions.
type Service struct{ opts Options }

// New returns a Service.
func New(opts Options) *Service {
	if opts.Now == nil {
		opts.Now = time.Now
	}
	return &Service{opts: opts}
}

// Close closes the open recipients of one delivery (store.CloseDelivery)
// with resolution and an optional note. It returns the positions closed.
func (s *Service) Close(ctx context.Context, mailID, target string, res store.Resolution, note, actor, via string) ([]int, error) {
	if res != store.Confirmed && res != store.GivenUp {
		return nil, ErrResolution
	}
	note, err := CheckNote(note)
	if err != nil {
		return nil, err
	}
	return s.opts.Store.CloseDelivery(ctx, store.Close{
		MailID: mailID, Target: target, Resolution: res, Note: note, Actor: actor, Via: via, At: s.opts.Now(),
	})
}

// Resend queues the failed and unknown recipients of one delivery again
// (store.ResendDelivery) and wakes the workers. It returns the positions
// queued.
func (s *Service) Resend(ctx context.Context, mailID, target string, acceptDuplicate bool, actor, via string) ([]int, error) {
	if !slices.Contains(s.opts.Targets, target) {
		return nil, s.unconfigured(ctx, mailID, target)
	}
	p, err := s.opts.Store.ResendDelivery(ctx, store.Resend{
		MailID: mailID, Target: target, AcceptDuplicate: acceptDuplicate, Actor: actor, Via: via, At: s.opts.Now(),
	})
	if err == nil && s.opts.Wake != nil {
		s.opts.Wake()
	}
	return p, err
}

// unconfigured returns ErrNotConfigured for an existing delivery, or
// store.ErrNoDelivery.
func (s *Service) unconfigured(ctx context.Context, mailID, target string) error {
	ds, err := s.opts.Store.Deliveries(ctx, mailID)
	if err != nil {
		return err
	}
	if !slices.ContainsFunc(ds, func(d store.Delivery) bool { return d.Target == target }) {
		return store.ErrNoDelivery
	}
	return ErrNotConfigured
}

// CheckNote trims the note and checks it: at most MaxNote characters of
// plain text on one line. It is redacted when stored (addresses become
// <redacted>), so it never keeps an address.
func CheckNote(note string) (string, error) {
	note = strings.TrimSpace(note)
	switch {
	case !utf8.ValidString(note):
		return "", ErrNoteInvalid
	case utf8.RuneCountInString(note) > MaxNote:
		return "", ErrNoteTooLong
	case strings.ContainsFunc(note, func(r rune) bool {
		return unicode.IsControl(r) || r == ' ' || r == ' '
	}):
		return "", ErrNoteInvalid
	}
	return note, nil
}

// Availability says whether an action is possible on a delivery now, and
// if not, why (Reason, a fixed text).
type Availability struct {
	OK     bool
	Reason string
	// Recipients is the number of recipients the action would change.
	Recipients int
	// Duplicate: a resend includes an unknown recipient and needs the
	// admin's consent that it may arrive twice.
	Duplicate bool
	// ConfirmOK: a close may use the resolution confirmed (no recipient is
	// queued).
	ConfirmOK bool
}

// Available returns what an admin may do with delivery d of a mail whose
// message is (stored) or is not kept any more. The store checks the same
// rules again when the action runs.
func (s *Service) Available(d query.DeliveryDetail, stored bool) (closeA, resendA Availability) {
	rs := make([]store.RecipientState, len(d.Recipients))
	for i, r := range d.Recipients {
		rs[i] = store.RecipientState{Position: r.Position, Status: r.Status, Resolution: r.Resolution}
	}
	if p, err := store.CloseCheck(rs, ""); err != nil {
		closeA.Reason = Message(err)
	} else {
		_, cerr := store.CloseCheck(rs, store.Confirmed)
		closeA = Availability{OK: true, Recipients: len(p), ConfirmOK: cerr == nil}
	}
	p, unknown, err := store.ResendCheck(rs, stored)
	switch {
	case err != nil:
		resendA.Reason = Message(err)
	case !slices.Contains(s.opts.Targets, d.Target):
		resendA.Reason = Message(ErrNotConfigured)
	default:
		resendA = Availability{OK: true, Recipients: len(p), Duplicate: unknown}
	}
	return closeA, resendA
}

// Message is the text to show for an error of Close or Resend; empty for
// an error that is not about the request (a broken store).
func Message(err error) string {
	for _, m := range []struct {
		err error
		msg string
	}{
		{store.ErrNoDelivery, "There is no delivery of this mail to this target."},
		{store.ErrInProgress, "The delivery is being sent right now. Wait for the attempt to end, then try again."},
		{store.ErrNothingToClose, "Nothing to close: every recipient has a final result."},
		{store.ErrConfirmQueued, "A queued recipient was never handed to the target, so it cannot be confirmed. Give it up, or wait for its next attempt."},
		{store.ErrNothingToResend, "Nothing to resend: no recipient is failed or unknown."},
		{store.ErrRawGone, "The stored message was removed (after hand-off, by retention or by a close), so it cannot be sent again."},
		{store.ErrDuplicateRisk, "The target may already have this mail. Confirm that a resend may deliver it twice."},
		{ErrResolution, "Choose a resolution: confirmed or given_up."},
		{ErrNoteTooLong, "The note is too long (at most 200 characters)."},
		{ErrNoteInvalid, "The note must be plain text on one line."},
		{ErrNotConfigured, "This target is no longer configured, so nothing would send it."},
	} {
		if errors.Is(err, m.err) {
			return m.msg
		}
	}
	return ""
}
