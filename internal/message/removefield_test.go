package message

import "testing"

func TestRemoveField(t *testing.T) {
	in := "X-MJ-CustomID: a\r\nSubject: s\r\nx-mj-customid: b\r\n\tfolded\r\nX-MJ-CustomIDs: keep\r\n\r\nX-MJ-CustomID: in body\r\n"
	want := "Subject: s\r\nX-MJ-CustomIDs: keep\r\n\r\nX-MJ-CustomID: in body\r\n"
	if got := string(RemoveField([]byte(in), HeaderMailjetCustomID)); got != want {
		t.Fatalf("got %q\nwant %q", got, want)
	}
	plain := []byte("Subject: s\n\nbody\n")
	if got := RemoveField(plain, HeaderMailjetCustomID); &got[0] != &plain[0] {
		t.Fatal("unchanged message was copied")
	}
	if got := string(RemoveField([]byte("X-MJ-CustomID: only"), HeaderMailjetCustomID)); got != "" {
		t.Fatalf("header-only message: %q", got)
	}
}
