//go:build linux || darwin || freebsd || netbsd || openbsd || dragonfly

package main

import (
	"context"
	"errors"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/eneuberg/postwerk/internal/auth"
	"github.com/eneuberg/postwerk/internal/config"
	"github.com/eneuberg/postwerk/internal/keys"
	"github.com/eneuberg/postwerk/internal/store"
	"github.com/eneuberg/postwerk/internal/testutil"
)

// lockConfig is a configuration on dbPath with the health listener on a
// fixed free port, so it can be asked before serve reports its addresses.
func lockConfig(t *testing.T, dbPath, lockWait string) (*config.Config, string) {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	httpAddr := l.Addr().String()
	l.Close()
	h, err := auth.Hash("secret", auth.MinIterations)
	if err != nil {
		t.Fatal(err)
	}
	files := map[string]string{
		"/senders":   "app:" + h + "\n",
		"/store-key": keys.Generate(time.Now()) + "\n",
	}
	cfg, err := config.Load([]string{
		"POSTWERK_SMTP_LISTEN=127.0.0.1:0",
		"POSTWERK_HTTP_LISTEN=" + httpAddr,
		"POSTWERK_ADMIN_LISTEN=off",
		"POSTWERK_DATA=" + dbPath,
		"POSTWERK_DATA_LOCK_WAIT=" + lockWait,
		"POSTWERK_SENDERS_FILE=" + realFile(t, files["/senders"]),
		"POSTWERK_STORE_KEY_FILE=/store-key",
		"POSTWERK_PSEUDONYMIZE_AFTER=0",
		"POSTWERK_SMTP_ALLOW_INSECURE_AUTH=true",
		"POSTWERK_SHUTDOWN_TIMEOUT=5s",
		"POSTWERK_TARGETS=capture",
		"POSTWERK_TARGET_CAPTURE_URL=smtp+plain://127.0.0.1:1",
	}, func(name string) ([]byte, error) { return readTestFile(files, name) })
	if err != nil {
		t.Fatal(err)
	}
	return cfg, httpAddr
}

func status(t *testing.T, addr, path string) int {
	t.Helper()
	resp, err := http.Get("http://" + addr + path)
	if err != nil {
		return 0
	}
	resp.Body.Close()
	return resp.StatusCode
}

// While another process holds the database, Postwerk waits: /healthz
// answers, /readyz does not, SMTP is closed. When the lock is let go, it
// starts and becomes ready (D61).
func TestServeWaitsForDataLock(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "pw.db")
	held, err := store.Open(context.Background(), dbPath, store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()
	cfg, httpAddr := lockConfig(t, dbPath, "30s")

	var out testutil.SafeBuffer
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	started := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- serve(ctx, cfg, testutil.Logger(&out), &out, func(net.Addr, net.Addr, net.Addr, net.Addr) { close(started) })
	}()
	testutil.Eventually(t, 5*time.Second, "waiting log", func() bool {
		return strings.Contains(out.String(), "database in use by another process")
	})
	if !strings.Contains(out.String(), `pid=`) {
		t.Fatalf("log without the holder: %s", out.String())
	}
	for range 3 {
		if h, r := status(t, httpAddr, "/healthz"), status(t, httpAddr, "/readyz"); h != 200 || r != 503 {
			t.Fatalf("while waiting: healthz %d, readyz %d", h, r)
		}
		time.Sleep(100 * time.Millisecond)
	}
	select {
	case <-started:
		t.Fatal("started while the database was locked")
	case err := <-done:
		t.Fatalf("serve returned while waiting: %v", err)
	default:
	}

	held.Close()
	select {
	case <-started:
	case err := <-done:
		t.Fatalf("serve returned: %v", err)
	case <-time.After(10 * time.Second):
		t.Fatal("did not start after the lock was released")
	}
	if r := status(t, httpAddr, "/readyz"); r != 200 {
		t.Fatalf("readyz after start = %d", r)
	}
	// While it runs, nobody else gets the database.
	if _, err := store.Open(context.Background(), dbPath, store.Options{}); !errors.Is(err, store.ErrLocked) {
		t.Fatalf("second open while serving: %v", err)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	// Stopped: the lock is free.
	st, err := store.Open(context.Background(), dbPath, store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	st.Close()
}

// When the lock is not let go in time, serve fails with ErrLocked.
func TestServeDataLockTimeout(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "pw.db")
	held, err := store.Open(context.Background(), dbPath, store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()
	cfg, _ := lockConfig(t, dbPath, "500ms")
	var out testutil.SafeBuffer
	err = serve(context.Background(), cfg, testutil.Logger(&out), &out, nil)
	if !errors.Is(err, store.ErrLocked) || !strings.Contains(err.Error(), "POSTWERK_DATA_LOCK_WAIT") {
		t.Fatalf("serve = %v", err)
	}
}

// A stop signal while waiting ends the wait without an error.
func TestServeStopWhileWaitingForDataLock(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "pw.db")
	held, err := store.Open(context.Background(), dbPath, store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()
	cfg, _ := lockConfig(t, dbPath, "1m")
	var out testutil.SafeBuffer
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- serve(ctx, cfg, testutil.Logger(&out), &out, nil) }()
	testutil.Eventually(t, 5*time.Second, "waiting log", func() bool {
		return strings.Contains(out.String(), "database in use by another process")
	})
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("serve did not stop")
	}
	if !strings.Contains(out.String(), "stopped while waiting for the database") {
		t.Fatalf("log: %s", out.String())
	}
}
