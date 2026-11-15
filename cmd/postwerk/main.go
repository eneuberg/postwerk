// Command postwerk is a thin, audited relay for outgoing mail.
//
// Usage:
//
//	postwerk [serve]        run the relay (configuration from POSTWERK_* variables)
//	postwerk hash-password  read a password from stdin, print a senders-file hash
//	postwerk gen-key        print a new key line for a store or hash key file
//	postwerk gen-token NAME ROLE  print a new UI token and its tokens-file line
//	postwerk version        print the version
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/eneuberg/postwerk/internal/addrhash"
	"github.com/eneuberg/postwerk/internal/auth"
	"github.com/eneuberg/postwerk/internal/config"
	"github.com/eneuberg/postwerk/internal/delivery"
	"github.com/eneuberg/postwerk/internal/httpapi"
	"github.com/eneuberg/postwerk/internal/ingress"
	"github.com/eneuberg/postwerk/internal/keys"
	"github.com/eneuberg/postwerk/internal/query"
	"github.com/eneuberg/postwerk/internal/report"
	"github.com/eneuberg/postwerk/internal/retention"
	"github.com/eneuberg/postwerk/internal/seal"
	"github.com/eneuberg/postwerk/internal/store"
	"github.com/eneuberg/postwerk/internal/ui"
)

// version is set at build time with -ldflags "-X main.version=…".
var version = "dev"

const usage = `usage: postwerk [command]

commands:
  serve          run the relay (default); configured by POSTWERK_* environment variables
  hash-password  read one password from stdin and print the value for the senders file
  gen-key        print a new random key line (<key-id>:<base64>) for POSTWERK_STORE_KEY_FILE
                 or POSTWERK_HASH_KEY_FILE; use a separate key for each file
  gen-token NAME ROLE
                 print a new random UI token (first line, give it to the person) and its line
                 for POSTWERK_UI_TOKENS_FILE (second line); ROLE is read or admin
  version        print the version
`

func main() {
	os.Exit(run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}

func run(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	cmd := "serve"
	if len(args) > 0 {
		cmd, args = args[0], args[1:]
	}
	if cmd == "gen-token" {
		return genToken(args, stdout, stderr)
	}
	if len(args) > 0 {
		fmt.Fprint(stderr, usage)
		return 2
	}
	switch cmd {
	case "serve":
		return serveCmd(stdout, stderr)
	case "hash-password":
		pw, err := auth.ReadPassword(stdin)
		if err != nil {
			fmt.Fprintln(stderr, "postwerk:", err)
			return 1
		}
		h, err := auth.Hash(pw, auth.DefaultIterations)
		if err != nil {
			fmt.Fprintln(stderr, "postwerk:", err)
			return 1
		}
		fmt.Fprintln(stdout, h)
		return 0
	case "gen-key":
		fmt.Fprintln(stdout, keys.Generate(time.Now()))
		return 0
	case "version":
		fmt.Fprintln(stdout, "postwerk", version)
		return 0
	case "help", "-h", "--help":
		fmt.Fprint(stdout, usage)
		return 0
	default:
		fmt.Fprint(stderr, usage)
		return 2
	}
}

// genToken prints a new UI token and the matching tokens-file line (U4).
func genToken(args []string, stdout, stderr io.Writer) int {
	if len(args) != 2 {
		fmt.Fprint(stderr, usage)
		return 2
	}
	token := auth.GenerateToken()
	line, err := auth.TokenLine(args[0], auth.Role(args[1]), token, auth.DefaultIterations)
	if err != nil {
		fmt.Fprintln(stderr, "postwerk:", err)
		return 2
	}
	fmt.Fprintln(stdout, token)
	fmt.Fprintln(stdout, line)
	return 0
}

func serveCmd(stdout, stderr io.Writer) int {
	cfg, err := config.Load(os.Environ(), readSecretFile)
	if err != nil {
		fmt.Fprintln(stderr, "postwerk: invalid configuration:")
		for _, line := range strings.Split(err.Error(), "\n") {
			fmt.Fprintln(stderr, "  -", line)
		}
		return 2
	}
	out := &lockedWriter{w: stdout}
	logger := slog.New(slog.NewJSONHandler(out, nil)).With("instance", cfg.Instance)
	for _, w := range cfg.Warnings() {
		logger.Warn(w)
	}
	warnFilePermissions(cfg.SecretFiles, logger)
	if cfg.ShutdownTimeout <= delivery.DefaultAttemptTimeout {
		logger.Warn("POSTWERK_SHUTDOWN_TIMEOUT is not longer than one delivery attempt; "+
			"a send in progress at shutdown can be cut and becomes an unclear outcome",
			"shutdown_timeout", cfg.ShutdownTimeout.String(), "attempt_timeout", delivery.DefaultAttemptTimeout.String())
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if err := serve(ctx, cfg, logger, out, nil); err != nil {
		logger.Error("postwerk stopped with an error", "err", err)
		return 1
	}
	return 0
}

// warnFilePermissions warns about secret files that others can read, or
// that a group can read which the process is not a member of. It does not
// refuse them: Kubernetes secret volumes often show 0644 through their
// symlinks, and refusing would push operators to weaker workarounds. A
// group-readable file owned by one of the process's own groups (0440 with
// fsGroup in Kubernetes) is fine.
func warnFilePermissions(files map[string]string, logger *slog.Logger) {
	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	slices.Sort(names)
	for _, name := range names {
		fi, err := os.Stat(files[name])
		if err != nil {
			continue // reading it worked moments ago; nothing to add
		}
		perm := fi.Mode().Perm()
		if perm&0o007 != 0 || (perm&0o070 != 0 && !ownGroup(fi)) {
			logger.Warn("secret file is readable by others; restrict it to the postwerk user (0400 or 0600)",
				"variable", name, "mode", fmt.Sprintf("%04o", perm))
		}
	}
}

// readSecretFile reads a secret file, at most 1 MiB and one byte (so that the
// config package can report an oversized file).
func readSecretFile(name string) ([]byte, error) {
	f, err := os.Open(name)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return io.ReadAll(io.LimitReader(f, 1<<20+1))
}

// lockedWriter serializes writes, so log lines and event lines on stdout
// never interleave.
type lockedWriter struct {
	mu sync.Mutex
	w  io.Writer
}

func (l *lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(p)
}

// serve runs Postwerk until ctx ends, then shuts down: ingress and workers
// stop together (workers after their current send), then the final event
// export, HTTP and the store. All of it shares one deadline,
// cfg.ShutdownTimeout. listening, if not nil, is called with the bound
// addresses once the listeners are open; adminAddr is nil when the admin
// listener is off.
func serve(ctx context.Context, cfg *config.Config, logger *slog.Logger, eventOut io.Writer,
	listening func(smtpAddr, httpAddr, adminAddr net.Addr)) error {
	sealer, err := seal.New(cfg.StoreKeys)
	if err != nil {
		return err
	}
	var rep *report.Reporter
	st, err := store.Open(ctx, cfg.DataPath, store.Options{
		OnEvent:           func() { rep.Wake() },
		PurgeRawOnHandoff: cfg.RawKeepAfterHandoff == 0,
		InsecureDelete:    !cfg.SecureDelete,
	})
	if err != nil {
		return err
	}
	rep = report.New(report.Options{
		Store: st, Sinks: []report.Sink{report.NewStdout(eventOut, cfg.Instance)}, Logger: logger,
	})

	warnUnconfiguredTargets(ctx, st, cfg.Targets, logger)

	mgr := delivery.New(delivery.Options{
		Store: st, Sealer: sealer, Targets: cfg.Targets, Logger: logger, HeloName: cfg.Hostname,
	})
	// Deliveries left in sending by a crash are recovered first, so that the
	// first retention run sees them as queued or unknown. Each worker retries
	// the recovery itself if this fails.
	if err := mgr.Recover(ctx); err != nil {
		logger.Error("recovery of interrupted deliveries failed; the workers retry it", "err", err)
	}
	ret := retention.New(retention.Options{Store: st, Policy: retainPolicy(cfg), Logger: logger})
	// The first run happens before any delivery, so nothing past its
	// maximum age is sent after a long downtime.
	_, _ = ret.Once(ctx)
	ing := ingress.New(ingress.Options{
		Store: st, Sealer: sealer, Senders: cfg.Senders, Targets: cfg.Targets, Hostname: cfg.Hostname,
		TLSCertificate: cfg.TLSCertificate, AllowInsecure: cfg.AllowInsecureAuth,
		MaxMessageBytes: cfg.MaxMessageBytes, MaxRecipients: cfg.MaxRecipients,
		MaxConnections: cfg.MaxConnections, MaxInflightBytes: cfg.MaxInflightBytes, Wake: mgr.Wake, Logger: logger,
	})
	httpSrv := httpapi.New(cfg.HTTPListen, func(ctx context.Context) error {
		if err := st.Ping(ctx); err != nil {
			return err
		}
		if !mgr.Running() {
			return errors.New("delivery workers not running")
		}
		return nil
	})

	smtpL, err := net.Listen("tcp", cfg.SMTPListen)
	if err != nil {
		st.Close()
		return fmt.Errorf("smtp listener: %w", err)
	}
	httpL, err := net.Listen("tcp", cfg.HTTPListen)
	if err != nil {
		smtpL.Close()
		st.Close()
		return fmt.Errorf("http listener: %w", err)
	}
	var adminSrv *http.Server
	var adminL net.Listener
	if cfg.Admin.Listen != "" {
		var names []string
		for _, t := range cfg.Targets {
			names = append(names, t.Name)
		}
		q := query.New(query.Options{Store: st, Hasher: hasher(cfg), Targets: names})
		u, err := ui.New(ui.Options{
			Query: q, Tokens: cfg.Admin.Tokens, Events: st, Hosts: cfg.Admin.Hosts, Instance: cfg.Instance,
			Logger: logger,
		})
		if err == nil {
			adminL, err = net.Listen("tcp", cfg.Admin.Listen)
		}
		if err != nil {
			httpL.Close()
			smtpL.Close()
			st.Close()
			return fmt.Errorf("admin listener: %w", err)
		}
		adminSrv = ui.NewServer(cfg.Admin.Listen, u, logger)
	}

	runCtx, cancelRun := context.WithCancel(context.WithoutCancel(ctx))
	defer cancelRun()
	repDone := make(chan struct{})
	go func() { rep.Run(runCtx); close(repDone) }()
	retDone := make(chan struct{})
	go func() { ret.Run(runCtx); close(retDone) }()
	mgr.Start(runCtx)

	serveErr := make(chan error, 3)
	go func() { serveErr <- ing.Serve(smtpL) }()
	go func() {
		if err := httpSrv.Serve(httpL); !errors.Is(err, http.ErrServerClosed) {
			serveErr <- err
		}
	}()
	var adminAddr net.Addr
	adminLog := "off"
	if adminSrv != nil {
		adminAddr = adminL.Addr()
		adminLog = adminAddr.String()
		go func() {
			if err := adminSrv.Serve(adminL); !errors.Is(err, http.ErrServerClosed) {
				serveErr <- err
			}
		}()
	}

	var names []string
	for _, t := range cfg.Targets {
		names = append(names, t.Name+"("+string(t.Scheme)+")")
	}
	logger.Info("postwerk started", "version", version, "smtp", smtpL.Addr().String(),
		"http", httpL.Addr().String(), "admin", adminLog, "targets", strings.Join(names, ","),
		"smtp_tls", cfg.TLSCertificate != nil, "insecure_auth", cfg.AllowInsecureAuth,
		"store_encryption", sealer.Encrypting(), "pseudonymize_after", cfg.PseudonymizeAfter.String())
	if listening != nil {
		listening(smtpL.Addr(), httpL.Addr(), adminAddr)
	}

	var runErr error
	select {
	case <-ctx.Done():
		logger.Info("shutting down", "timeout", cfg.ShutdownTimeout.String())
	case runErr = <-serveErr:
		logger.Error("listener failed, shutting down", "err", runErr)
	}

	sd, cancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
	defer cancel()
	// Workers stop claiming at once and finish their current send, which
	// takes at most delivery.DefaultAttemptTimeout; meanwhile open SMTP
	// sessions get at most half of the deadline. Mails accepted in that
	// time stay queued for the next start.
	cancelRun()
	ingDone := make(chan struct{})
	go func() {
		defer close(ingDone)
		ingCtx, ingCancel := context.WithTimeout(sd, cfg.ShutdownTimeout/2)
		defer ingCancel()
		if err := ing.Shutdown(ingCtx); err != nil {
			logger.Warn("smtp sessions still open at shutdown were closed", "err", err)
		}
	}()
	if err := mgr.Stop(sd); err != nil {
		logger.Error("delivery workers did not stop in time; interrupted sends are recovered on the next start", "err", err)
	}
	<-ingDone
	for _, done := range []chan struct{}{repDone, retDone} {
		select {
		case <-done:
		case <-sd.Done():
		}
	}
	if err := rep.Flush(sd); err != nil {
		logger.Warn("final event export incomplete; it resumes on the next start", "err", err)
	}
	if err := httpSrv.Shutdown(sd); err != nil {
		logger.Warn("http shutdown incomplete", "err", err)
	}
	if adminSrv != nil {
		if err := adminSrv.Shutdown(sd); err != nil {
			logger.Warn("admin listener shutdown incomplete", "err", err)
		}
	}
	if err := st.Close(); err != nil {
		logger.Warn("closing the store failed", "err", err)
	}
	logger.Info("postwerk stopped")
	return runErr
}

// retainPolicy builds the store's retention policy from the configuration.
func retainPolicy(cfg *config.Config) store.RetainPolicy {
	p := store.RetainPolicy{
		RawKeepAfterHandoff: cfg.RawKeepAfterHandoff,
		RawKeepAfterFailure: cfg.RawKeepAfterFailure,
		RawMaxAge:           cfg.RawMaxAge,
		PseudonymizeAfter:   cfg.PseudonymizeAfter,
		RecordRetention:     cfg.RecordRetention,
		EventRetention:      cfg.EventRetention,
		Optional:            make(map[string]bool),
	}
	if cfg.PseudonymizeAfter > 0 && cfg.HashKeys != nil {
		p.Pseudonymize = hasher(cfg).Hash
	}
	for _, t := range cfg.Targets {
		p.Optional[t.Name] = t.Optional
	}
	return p
}

// hasher returns the address hasher, or nil without a hash key file.
func hasher(cfg *config.Config) *addrhash.Hasher {
	if cfg.HashKeys == nil {
		return nil
	}
	return addrhash.New(cfg.HashKeys)
}

// warnUnconfiguredTargets logs pending deliveries for targets that are no
// longer configured; they stay visible but nobody works on them.
func warnUnconfiguredTargets(ctx context.Context, st *store.Store, targets []config.Target, logger *slog.Logger) {
	pending, err := st.PendingByTarget(ctx)
	if err != nil {
		logger.Warn("could not count pending deliveries", "err", err)
		return
	}
	known := make(map[string]bool)
	for _, t := range targets {
		known[t.Name] = true
	}
	for name, n := range pending {
		if !known[name] {
			logger.Warn("pending deliveries for a target that is not configured", "target", name, "count", n)
		}
	}
}
