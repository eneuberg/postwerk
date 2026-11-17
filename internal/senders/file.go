package senders

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync/atomic"
	"time"

	"github.com/eneuberg/postwerk/internal/auth"
	"github.com/eneuberg/postwerk/internal/filewatch"
)

// The senders file (POSTWERK_SENDERS_FILE). One line per credential, plus an
// optional policy line per sender; empty lines and lines starting with "#"
// are ignored:
//
//	name:pbkdf2-sha256$<iterations>$<salt>$<key>    a password (every 1.0 line)
//	name:pwk-sha256$<id>$<digest>                   a key (postwerk gen-sender)
//	name:policy from=*@example.com max_per_minute=60 max_rcpt_per_hour=500
//
// A sender has one or two credentials (two during a rotation), at most one of
// them a password, and at most one policy line. A policy line needs a
// credential line for the same name.

const policyPrefix = "policy"

// parseFile reads a senders file. Errors name the line number but never quote
// the line, which may hold a secret. An empty file (no senders) is an error
// unless allowEmpty.
func parseFile(r io.Reader, allowEmpty bool) (*set, error) {
	s := newSet(KindFile)
	s.hashes = map[string]string{}
	policies := map[string]bool{}
	var errs []error
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 4096), 64*1024)
	lineNo := 0
	for sc.Scan() {
		lineNo++
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if err := s.parseLine(line, policies); err != nil {
			errs = append(errs, fmt.Errorf("line %d: %w", lineNo, err))
		}
	}
	if err := sc.Err(); err != nil {
		errs = append(errs, fmt.Errorf("read: %w", err))
	}
	for name, snd := range s.senders {
		if len(snd.Keys) == 0 && !snd.Password {
			errs = append(errs, fmt.Errorf("sender %q has a policy line but no password or key line", name))
		}
	}
	if len(errs) == 0 && len(s.senders) == 0 && !allowEmpty {
		errs = append(errs, errors.New("no senders defined"))
	}
	if len(errs) > 0 {
		return nil, errors.Join(errs...)
	}
	if len(s.hashes) == 0 {
		s.hashes = nil
	}
	if err := s.finish(); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *set) parseLine(line string, policies map[string]bool) error {
	name, value, ok := strings.Cut(line, ":")
	if !ok {
		return errors.New("expected name:hash, name:" + KeyScheme + "$… or name:policy …")
	}
	if !ValidName(name) {
		return errors.New("invalid sender name")
	}
	snd, exists := s.senders[name]
	if !exists {
		snd = Sender{Name: name, Source: KindFile}
	}
	credentials := len(snd.Keys)
	if snd.Password {
		credentials++
	}
	switch {
	case value == policyPrefix || strings.HasPrefix(value, policyPrefix+" "):
		if policies[name] {
			return fmt.Errorf("sender %q has two policy lines", name)
		}
		p, err := ParsePolicy(strings.TrimPrefix(value, policyPrefix))
		if err != nil {
			return err
		}
		policies[name] = true
		snd.Policy = p
	case strings.HasPrefix(value, auth.Scheme+"$"):
		if snd.Password {
			// The 1.0 wording for a name listed twice.
			return fmt.Errorf("sender %q listed twice", name)
		}
		if credentials >= MaxActiveKeys {
			return fmt.Errorf("sender %q has more than %d credentials", name, MaxActiveKeys)
		}
		if err := auth.CheckHash(value); err != nil {
			return err
		}
		snd.Password = true
		s.hashes[name] = value
	case strings.HasPrefix(value, KeyScheme+"$"):
		id, d, err := parseKeyHash(value)
		if err != nil {
			return err
		}
		if _, dup := s.keys[id]; dup {
			return fmt.Errorf("key id %s listed twice", id)
		}
		if credentials >= MaxActiveKeys {
			return fmt.Errorf("sender %q has more than %d credentials", name, MaxActiveKeys)
		}
		k := KeyInfo{ID: id}
		snd.Keys = append(snd.Keys, k)
		s.keys[id] = Credential{Sender: name, Source: KindFile, Key: k, digest: d}
	default:
		return fmt.Errorf("expected %s$…, %s$… or policy …", auth.Scheme, KeyScheme)
	}
	s.senders[name] = snd
	return nil
}

// FileOptions configure a FileSource.
type FileOptions struct {
	Path string
	// AllowEmpty accepts a file without senders (POSTWERK_SENDER_KEYS=both,
	// where the database may hold them all).
	AllowEmpty bool
	// Interval between checks for a change; 0 means filewatch's default.
	Interval time.Duration
	// OnEvent, if set, receives senders.reloaded and senders.reload_failed.
	OnEvent func(Event)
	// Settle: see filewatch.Options; tests shorten it.
	Settle time.Duration
}

// FileSource is the Source of the senders file. It re-reads the file every
// interval and swaps in a changed file only if it is valid and has no name
// or key id that another source of its Registry has; otherwise the last good
// file stays and senders.reload_failed is written once per content.
type FileSource struct {
	opts    FileOptions
	cur     atomic.Pointer[set]
	watcher *filewatch.Watcher
	guard   atomic.Pointer[guard]
}

var _ Source = (*FileSource)(nil)

// NewFileSource reads the file; a missing or invalid file is an error.
func NewFileSource(opts FileOptions) (*FileSource, error) {
	f := &FileSource{opts: opts}
	w, err := filewatch.New(filewatch.Options{
		Files: []string{opts.Path}, Interval: opts.Interval, Settle: opts.Settle,
		Load: f.load,
		Loaded: func() {
			s := f.cur.Load()
			f.emit(Event{Kind: KindReloaded, Level: "info", Text: fmt.Sprintf("senders file reloaded: senders=%d keys=%d passwords=%d",
				len(s.senders), len(s.keys), len(s.hashes))})
		},
		Failed: func(err error) {
			f.emit(Event{Kind: KindReloadFailed, Level: "error",
				Text: "senders file not reloaded, the previous one stays: " + oneLine(err)})
		},
	})
	if err != nil {
		return nil, err
	}
	f.watcher = w
	return f, nil
}

func (f *FileSource) load(contents [][]byte) error {
	s, err := parseFile(bytes.NewReader(contents[0]), f.opts.AllowEmpty)
	if err != nil {
		return err
	}
	if g := f.guard.Load(); g != nil {
		return g.swap(func() error {
			if err := g.check(f, s); err != nil {
				return err
			}
			f.cur.Store(s)
			return nil
		})
	}
	f.cur.Store(s)
	return nil
}

func (f *FileSource) emit(e Event) {
	if f.opts.OnEvent != nil {
		f.opts.OnEvent(e)
	}
}

// Run checks the file for changes until ctx ends.
func (f *FileSource) Run(ctx context.Context) { f.watcher.Run(ctx) }

// Check looks at the file once and reports whether a new one was loaded.
func (f *FileSource) Check(ctx context.Context) bool { return f.watcher.Check(ctx) }

func (f *FileSource) snapshot() *set                       { return f.cur.Load() }
func (f *FileSource) setGuard(g *guard)                    { f.guard.Store(g) }
func (f *FileSource) ByKeyID(id string) (Credential, bool) { return f.cur.Load().ByKeyID(id) }
func (f *FileSource) ByName(name string) (Sender, bool)    { return f.cur.Load().ByName(name) }
func (f *FileSource) List() []Sender                       { return f.cur.Load().List() }

// oneLine joins a multi-line error (errors.Join) for an event text.
func oneLine(err error) string { return strings.ReplaceAll(err.Error(), "\n", "; ") }

// CheckFile parses a senders file and returns its number of senders, for the
// configuration check at start. Errors name line numbers, never content.
func CheckFile(b []byte, allowEmpty bool) (int, error) {
	s, err := parseFile(bytes.NewReader(b), allowEmpty)
	if err != nil {
		return 0, err
	}
	return len(s.senders), nil
}
