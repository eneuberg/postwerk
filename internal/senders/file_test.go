package senders

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/eneuberg/postwerk/internal/auth"
)

var testHash = sync.OnceValue(func() string {
	h, err := auth.Hash("secret", auth.MinIterations)
	if err != nil {
		panic(err)
	}
	return h
})

// keyLine returns a new key and its file line for name.
func keyLine(t testing.TB, name string) (key, id, line string) {
	t.Helper()
	key, id = NewKey()
	line, err := KeyLine(name, key)
	if err != nil {
		t.Fatal(err)
	}
	return key, id, line
}

func TestParseFile(t *testing.T) {
	_, id1, k1 := keyLine(t, "app")
	_, _, k2 := keyLine(t, "app")
	_, _, k3 := keyLine(t, "app")
	_, _, other := keyLine(t, "other")
	pw := "app:" + testHash()
	for _, tc := range []struct {
		name    string
		file    string
		empty   bool
		wantErr string // "" for success
	}{
		{"1.0 file", "# senders\n\n" + pw + "\n", false, ""},
		{"key", k1 + "\n", false, ""},
		{"rotation: two keys", k1 + "\n" + k2 + "\n", false, ""},
		{"rotation: password and key", pw + "\n" + k1 + "\n", false, ""},
		{"policy", k1 + "\napp:policy from=*@example.com max_per_minute=10\n", false, ""},
		{"policy first", "app:policy max_per_minute=10\n" + k1 + "\n", false, ""},
		{"empty policy line", k1 + "\napp:policy\n", false, ""},
		{"three keys", k1 + "\n" + k2 + "\n" + k3 + "\n", false, "more than 2 credentials"},
		{"password twice", pw + "\n" + pw + "\n", false, `sender "app" listed twice`},
		{"same key twice", k1 + "\n" + strings.Replace(k1, "app:", "other:", 1) + "\n", false, "key id " + id1 + " listed twice"},
		{"policy twice", k1 + "\napp:policy\napp:policy\n", false, "two policy lines"},
		{"policy alone", "app:policy max_per_minute=1\n", false, "no password or key line"},
		{"bad policy", k1 + "\napp:policy speed=9\n", false, "line 2: invalid policy"},
		{"bad name", "App:" + testHash() + "\n", false, "invalid sender name"},
		{"no colon", "app\n", false, "expected name:hash"},
		{"unknown scheme", "app:sha256$abc\n", false, "expected pbkdf2-sha256$"},
		{"bad key hash", "app:pwk-sha256$short$AAAA\n", false, "12-character key id"},
		{"no senders", "# nothing\n", false, "no senders defined"},
		{"no senders allowed", "# nothing\n", true, ""},
		{"two senders", k1 + "\n" + other + "\n", false, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := CheckFile([]byte(tc.file), tc.empty)
			switch {
			case tc.wantErr == "" && err != nil:
				t.Fatalf("err = %v", err)
			case tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)):
				t.Fatalf("err = %v, want %q", err, tc.wantErr)
			}
			if err != nil && strings.Contains(err.Error(), "pbkdf2-sha256$") && strings.Contains(err.Error(), testHash()) {
				t.Fatal("error quotes a hash")
			}
		})
	}
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestFileSourceReload(t *testing.T) {
	path := filepath.Join(t.TempDir(), "senders")
	_, id1, k1 := keyLine(t, "app")
	_, id2, k2 := keyLine(t, "app")
	writeFile(t, path, k1+"\n")
	var mu sync.Mutex
	var events []Event
	f, err := NewFileSource(FileOptions{Path: path, Settle: time.Millisecond, OnEvent: func(e Event) {
		mu.Lock()
		events = append(events, e)
		mu.Unlock()
	}})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, ok := f.ByKeyID(id1); !ok {
		t.Fatal("key missing")
	}
	// Rotation: the second key is added, then the first removed.
	writeFile(t, path, k1+"\n"+k2+"\napp:policy max_per_minute=5\n")
	if !f.Check(ctx) {
		t.Fatal("change not loaded")
	}
	s, _ := f.ByName("app")
	if len(s.Keys) != 2 || s.Policy.MaxPerMinute() != 5 {
		t.Fatalf("after rotation: %+v", s)
	}
	writeFile(t, path, "this is broken\n")
	if f.Check(ctx) {
		t.Fatal("broken file loaded")
	}
	if _, ok := f.ByKeyID(id2); !ok {
		t.Fatal("broken file replaced the good one")
	}
	writeFile(t, path, k2+"\n")
	f.Check(ctx)
	if _, ok := f.ByKeyID(id1); ok {
		t.Fatal("removed key still there")
	}
	mu.Lock()
	defer mu.Unlock()
	if len(events) != 3 || events[0].Kind != KindReloaded || events[1].Kind != KindReloadFailed ||
		events[1].Level != "error" || events[2].Kind != KindReloaded {
		t.Fatalf("events %+v", events)
	}
	if strings.Contains(events[1].Text, "this is broken") || !strings.Contains(events[1].Text, "line 1") {
		t.Fatalf("failure text %q", events[1].Text)
	}
}
