package message

import (
	"bufio"
	"bytes"
	"net/textproto"
	"reflect"
	"slices"
	"strings"
	"testing"
)

func TestProcessStripsOwnHeaders(t *testing.T) {
	in := "From: a@example.test\r\n" +
		"X-Postwerk-Ref: order-42\r\n" +
		"Subject: hello\r\n" +
		"x-postwerk-skip: Capture,\r\n" +
		"\tmailpit , capture\r\n" +
		"X-POSTWERK-Other: folded\r\n" +
		"  continuation\r\n" +
		"Message-ID: <abc@example.test>\r\n" +
		"\r\n" +
		"X-Postwerk-Ref: in the body stays\r\n" +
		"body line\r\n"
	want := "From: a@example.test\r\n" +
		"Subject: hello\r\n" +
		"Message-ID: <abc@example.test>\r\n" +
		"\r\n" +
		"X-Postwerk-Ref: in the body stays\r\n" +
		"body line\r\n"

	res := Process([]byte(in))
	if got := string(res.Message); got != want {
		t.Fatalf("message:\n%q\nwant\n%q", got, want)
	}
	if res.Ref != "order-42" {
		t.Errorf("Ref = %q", res.Ref)
	}
	if !reflect.DeepEqual(res.Skip, []string{"capture", "mailpit"}) {
		t.Errorf("Skip = %q", res.Skip)
	}
	if res.MessageID != "<abc@example.test>" {
		t.Errorf("MessageID = %q", res.MessageID)
	}
}

func TestProcessLFOnly(t *testing.T) {
	in := "Subject: x\nX-Postwerk-Skip: a\n b\nTo: y\n\nbody\n"
	res := Process([]byte(in))
	if got, want := string(res.Message), "Subject: x\nTo: y\n\nbody\n"; got != want {
		t.Fatalf("message = %q, want %q", got, want)
	}
	if !reflect.DeepEqual(res.Skip, []string{"a b"}) {
		t.Errorf("Skip = %q", res.Skip)
	}
}

func TestProcessNoBody(t *testing.T) {
	in := "Subject: x\r\nX-Postwerk-Ref: r1\r\n"
	res := Process([]byte(in))
	if got := string(res.Message); got != "Subject: x\r\n" {
		t.Fatalf("message = %q", got)
	}
	if res.Ref != "r1" {
		t.Errorf("Ref = %q", res.Ref)
	}

	// Last line without a line ending.
	res = Process([]byte("Subject: x\r\nX-Postwerk-Ref: r2"))
	if got := string(res.Message); got != "Subject: x\r\n" || res.Ref != "r2" {
		t.Fatalf("message = %q, ref = %q", got, res.Ref)
	}
}

func TestProcessEmptyAndBodyOnly(t *testing.T) {
	if res := Process(nil); len(res.Message) != 0 {
		t.Fatalf("message = %q", res.Message)
	}
	in := "\r\nX-Postwerk-Ref: body\r\n"
	if res := Process([]byte(in)); string(res.Message) != in || res.Ref != "" {
		t.Fatalf("message = %q, ref = %q", res.Message, res.Ref)
	}
}

func TestProcessKeepsBytesExactly(t *testing.T) {
	// Unusual but legal bytes: 8-bit header text, trailing white space,
	// mixed line endings, a dot line, binary-looking body.
	in := "Subject: Grüße \t \r\n" +
		"Weird-Line-Without-Colon\r\n" +
		"X-Postwerk-Id: forged\r\n" +
		"To: x\n" +
		"\r\n" +
		".\r\n" +
		"\x00\x01\xff\r\n" +
		"no final newline"
	want := strings.Replace(in, "X-Postwerk-Id: forged\r\n", "", 1)
	res := Process([]byte(in))
	if !bytes.Equal(res.Message, []byte(want)) {
		t.Fatalf("message:\n%q\nwant\n%q", res.Message, want)
	}
}

func TestProcessNameVariants(t *testing.T) {
	in := "X-Postwerk-Ref : spaced\r\nx-PoStWeRk-rEf: second\r\nA: b\r\n\r\n"
	res := Process([]byte(in))
	if string(res.Message) != "A: b\r\n\r\n" {
		t.Fatalf("message = %q", res.Message)
	}
	if res.Ref != "spaced" {
		t.Errorf("Ref = %q, want the first one", res.Ref)
	}
}

func TestRefValidation(t *testing.T) {
	tests := []struct {
		value, want string
	}{
		{"  abc-123  ", "abc-123"},
		{"with space", "with space"},
		{strings.Repeat("a", 256), strings.Repeat("a", 256)},
		{strings.Repeat("a", 257), ""},
		{"user@example.test", ""},
		{"grüße", ""},
		{"tab\tinside", ""},
		{"", ""},
	}
	for _, tt := range tests {
		res := Process([]byte("X-Postwerk-Ref: " + tt.value + "\r\n\r\n"))
		if res.Ref != tt.want {
			t.Errorf("ref %q: got %q, want %q", tt.value, res.Ref, tt.want)
		}
	}
}

func TestMessageIDValidation(t *testing.T) {
	res := Process([]byte("Message-Id: <a@b>\r\nMessage-ID: <second@b>\r\n\r\n"))
	if res.MessageID != "<a@b>" {
		t.Errorf("MessageID = %q", res.MessageID)
	}
	res = Process([]byte("Message-ID: <" + strings.Repeat("a", 1000) + ">\r\n\r\n"))
	if res.MessageID != "" {
		t.Errorf("long MessageID kept")
	}
}

func TestWrite(t *testing.T) {
	var buf bytes.Buffer
	msg := []byte("Subject: x\r\n\r\nbody")
	err := Write(&buf, []Field{{HeaderID, "pw_1"}, {HeaderMailjetCustomID, "pw_1"}}, msg)
	if err != nil {
		t.Fatal(err)
	}
	want := "X-Postwerk-Id: pw_1\r\nX-MJ-CustomID: pw_1\r\nSubject: x\r\n\r\nbody"
	if buf.String() != want {
		t.Fatalf("got %q", buf.String())
	}
	if err := Write(&buf, []Field{{"A", "x\r\nB: y"}}, msg); err == nil {
		t.Fatal("header injection accepted")
	}
}

func TestEightBit(t *testing.T) {
	tests := []struct {
		msg           string
		any8, header8 bool
	}{
		{"Subject: hi\r\n\r\nbody\r\n", false, false},
		{"Subject: hi\r\n\r\nGrüße\r\n", true, false},
		{"Subject: Grüße\r\n\r\nbody\r\n", true, true},
		{"Subject: hi\n\nGrüße\n", true, false},
		{"Subject: hi\r\n Grüße folded\r\n\r\nbody", true, true},
		{"no header end Grüße", true, true},
		{"", false, false},
	}
	for _, tt := range tests {
		if got := Has8Bit([]byte(tt.msg)); got != tt.any8 {
			t.Errorf("Has8Bit(%q) = %v", tt.msg, got)
		}
		if got := HeaderHas8Bit([]byte(tt.msg)); got != tt.header8 {
			t.Errorf("HeaderHas8Bit(%q) = %v", tt.msg, got)
		}
	}
}

func TestSize(t *testing.T) {
	fields := []Field{{Name: "X-Postwerk-Id", Value: "pw_1"}, {Name: "X-MJ-CustomID", Value: "pw_1"}}
	msg := []byte("Subject: x\r\n\r\nbody\r\n")
	var buf bytes.Buffer
	if err := Write(&buf, fields, msg); err != nil {
		t.Fatal(err)
	}
	if got := Size(fields, msg); got != buf.Len() {
		t.Fatalf("Size = %d, Write wrote %d", got, buf.Len())
	}
}

// Size must match what the SMTP client's DATA writer really sends, for every
// mix of CR, LF and text (no dots, so no transparency dots are added).
func TestSizeMatchesDotWriter(t *testing.T) {
	fields := []Field{{Name: HeaderID, Value: "pw_01JD0000000000000000000000"}}
	alphabet := []byte("\r\na")
	check := func(msg []byte) {
		var buf bytes.Buffer
		tw := textproto.NewWriter(bufio.NewWriter(&buf))
		dw := tw.DotWriter()
		if err := Write(dw, fields, msg); err != nil {
			t.Fatal(err)
		}
		if err := dw.Close(); err != nil {
			t.Fatal(err)
		}
		// The writer ends with ".\r\n", which is not part of the message.
		if got, want := Size(fields, msg), buf.Len()-len(".\r\n"); got != want {
			t.Fatalf("Size(%q) = %d, the writer sent %d", msg, got, want)
		}
	}
	// Every message of up to 6 bytes from the alphabet.
	var gen func(prefix []byte, n int)
	gen = func(prefix []byte, n int) {
		check(prefix)
		if n == 0 {
			return
		}
		for _, c := range alphabet {
			gen(append(slices.Clone(prefix), c), n-1)
		}
	}
	gen(nil, 6)
	check([]byte("Subject: x\n\nbody\nwith LF only"))
	check([]byte("Subject: x\r\n\r\nbody\r\r\n\r\n"))
}

func TestFieldsSize(t *testing.T) {
	f := []Field{{Name: "X-A", Value: "1"}, {Name: "X-Bb", Value: "22"}}
	if got := FieldsSize(f); got != len("X-A: 1\r\nX-Bb: 22\r\n") {
		t.Fatalf("FieldsSize = %d", got)
	}
}
