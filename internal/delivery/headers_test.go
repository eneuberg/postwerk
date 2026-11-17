package delivery

import (
	"bufio"
	"bytes"
	"net/textproto"
	"testing"

	"github.com/eneuberg/postwerk/internal/config"
	"github.com/eneuberg/postwerk/internal/message"
)

// A message whose first line starts with white space continues the field
// written before it. The sender must not be able to change a tracking
// switch or X-MJ-CustomID that way (D60).
func TestOwnFieldsSurviveLeadingContinuation(t *testing.T) {
	const mailID = "pw_01JD0000000000000000000000"
	raw := []byte(" 1\r\nX-Mailjet-TrackClick: 1\r\nSubject: hi\r\n\r\nbody\r\n")
	for _, tr := range []string{config.TrackingOff, config.TrackingAccount} {
		tgt := config.Target{Name: "mailjet", Status: config.StatusMailjet, MailjetTracking: tr}
		var b bytes.Buffer
		if err := message.Write(&b, headersFor(tgt, mailID), stripFor(tgt, message.Process(raw).Message)); err != nil {
			t.Fatal(err)
		}
		h, err := textproto.NewReader(bufio.NewReader(&b)).ReadMIMEHeader()
		if err != nil {
			t.Fatal(err)
		}
		if got := h.Values(message.HeaderMailjetCustomID); len(got) != 1 || got[0] != mailID {
			t.Errorf("tracking %s: X-MJ-CustomID = %q", tr, got)
		}
		if tr != config.TrackingOff {
			continue
		}
		for _, name := range []string{message.HeaderMailjetTrackOpen, message.HeaderMailjetTrackClick} {
			if got := h.Values(name); len(got) != 1 || got[0] != "0" {
				t.Errorf("%s = %q, want only \"0\"", name, got)
			}
		}
	}
}
