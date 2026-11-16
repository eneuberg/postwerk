// Package message handles the few header fields Postwerk reads, removes and
// adds (I8, D10). It does not parse or change anything else: every byte that
// is not part of an X-Postwerk-* field is kept exactly as received (P5).
package message

import (
	"bytes"
	"fmt"
	"io"
	"strings"
)

// Header names Postwerk reads or writes.
const (
	// HeaderID carries the Postwerk mail id towards every target.
	HeaderID = "X-Postwerk-Id"
	// HeaderRef is the sender's own reference for a mail.
	HeaderRef = "X-Postwerk-Ref"
	// HeaderSkip names targets that must not receive a mail.
	HeaderSkip = "X-Postwerk-Skip"
	// HeaderMailjetCustomID is Mailjet's tracking header.
	HeaderMailjetCustomID = "X-MJ-CustomID"
)

const (
	ownPrefix       = "x-postwerk-"
	maxRefLen       = 256
	maxMessageIDLen = 998
)

// Result is what Process found in a message.
type Result struct {
	// MessageID is the first Message-ID field, trimmed. It is empty if the
	// field is missing, longer than 998 bytes or not printable ASCII.
	MessageID string
	// Ref is the first X-Postwerk-Ref field, trimmed. It is empty if the
	// field is missing, longer than 256 bytes, not printable ASCII or
	// contains "@" (a reference must be an opaque id, not an address).
	Ref string
	// Skip holds the lower-cased names from all X-Postwerk-Skip fields,
	// without duplicates, in order of appearance.
	Skip []string
	// Message is the input without any X-Postwerk-* field.
	Message []byte
}

// Process reads Message-ID, X-Postwerk-Ref and X-Postwerk-Skip from the header
// section of raw and removes every X-Postwerk-* field, including its folded
// continuation lines. Lines may end in CRLF or LF. The header section ends at
// the first empty line; a message without one is all header.
func Process(raw []byte) Result {
	var res Result
	refSeen := false
	seenSkip := make(map[string]bool)
	res.Message = walkHeader(raw, func(name string, field []byte) bool {
		lower := strings.ToLower(name)
		value := unfold(field, len(name))
		switch {
		case strings.HasPrefix(lower, ownPrefix):
			switch lower {
			case "x-postwerk-ref":
				if !refSeen {
					refSeen = true
					res.Ref = cleanRef(value)
				}
			case "x-postwerk-skip":
				for _, s := range strings.Split(value, ",") {
					s = strings.ToLower(strings.TrimSpace(s))
					if s != "" && !seenSkip[s] {
						seenSkip[s] = true
						res.Skip = append(res.Skip, s)
					}
				}
			}
			return false // removed from the message
		case lower == "message-id" && res.MessageID == "":
			res.MessageID = cleanMessageID(value)
		}
		return true
	})
	return res
}

// RemoveField returns raw without any header field called name (compared
// case-insensitively), including folded continuation lines. Everything else
// is kept byte for byte. If there is no such field, raw itself is returned.
func RemoveField(raw []byte, name string) []byte {
	found := false
	out := walkHeader(raw, func(n string, _ []byte) bool {
		if strings.EqualFold(n, name) {
			found = true
			return false
		}
		return true
	})
	if !found {
		return raw
	}
	return out
}

// walkHeader calls keep for every header field of raw, with the field's name
// and its bytes including continuation lines and line ends, and returns raw
// without the fields for which keep returned false. Lines that are not
// fields and the body are kept as they are.
func walkHeader(raw []byte, keep func(name string, field []byte) bool) []byte {
	out := make([]byte, 0, len(raw))

	// field is the current header field: raw[start:end], with its name.
	var start, end int
	var name string
	inField := false

	flush := func() {
		if !inField {
			return
		}
		inField = false
		if keep(name, raw[start:end]) {
			out = append(out, raw[start:end]...)
		}
	}

	pos := 0
	for pos < len(raw) {
		lineEnd := bytes.IndexByte(raw[pos:], '\n')
		if lineEnd < 0 {
			lineEnd = len(raw)
		} else {
			lineEnd += pos + 1
		}
		line := raw[pos:lineEnd]

		if isEmptyLine(line) {
			flush()
			return append(out, raw[pos:]...)
		}
		if line[0] == ' ' || line[0] == '\t' {
			if inField {
				end = lineEnd // continuation of the current field
			} else {
				out = append(out, line...) // stray continuation, kept as is
			}
			pos = lineEnd
			continue
		}
		flush()
		if n, ok := fieldName(line); ok {
			start, end, name, inField = pos, lineEnd, n, true
		} else {
			out = append(out, line...) // not a field, kept as is
		}
		pos = lineEnd
	}
	flush()
	return out
}

// Field is a header field that Postwerk adds.
type Field struct {
	Name  string
	Value string
}

// Write writes fields, each ending in CRLF, followed by msg unchanged. Names
// and values must not contain line breaks.
func Write(w io.Writer, fields []Field, msg []byte) error {
	var buf bytes.Buffer
	for _, f := range fields {
		if strings.ContainsAny(f.Name, "\r\n:") || strings.ContainsAny(f.Value, "\r\n") {
			return fmt.Errorf("message: invalid header field %q", f.Name)
		}
		buf.WriteString(f.Name)
		buf.WriteString(": ")
		buf.WriteString(f.Value)
		buf.WriteString("\r\n")
	}
	if _, err := w.Write(buf.Bytes()); err != nil {
		return err
	}
	_, err := w.Write(msg)
	return err
}

// Size returns the size of fields and msg as a target receives them (I17):
// the SMTP client's DATA writer turns every LF without a CR before it into
// CRLF and ends a message that has no final line break with one. This is
// the size RFC 1870 means; the dots added for transparency are not counted.
func Size(fields []Field, msg []byte) int {
	return FieldsSize(fields) + WireSize(msg)
}

// FieldsSize returns the number of bytes of fields as Write writes them.
func FieldsSize(fields []Field) int {
	n := 0
	for _, f := range fields {
		n += len(f.Name) + len(": ") + len(f.Value) + len("\r\n")
	}
	return n
}

// WireSize returns the size of msg as a target receives it, written after
// complete header lines; see Size. It follows net/textproto's DotWriter: a
// CR only pairs with the LF right after it, and a CR that follows a CR
// starts no pair.
func WireSize(msg []byte) int {
	n := len(msg)
	cr := false // the previous byte was a CR that can pair with an LF
	for _, c := range msg {
		if c == '\n' && !cr {
			n++
		}
		cr = c == '\r' && !cr
	}
	switch {
	case cr:
		n++ // a final CR gets its LF
	case len(msg) > 0 && msg[len(msg)-1] != '\n':
		n += len("\r\n")
	}
	return n
}

func isEmptyLine(line []byte) bool {
	return len(line) == 1 && line[0] == '\n' || len(line) == 2 && line[0] == '\r' && line[1] == '\n'
}

// fieldName returns the name of a header line "Name: value". RFC 5322 field
// names are printable ASCII without ":"; white space between name and colon
// (obsolete syntax) is accepted and not part of the name.
func fieldName(line []byte) (string, bool) {
	colon := bytes.IndexByte(line, ':')
	if colon <= 0 {
		return "", false
	}
	n := bytes.TrimRight(line[:colon], " \t")
	if len(n) == 0 {
		return "", false
	}
	for _, c := range n {
		if c < 33 || c > 126 {
			return "", false
		}
	}
	return string(n), true
}

// unfold returns the value of a field: everything after the colon, with line
// breaks removed and surrounding white space trimmed.
func unfold(field []byte, nameLen int) string {
	colon := bytes.IndexByte(field[nameLen:], ':') + nameLen
	v := field[colon+1:]
	v = bytes.ReplaceAll(v, []byte("\r"), nil)
	v = bytes.ReplaceAll(v, []byte("\n"), nil)
	return strings.TrimSpace(string(v))
}

func cleanRef(v string) string {
	if len(v) > maxRefLen || !printable(v) || strings.Contains(v, "@") {
		return ""
	}
	return v
}

func cleanMessageID(v string) string {
	if len(v) > maxMessageIDLen || !printable(v) {
		return ""
	}
	return v
}

// printable reports whether s is printable ASCII (space to tilde).
func printable(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] < 0x20 || s[i] > 0x7e {
			return false
		}
	}
	return true
}

// Has8Bit reports whether msg contains a byte outside 7-bit ASCII anywhere.
// Such a message needs a target that offers 8BITMIME.
func Has8Bit(msg []byte) bool { return !isASCII(msg) }

// HeaderHas8Bit reports whether the header section of msg (everything before
// the first empty line) contains a byte outside 7-bit ASCII: UTF-8 in header
// fields, which needs SMTPUTF8 (RFC 6532).
func HeaderHas8Bit(msg []byte) bool {
	pos := 0
	for pos < len(msg) {
		lineEnd := bytes.IndexByte(msg[pos:], '\n')
		if lineEnd < 0 {
			lineEnd = len(msg)
		} else {
			lineEnd += pos + 1
		}
		line := msg[pos:lineEnd]
		if isEmptyLine(line) {
			return false
		}
		if !isASCII(line) {
			return true
		}
		pos = lineEnd
	}
	return false
}

func isASCII(b []byte) bool {
	for _, c := range b {
		if c >= 0x80 {
			return false
		}
	}
	return true
}
