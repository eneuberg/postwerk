package delivery

import (
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/emersion/go-smtp"

	"github.com/eneuberg/postwerk/internal/config"
	"github.com/eneuberg/postwerk/internal/store"
)

func reply(code int, msg string) error {
	return &smtp.SMTPError{Code: code, EnhancedCode: smtp.EnhancedCode{code / 100, 0, 0}, Message: msg}
}

func TestClassify(t *testing.T) {
	netErr := &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connection refused")}
	tests := []struct {
		name     string
		err      error
		dataSent bool
		want     Outcome
		code     int
	}{
		{"250 after data", nil, true, HandedOff, 250},
		{"251 after data", reply(251, "ok"), true, HandedOff, 251},
		{"535 before data", reply(535, "bad login"), false, AuthFailed, 535},
		{"535 after data", reply(535, "bad login"), true, AuthFailed, 535},
		{"421 before data", reply(421, "busy"), false, Temporary, 421},
		{"451 after data", reply(451, "later"), true, Temporary, 451},
		{"550 rcpt", reply(550, "no such user"), false, Permanent, 550},
		{"554 after data", reply(554, "rejected"), true, Permanent, 554},
		{"530 auth required", reply(530, "auth required"), false, Permanent, 530},
		{"network before data", netErr, false, Temporary, 0},
		{"network after data", netErr, true, Unclear, 0},
		{"eof after data", io.ErrUnexpectedEOF, true, Unclear, 0},
		{"wrapped reply", fmt.Errorf("rcpt: %w", reply(452, "too many")), false, Temporary, 452},
		{"odd code before data", reply(354, "?"), false, Temporary, 354},
		{"odd code after data", reply(354, "?"), true, Unclear, 354},
	}
	for _, tt := range tests {
		o, code, _ := Classify(tt.err, tt.dataSent)
		if o != tt.want || code != tt.code {
			t.Errorf("%s: got (%v, %d), want (%v, %d)", tt.name, o, code, tt.want, tt.code)
		}
	}
	_, _, text := Classify(&smtp.SMTPError{Code: 550, EnhancedCode: smtp.EnhancedCode{5, 1, 1}, Message: "unknown"}, false)
	if text != "5.1.1 unknown" {
		t.Errorf("text = %q", text)
	}
}

func TestDecide(t *testing.T) {
	now := time.UnixMilli(10_000_000)
	backoff := func(n int) time.Duration { return time.Duration(n) * time.Minute }
	pol := func(on config.OnUnknown, optional bool, windowLeft time.Duration) policy {
		return policy{onUnknown: on, optional: optional, deadline: now.Add(windowLeft), backoff: backoff}
	}
	type want struct {
		status store.Status
		next   time.Duration // after now, for queued
		kind   string
		level  store.Level
	}
	tests := []struct {
		name string
		o    Outcome
		p    policy
		want want
	}{
		{"handed off", HandedOff, pol(config.Hold, false, time.Hour), want{store.HandedOff, 0, store.KindDeliveryHandedOff, store.Info}},
		{"temporary", Temporary, pol(config.Hold, false, time.Hour), want{store.Queued, 2 * time.Minute, store.KindDeliveryRetry, store.Warn}},
		{"temporary, window nearly over", Temporary, pol(config.Hold, false, 30*time.Second), want{store.Queued, 30 * time.Second, store.KindDeliveryRetry, store.Warn}},
		{"temporary, window over", Temporary, pol(config.Hold, false, 0), want{store.Failed, 0, store.KindDeliveryFailed, store.Error}},
		{"temporary, window over, optional", Temporary, pol(config.Hold, true, -time.Second), want{store.Failed, 0, store.KindDeliveryFailed, store.Warn}},
		{"auth failed", AuthFailed, pol(config.Hold, false, time.Hour), want{store.Queued, 2 * time.Minute, store.KindTargetAuthFailed, store.Error}},
		{"auth failed, optional", AuthFailed, pol(config.Hold, true, time.Hour), want{store.Queued, 2 * time.Minute, store.KindTargetAuthFailed, store.Warn}},
		{"auth failed, window over", AuthFailed, pol(config.Hold, false, 0), want{store.Failed, 0, store.KindDeliveryFailed, store.Error}},
		{"permanent", Permanent, pol(config.Retry, false, time.Hour), want{store.Failed, 0, store.KindDeliveryFailed, store.Error}},
		{"permanent, optional", Permanent, pol(config.Retry, true, time.Hour), want{store.Failed, 0, store.KindDeliveryFailed, store.Warn}},
		{"unclear, hold", Unclear, pol(config.Hold, false, time.Hour), want{store.Unknown, 0, store.KindDeliveryUnknown, store.Error}},
		{"unclear, hold, optional", Unclear, pol(config.Hold, true, time.Hour), want{store.Unknown, 0, store.KindDeliveryUnknown, store.Warn}},
		{"unclear, check", Unclear, pol(config.Check, false, time.Hour), want{store.Unknown, 0, store.KindDeliveryUnknown, store.Error}},
		{"unclear, retry", Unclear, pol(config.Retry, false, time.Hour), want{store.Queued, 2 * time.Minute, store.KindDeliveryRetry, store.Warn}},
		{"unclear, retry, window over", Unclear, pol(config.Retry, false, 0), want{store.Failed, 0, store.KindDeliveryFailed, store.Error}},
	}
	for _, tt := range tests {
		d := decide(tt.o, "detail", tt.p, now, 2)
		got := want{d.status, 0, d.kind, d.level}
		if d.status == store.Queued {
			got.next = d.next.Sub(now)
		}
		if got != tt.want {
			t.Errorf("%s: got %+v, want %+v", tt.name, got, tt.want)
		}
	}
	if d := decide(Unclear, "", pol(config.Check, false, time.Hour), now, 1); !strings.Contains(d.text, "check pending") {
		t.Errorf("check text = %q", d.text)
	}
}

func TestDefaultBackoff(t *testing.T) {
	want := []time.Duration{
		30 * time.Second, time.Minute, 2 * time.Minute, 5 * time.Minute, 10 * time.Minute, 30 * time.Minute,
		time.Hour, time.Hour, time.Hour,
	}
	for i, w := range want {
		if got := DefaultBackoff(i + 1); got != w {
			t.Errorf("attempt %d: %v, want %v", i+1, got, w)
		}
	}
	if DefaultBackoff(0) != 30*time.Second {
		t.Error("attempt 0")
	}
}
