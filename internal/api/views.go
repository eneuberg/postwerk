package api

import (
	"time"

	"github.com/eneuberg/postwerk/internal/query"
	"github.com/eneuberg/postwerk/internal/store"
)

// The JSON shapes of docs/api.md. They are built from internal/query only,
// which holds no message content; times are RFC 3339 in UTC, and a time that
// is not set is left out.

type targetStatus struct {
	Target string       `json:"target"`
	Status store.Status `json:"status"`
}

type mailSummary struct {
	ID         string         `json:"id"`
	ReceivedAt time.Time      `json:"received_at"`
	Sender     string         `json:"sender"`
	Size       int            `json:"size"`
	Recipients int            `json:"recipients"`
	Status     store.Status   `json:"status"`
	Targets    []targetStatus `json:"targets"`
}

func summary(m store.MailSummary) mailSummary {
	s := mailSummary{
		ID: m.ID, ReceivedAt: utc(m.ReceivedAt), Sender: m.Sender, Size: m.Size, Recipients: m.Recipients,
		Status: m.Status, Targets: []targetStatus{},
	}
	for _, t := range m.Targets {
		s.Targets = append(s.Targets, targetStatus{Target: t.Target, Status: t.Status})
	}
	return s
}

// address is an address as the token may see it.
type address struct {
	// State is full, masked, pseudonymized or null_sender.
	State   string `json:"state"`
	Address string `json:"address,omitempty"`
}

func addr(a query.Address) address {
	switch a.State {
	case query.Plain:
		return address{State: "full", Address: a.Text}
	case query.MaskedAddr:
		return address{State: "masked", Address: a.Text}
	case query.Pseudonymized:
		return address{State: "pseudonymized"}
	case query.NullSender:
		return address{State: "null_sender"}
	}
	return address{State: "pseudonymized"} // unknown: show nothing
}

type recipient struct {
	Position int `json:"position"`
	address
}

type params struct {
	Body         string `json:"body,omitempty"`
	SMTPUTF8     bool   `json:"smtputf8"`
	DeclaredSize int64  `json:"declared_size,omitempty"`
}

type deliveryRecipient struct {
	Position int          `json:"position"`
	Status   store.Status `json:"status"`
	LastCode int          `json:"last_code,omitempty"`
	LastText string       `json:"last_text,omitempty"`
}

type delivery struct {
	Target      string              `json:"target"`
	Status      store.Status        `json:"status"`
	Attempts    int                 `json:"attempts"`
	NextAttempt *time.Time          `json:"next_attempt,omitempty"`
	DataSentAt  *time.Time          `json:"data_sent_at,omitempty"`
	LastCode    int                 `json:"last_code,omitempty"`
	LastText    string              `json:"last_text,omitempty"`
	Recipients  []deliveryRecipient `json:"recipients"`
}

type event struct {
	Seq       int64       `json:"seq"`
	Time      time.Time   `json:"time"`
	Kind      string      `json:"kind"`
	Level     store.Level `json:"level"`
	Target    string      `json:"target,omitempty"`
	Recipient *int        `json:"recipient,omitempty"`
	Attempt   int         `json:"attempt,omitempty"`
	Code      int         `json:"code,omitempty"`
	Text      string      `json:"text,omitempty"`
}

type mailDetail struct {
	ID              string       `json:"id"`
	ReceivedAt      time.Time    `json:"received_at"`
	Sender          string       `json:"sender"`
	Size            int          `json:"size"`
	Ref             string       `json:"ref,omitempty"`
	MessageID       string       `json:"message_id,omitempty"`
	EnvelopeFrom    address      `json:"envelope_from"`
	Params          params       `json:"params"`
	Stored          bool         `json:"stored"`
	RawPurgedAt     *time.Time   `json:"raw_purged_at,omitempty"`
	PseudonymizedAt *time.Time   `json:"pseudonymized_at,omitempty"`
	Status          store.Status `json:"status"`
	Recipients      []recipient  `json:"recipients"`
	Deliveries      []delivery   `json:"deliveries"`
	Events          []event      `json:"events"`
	EventsTruncated bool         `json:"events_truncated"`
}

func detail(m query.MailDetail) mailDetail {
	d := mailDetail{
		ID: m.ID, ReceivedAt: utc(m.ReceivedAt), Sender: m.Sender, Size: m.Size, Ref: m.Ref, MessageID: m.MessageID,
		EnvelopeFrom: addr(m.EnvelopeFrom),
		Params:       params{Body: m.Params.Body, SMTPUTF8: m.Params.SMTPUTF8, DeclaredSize: m.Params.DeclaredSize},
		Stored:       m.Stored, RawPurgedAt: optTime(m.RawPurgedAt), PseudonymizedAt: optTime(m.PseudonymizedAt),
		Status: m.Status, Recipients: []recipient{}, Deliveries: []delivery{}, Events: []event{},
		EventsTruncated: m.EventsTruncated,
	}
	for i, r := range m.Recipients {
		d.Recipients = append(d.Recipients, recipient{Position: i, address: addr(r)})
	}
	for _, dl := range m.Deliveries {
		x := delivery{
			Target: dl.Target, Status: dl.Status, Attempts: dl.Attempts, NextAttempt: optTime(dl.NextAttempt),
			DataSentAt: optTime(dl.DataSent), LastCode: dl.LastCode, LastText: dl.LastText,
			Recipients: []deliveryRecipient{},
		}
		for _, r := range dl.Recipients {
			x.Recipients = append(x.Recipients, deliveryRecipient{
				Position: r.Position, Status: r.Status, LastCode: r.LastCode, LastText: r.LastText})
		}
		d.Deliveries = append(d.Deliveries, x)
	}
	for _, e := range m.Events {
		if e.Actor != "" {
			continue // caused by a person or a token (api.view, ui.view): not part of the mail's history
		}
		d.Events = append(d.Events, event{
			Seq: e.Seq, Time: utc(e.Time), Kind: e.Kind, Level: e.Level, Target: e.Target, Recipient: e.Recipient,
			Attempt: e.Attempt, Code: e.Code, Text: e.Text,
		})
	}
	return d
}

type targetOverview struct {
	Target     string `json:"target"`
	Configured bool   `json:"configured"`
	Optional   bool   `json:"optional"`
	// State is "up" or "down" for a configured target (D45), since
	// StateSince; DownReason is "auth" or "unreachable" while down.
	State      string     `json:"state,omitempty"`
	StateSince *time.Time `json:"state_since,omitempty"`
	DownReason string     `json:"down_reason,omitempty"`
	// Recent counts the recipients of mails received since Since, by
	// status.
	Recent                 map[store.Status]int `json:"recent"`
	RecentTotal            int                  `json:"recent_total"`
	Queued                 int                  `json:"queued"`
	OldestQueuedAt         *time.Time           `json:"oldest_queued_at,omitempty"`
	OldestQueuedAgeSeconds *int64               `json:"oldest_queued_age_seconds,omitempty"`
}

type overviewResponse struct {
	Now      time.Time        `json:"now"`
	Since    time.Time        `json:"since"`
	Targets  []targetOverview `json:"targets"`
	Problems []mailSummary    `json:"problems"`
}

func utc(t time.Time) time.Time { return t.UTC() }

func optTime(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	u := t.UTC()
	return &u
}
