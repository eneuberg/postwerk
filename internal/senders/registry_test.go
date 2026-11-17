package senders

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/eneuberg/postwerk/internal/store"
)

func openStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "pw.db"), store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

type fixture struct {
	path string
	file *FileSource
	db   *DBSource
	reg  *Registry
	st   *store.Store
}

// newFixture builds a file source with content, a database source and the
// registry over both.
func newFixture(t *testing.T, content string) *fixture {
	t.Helper()
	fx := &fixture{path: filepath.Join(t.TempDir(), "senders"), st: openStore(t)}
	writeFile(t, fx.path, content)
	var err error
	fx.file, err = NewFileSource(FileOptions{Path: fx.path, AllowEmpty: true, Settle: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	fx.db, err = NewDBSource(context.Background(), fx.st, nil)
	if err != nil {
		t.Fatal(err)
	}
	fx.reg, err = NewRegistry(fx.file, fx.db)
	if err != nil {
		t.Fatal(err)
	}
	return fx
}

func TestRegistryRefusesClashes(t *testing.T) {
	ctx := context.Background()
	_, _, line := keyLine(t, "app")
	fx := newFixture(t, line+"\n")
	// A database sender cannot take a file sender's name.
	if _, err := fx.db.Create(ctx, CreateRequest{Name: "app", Actor: "ops"}); !errors.Is(err, ErrNameTaken) {
		t.Fatalf("create over a file name: %v", err)
	}
	if _, err := fx.db.Create(ctx, CreateRequest{Name: "billing", Actor: "ops"}); err != nil {
		t.Fatal(err)
	}
	if _, err := fx.db.Create(ctx, CreateRequest{Name: "billing", Actor: "ops"}); !errors.Is(err, ErrNameTaken) {
		t.Fatalf("second create: %v", err)
	}
	// A file reload cannot take a database sender's name.
	_, _, billing := keyLine(t, "billing")
	writeFile(t, fx.path, line+"\n"+billing+"\n")
	if fx.file.Check(ctx) {
		t.Fatal("clashing file loaded")
	}
	if s, _ := fx.reg.ByName("billing"); s.Source != KindDB {
		t.Fatal("database sender replaced")
	}
	// Nor at start.
	if _, err := NewRegistry(mustFile(t, fx.path), fx.db); err == nil || !strings.Contains(err.Error(), "billing") {
		t.Fatalf("start with a clash: %v", err)
	}
	names := []string{}
	for _, s := range fx.reg.List() {
		names = append(names, string(s.Source)+":"+s.Name)
	}
	if strings.Join(names, ",") != "file:app,db:billing" {
		t.Fatalf("List = %v", names)
	}
	if !fx.reg.Has("app") || !fx.reg.Has("billing") || fx.reg.Has("nobody") {
		t.Fatal("Has")
	}
}

func mustFile(t *testing.T, path string) *FileSource {
	t.Helper()
	// Read the file as it is now, without the registry's check.
	f, err := NewFileSource(FileOptions{Path: path, AllowEmpty: true})
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func TestRegistryOnlyComposesOwnSources(t *testing.T) {
	if _, err := NewRegistry(fakeSource{}); err == nil {
		t.Fatal("foreign source accepted")
	}
	var f *FileSource
	r, err := NewRegistry(f, nil)
	if err != nil || len(r.List()) != 0 {
		t.Fatalf("nil sources: %v", err)
	}
}

type fakeSource struct{}

func (fakeSource) ByKeyID(string) (Credential, bool) { return Credential{}, false }
func (fakeSource) ByName(string) (Sender, bool)      { return Sender{}, false }
func (fakeSource) List() []Sender                    { return nil }

// Run with -race: reloads of the file and changes in the database while
// logins run never mix two states, and a name never lands in both sources.
func TestRegistryConcurrent(t *testing.T) {
	ctx := context.Background()
	keyA, _, lineA := keyLine(t, "app")
	_, _, lineB := keyLine(t, "app")
	_, _, shared := keyLine(t, "shared")
	fx := newFixture(t, lineA+"\n")
	a := NewAuthenticator(fx.reg, AuthOptions{})
	var wg sync.WaitGroup
	stop := make(chan struct{})
	var logins, failures atomic.Int32
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				if _, err := a.Authenticate("app", keyA); err == nil {
					logins.Add(1)
				} else {
					failures.Add(1)
				}
				fx.reg.List()
			}
		}()
	}
	var created atomic.Bool
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := range 30 {
			content := lineA + "\n"
			if i%2 == 1 {
				content += lineB + "\n" + shared + "\n"
			}
			// Replaced at once, as Kubernetes swaps a Secret's files.
			tmp := fx.path + ".tmp"
			_ = os.WriteFile(tmp, []byte(content), 0o600)
			_ = os.Rename(tmp, fx.path)
			fx.file.Check(ctx)
		}
	}()
	go func() {
		defer wg.Done()
		for range 30 {
			if _, err := fx.db.Create(ctx, CreateRequest{Name: "shared", Actor: "ops"}); err == nil {
				created.Store(true)
				return
			}
		}
	}()
	time.Sleep(200 * time.Millisecond)
	close(stop)
	wg.Wait()
	if logins.Load() == 0 || failures.Load() != 0 {
		t.Fatalf("logins %d failures %d: the key in every file version must always work", logins.Load(), failures.Load())
	}
	inFile := false
	if _, ok := fx.file.ByName("shared"); ok {
		inFile = true
	}
	_, inDB := fx.db.ByName("shared")
	if inFile && inDB {
		t.Fatal("shared is in both sources")
	}
}
