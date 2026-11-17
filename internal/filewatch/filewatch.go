// Package filewatch notices when files change and hands their new content to
// a loader, with the rules internal/tlsreload follows for certificates (I16):
//
//   - The files are read at a fixed interval and compared by content (a
//     SHA-256 over all of them), not by modification time: Kubernetes swaps
//     the files of a Secret volume with a symlink change that keeps the times.
//   - Changed content goes to Load. If Load refuses it, the files are read once
//     more after Settle (a read may have fallen between two files of a swap);
//     content that is still refused is reported once, through Failed, and the
//     caller keeps what it had. The same broken content is not reported again.
//   - Content that Load accepted is reported through Loaded.
//
// The watcher holds no parsed state itself; the caller's Load swaps it in.
package filewatch

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"sync"
	"time"
)

// Defaults.
const (
	DefaultInterval = 30 * time.Second
	DefaultSettle   = 2 * time.Second
	// DefaultMaxBytes bounds what is read from each file, like every secret
	// file Postwerk reads at start.
	DefaultMaxBytes = 1 << 20
)

// Options configure a Watcher.
type Options struct {
	// Files are read in this order and passed to Load in the same order.
	Files []string
	// Load parses the content and, if it is valid, makes it current. An
	// error keeps what was current.
	Load func(contents [][]byte) error
	// Loaded, if set, is called after Load accepted changed content.
	Loaded func()
	// Failed, if set, is called once per refused content.
	Failed func(err error)
	// Interval between checks; 0 means DefaultInterval.
	Interval time.Duration
	// Settle before the second read of refused content; 0 means
	// DefaultSettle.
	Settle time.Duration
	// MaxBytes per file; 0 means DefaultMaxBytes.
	MaxBytes int64
}

// Watcher checks the files. It is safe for concurrent use; checks are
// serialized.
type Watcher struct {
	opts Options

	mu        sync.Mutex
	current   [sha256.Size]byte // content Load accepted last
	failed    [sha256.Size]byte // content reported as refused last
	hasFailed bool
}

// New reads the files and passes their content to Load once. It fails if
// they cannot be read or Load refuses them, so a broken file stops the start.
func New(opts Options) (*Watcher, error) {
	if len(opts.Files) == 0 || opts.Load == nil {
		return nil, errors.New("filewatch: Files and Load are required")
	}
	if opts.Interval <= 0 {
		opts.Interval = DefaultInterval
	}
	if opts.Settle <= 0 {
		opts.Settle = DefaultSettle
	}
	if opts.MaxBytes <= 0 {
		opts.MaxBytes = DefaultMaxBytes
	}
	w := &Watcher{opts: opts}
	contents, digest, err := w.read()
	if err != nil {
		return nil, err
	}
	if err := opts.Load(contents); err != nil {
		return nil, err
	}
	w.current = digest
	return w, nil
}

// Run checks every Interval until ctx ends.
func (w *Watcher) Run(ctx context.Context) {
	t := time.NewTicker(w.opts.Interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			w.Check(ctx)
		}
	}
}

// Check reads the files once and reports whether new content was loaded.
func (w *Watcher) Check(ctx context.Context) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	contents, digest, err := w.read()
	if err == nil && digest == w.current {
		return false
	}
	if err == nil {
		err = w.opts.Load(contents)
	}
	if err != nil {
		select {
		case <-ctx.Done():
			return false
		case <-time.After(w.opts.Settle):
		}
		contents, digest, err = w.read()
		if err == nil && digest == w.current {
			return false
		}
		if err == nil {
			err = w.opts.Load(contents)
		}
	}
	if err != nil {
		if !w.hasFailed || digest != w.failed {
			w.hasFailed, w.failed = true, digest
			if w.opts.Failed != nil {
				w.opts.Failed(err)
			}
		}
		return false
	}
	w.current, w.hasFailed = digest, false
	if w.opts.Loaded != nil {
		w.opts.Loaded()
	}
	return true
}

// read reads every file. The digest covers what was read even when a file
// is missing, so a missing file is reported once, not every interval.
func (w *Watcher) read() ([][]byte, [sha256.Size]byte, error) {
	var digest [sha256.Size]byte
	h := sha256.New()
	contents := make([][]byte, 0, len(w.opts.Files))
	var firstErr error
	for _, name := range w.opts.Files {
		b, err := readFile(name, w.opts.MaxBytes)
		if err != nil {
			h.Write([]byte("error: " + err.Error()))
			if firstErr == nil {
				firstErr = fmt.Errorf("%s: %w", name, err)
			}
		}
		h.Write(b)
		h.Write([]byte{0})
		contents = append(contents, b)
	}
	copy(digest[:], h.Sum(nil))
	return contents, digest, firstErr
}

func readFile(name string, maxBytes int64) ([]byte, error) {
	f, err := os.Open(name)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, maxBytes+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > maxBytes {
		return nil, fmt.Errorf("larger than %d bytes", maxBytes)
	}
	return b, nil
}
