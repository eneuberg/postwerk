// Command postwerk is a thin, audited relay for outgoing mail.
//
// Usage:
//
//	postwerk [serve]        run the relay (configuration from POSTWERK_* variables)
//	postwerk hash-password  read a password from stdin, print a senders-file hash
//	postwerk gen-sender NAME  print a new sender key and its senders-file line
//	postwerk gen-key        print a new key line for a store or hash key file
//	postwerk gen-token NAME  print a new UI token and its tokens-file line
//	postwerk gen-api-token NAME ROLE  print a new API bearer token and its tokens-file line
//	postwerk version        print the version
package main

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/eneuberg/postwerk/internal/addrhash"
	"github.com/eneuberg/postwerk/internal/api"
	"github.com/eneuberg/postwerk/internal/auth"
	"github.com/eneuberg/postwerk/internal/config"
	"github.com/eneuberg/postwerk/internal/delivery"
	"github.com/eneuberg/postwerk/internal/httpapi"
	"github.com/eneuberg/postwerk/internal/ingress"
	"github.com/eneuberg/postwerk/internal/keys"
	"github.com/eneuberg/postwerk/internal/query"
	"github.com/eneuberg/postwerk/internal/report"
	"github.com/eneuberg/postwerk/internal/resolve"
	"github.com/eneuberg/postwerk/internal/retention"
	"github.com/eneuberg/postwerk/internal/seal"
	"github.com/eneuberg/postwerk/internal/senders"
	"github.com/eneuberg/postwerk/internal/senders/limit"
	"github.com/eneuberg/postwerk/internal/store"
	"github.com/eneuberg/postwerk/internal/tlsreload"
	"github.com/eneuberg/postwerk/internal/ui"
	"github.com/eneuberg/postwerk/internal/watch"
)

// finalExportTimeout bounds the event export at shutdown.
const finalExportTimeout = 10 * time.Second

// version is set at build time with -ldflags "-X main.version=…".
var version = "dev"

const usage = `usage: postwerk [command]

commands:
  serve          run the relay (default); configured by POSTWERK_* environment variables
  hash-password  read one password from stdin and print the value for the senders file
  gen-sender NAME
                 print a new random sender key (first line, the SMTP password of the application)
                 and its line for POSTWERK_SENDERS_FILE (second line); the user name is NAME
  gen-key        print a new random key line (<key-id>:<base64>) for POSTWERK_STORE_KEY_FILE
                 or POSTWERK_HASH_KEY_FILE; use a separate key for each file
  gen-token NAME [admin]
                 print a new random UI token (first line, give it to the person) and its line
                 for POSTWERK_UI_TOKENS_FILE (second line); every UI account is an admin
  gen-api-token NAME ROLE
                 print a new random API bearer token (first line, for the calling application)
                 and its line for POSTWERK_API_TOKENS_FILE (second line); ROLE is read or admin
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
		return genToken(args, stdout, stderr, false)
	}
	if cmd == "gen-api-token" {
		return genToken(args, stdout, stderr, true)
	}
	if cmd == "gen-sender" {
		return genSender(args, stdout, stderr)
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
// A UI token is always admin; "admin" may still be given, as in older
// guides. For the API (H5) the role is required and the first line is the
// whole bearer value, <name>.<token>.
func genToken(args []string, stdout, stderr io.Writer, forAPI bool) int {
	if !forAPI && len(args) == 2 && args[1] != string(auth.RoleAdmin) {
		fmt.Fprintln(stderr, "postwerk: the UI has admin accounts only; use 'postwerk gen-token NAME'")
		return 2
	}
	if !forAPI && len(args) == 1 {
		args = append(args, string(auth.RoleAdmin))
	}
	if len(args) != 2 {
		fmt.Fprint(stderr, usage)
		return 2
	}
	token := auth.GenerateToken()
	var line string
	var err error
	if forAPI {
		line, err = auth.APITokenLine(args[0], auth.Role(args[1]), token)
	} else {
		line, err = auth.TokenLine(args[0], auth.Role(args[1]), token, auth.DefaultIterations)
	}
	if err != nil {
		fmt.Fprintln(stderr, "postwerk:", err)
		return 2
	}
	if forAPI {
		token = api.BearerToken(args[0], token)
	}
	fmt.Fprintln(stdout, token)
	fmt.Fprintln(stdout, line)
	return 0
}

// genSender prints a new sender key and its senders-file line (D62). The
// file keeps only the SHA-256 of the key's secret part.
func genSender(args []string, stdout, stderr io.Writer) int {
	if len(args) != 1 {
		fmt.Fprint(stderr, usage)
		return 2
	}
	key, _ := senders.NewKey()
	line, err := senders.KeyLine(args[0], key)
	if err != nil {
		fmt.Fprintln(stderr, "postwerk: invalid sender name (use [a-z0-9][a-z0-9._-]{0,63})")
		return 2
	}
	fmt.Fprintln(stdout, key)
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
// addresses once the listeners are open; adminAddr and apiAddr are nil when
// the admin listener or the API is off.
func serve(ctx context.Context, cfg *config.Config, logger *slog.Logger, eventOut io.Writer,
	listening func(smtpAddr, httpAddr, adminAddr, apiAddr net.Addr)) error {
	sealer, err := seal.New(cfg.StoreKeys)
	if err != nil {
		return err
	}
	// The health listener opens first: /healthz answers while Postwerk
	// waits for the data lock and starts up, so a liveness probe does not
	// kill it for waiting; /readyz fails until ready is set below (D61).
	var ready atomic.Pointer[httpapi.Ready]
	httpSrv := httpapi.New(cfg.HTTPListen, func(ctx context.Context) error {
		r := ready.Load()
		if r == nil {
			return errors.New("starting")
		}
		return (*r)(ctx)
	})
	httpL, err := net.Listen("tcp", cfg.HTTPListen)
	if err != nil {
		return fmt.Errorf("http listener: %w", err)
	}
	serveErr := make(chan error, 4)
	go func() {
		if err := httpSrv.Serve(httpL); !errors.Is(err, http.ErrServerClosed) {
			serveErr <- err
		}
	}()
	// Every return before the shutdown below closes it; after the
	// shutdown, Close does nothing more.
	defer httpSrv.Close()

	if !store.LockSupported {
		logger.Warn("no data lock on this platform: make sure only one Postwerk uses the database", "data", cfg.DataPath)
	}
	var rep *report.Reporter
	st, err := store.Open(ctx, cfg.DataPath, store.Options{
		OnEvent:           func() { rep.Wake() },
		PurgeRawOnHandoff: cfg.RawKeepAfterHandoff == 0,
		InsecureDelete:    !cfg.SecureDelete,
		LockWait:          cfg.DataLockWait,
		OnLockWait: func(waited time.Duration, holder string) {
			logger.Warn("database in use by another process, waiting for it to stop",
				"lock", store.LockPath(cfg.DataPath), "holder", holder,
				"waited", waited.Round(time.Second).String(), "wait", cfg.DataLockWait.String())
		},
	})
	if err != nil {
		if ctx.Err() != nil && errors.Is(err, ctx.Err()) {
			logger.Info("stopped while waiting for the database")
			return nil
		}
		return err
	}
	if schema, applied := st.Schema(); len(applied) > 0 {
		logger.Info("database migrated", "schema", schema, "applied", applied)
	} else {
		logger.Info("database schema up to date", "schema", schema)
	}
	sinks := []report.Sink{report.NewStdout(eventOut, cfg.Instance)}
	sentryLog := "off"
	if cfg.Sentry != nil {
		sinks = append(sinks, report.NewSentry(report.SentryOptions{
			DSN: cfg.Sentry.DSN, MinLevel: store.Level(cfg.Sentry.MinLevel), Instance: cfg.Instance,
			Version: version, Logger: logger,
		}))
		sentryLog = cfg.Sentry.DSN.Host + " (" + cfg.Sentry.MinLevel + ")"
	}
	rep = report.New(report.Options{Store: st, Sinks: sinks, Logger: logger})

	warnUnconfiguredTargets(ctx, st, cfg.Targets, logger)

	mgr := delivery.New(delivery.Options{
		Store: st, Sealer: sealer, Targets: cfg.Targets, Logger: logger, HeloName: cfg.Hostname,
		ProbeInterval: cfg.ProbeInterval, ProbeMaxInterval: cfg.ProbeMaxInterval, DownAfterProbes: cfg.DownAfterProbes,
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
	// The STARTTLS certificate is re-read every minute, so a renewed one
	// is served without a restart (I16).
	var smtpTLS *tlsreload.Reloader
	if cfg.TLSCertFile != "" {
		smtpTLS, err = tlsreload.New(tlsreload.Options{
			Name: "smtp", CertFile: cfg.TLSCertFile, KeyFile: cfg.TLSKeyFile, Logger: logger,
			OnEvent: eventAppender(st, logger),
		})
		if err != nil {
			st.Close()
			return err
		}
	}
	var getCert func(*tls.ClientHelloInfo) (*tls.Certificate, error)
	if smtpTLS != nil {
		getCert = smtpTLS.GetCertificate
	}
	// Postwerk's own signals (D49, D50): free space on the data volume and
	// mails waiting too long per target. Both start from the state the
	// event log has, so a restart neither repeats nor loses a change.
	disk, err := watch.NewDisk(ctx, watch.DiskOptions{
		Store: st, Dir: filepath.Dir(cfg.DataPath), MinFree: cfg.MinFreeBytes, Logger: logger,
	})
	if err != nil {
		st.Close()
		return err
	}
	var watched []watch.Target
	for _, t := range cfg.Targets {
		watched = append(watched, watch.Target{Name: t.Name, Optional: t.Optional})
	}
	queue, err := watch.NewQueue(ctx, watch.QueueOptions{
		Store: st, Targets: watched, After: cfg.QueueStaleAfter, Repeat: cfg.QueueStaleRepeat, Logger: logger,
	})
	if err != nil {
		st.Close()
		return err
	}
	sources, err := openSenders(ctx, cfg, st, logger)
	if err != nil {
		st.Close()
		return err
	}
	usage := senders.NewUsage(st, 0, logger)
	authn := senders.NewAuthenticator(sources.registry, senders.AuthOptions{Usage: usage, Logger: logger})
	volume, err := watch.NewVolume(ctx, watch.VolumeOptions{
		Store: st, Factor: cfg.SenderVolume.Factor, Min: cfg.SenderVolume.Min, Max: cfg.SenderVolume.Max, Logger: logger,
	})
	if err != nil {
		st.Close()
		return err
	}
	ing := ingress.New(ingress.Options{
		Store: st, Sealer: sealer, Auth: authn, Limiter: limit.New(nil), Targets: cfg.Targets, Hostname: cfg.Hostname,
		GetCertificate: getCert, AllowInsecure: cfg.AllowInsecureAuth,
		MaxMessageBytes: acceptBytes(cfg), MaxRecipients: cfg.MaxRecipients,
		MaxConnections: cfg.MaxConnections, MaxInflightBytes: cfg.MaxInflightBytes, Wake: mgr.Wake, Logger: logger,
		Space: disk,
	})
	smtpL, err := net.Listen("tcp", cfg.SMTPListen)
	if err != nil {
		st.Close()
		return fmt.Errorf("smtp listener: %w", err)
	}
	var adminSrv *http.Server
	var adminL net.Listener
	if cfg.Admin.Listen != "" {
		qo := queryOptions(cfg, st, mgr, sources.registry)
		q := query.New(qo)
		rs := resolve.New(resolve.Options{Store: st, Targets: qo.Targets, Wake: mgr.Wake})
		var tokens ui.Verifier // nil without a tokens file
		if cfg.Admin.Tokens != nil {
			tokens = cfg.Admin.Tokens
		}
		err := checkUIAccounts(ctx, cfg.Admin.Tokens, st, logger)
		var u *ui.UI
		if err == nil {
			u, err = ui.New(ui.Options{
				Query: q, Resolve: rs, Tokens: tokens, Accounts: st, Events: st, Hosts: cfg.Admin.Hosts, Instance: cfg.Instance,
				Logger: logger,
			})
		}
		if err == nil {
			adminL, err = net.Listen("tcp", cfg.Admin.Listen)
		}
		if err != nil {
			smtpL.Close()
			st.Close()
			return fmt.Errorf("admin listener: %w", err)
		}
		adminSrv = ui.NewServer(cfg.Admin.Listen, u, logger)
	}
	apiHandler, apiSrv, apiL, apiTLS, err := apiListener(cfg, st, mgr, sources.registry, logger)
	if err != nil {
		if adminL != nil {
			adminL.Close()
		}
		smtpL.Close()
		st.Close()
		return fmt.Errorf("api listener: %w", err)
	}

	runCtx, cancelRun := context.WithCancel(context.WithoutCancel(ctx))
	defer cancelRun()
	repDone := make(chan struct{})
	go func() { rep.Run(runCtx); close(repDone) }()
	retDone := make(chan struct{})
	go func() { ret.Run(runCtx); close(retDone) }()
	diskDone := make(chan struct{})
	go func() { disk.Run(runCtx); close(diskDone) }()
	queueDone := make(chan struct{})
	go func() { queue.Run(runCtx); close(queueDone) }()
	volumeDone := make(chan struct{})
	go func() { volume.Run(runCtx); close(volumeDone) }()
	usageDone := make(chan struct{})
	go func() { usage.Run(runCtx); close(usageDone) }()
	if sources.file != nil {
		go sources.file.Run(runCtx)
	}
	mgr.Start(runCtx)
	if smtpTLS != nil {
		go smtpTLS.Run(runCtx)
	}
	if apiTLS != nil {
		go apiTLS.Run(runCtx)
	}

	go func() { serveErr <- ing.Serve(smtpL) }()
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
	var apiAddr net.Addr
	apiLog := "off"
	if apiSrv != nil {
		apiAddr = apiL.Addr()
		apiLog = apiAddr.String()
		go func() {
			if err := apiSrv.Serve(apiL); !errors.Is(err, http.ErrServerClosed) {
				serveErr <- err
			}
		}()
	}

	var names []string
	for _, t := range cfg.Targets {
		names = append(names, t.Name+"("+string(t.Scheme)+")")
	}
	logger.Info("postwerk started", "version", version, "smtp", smtpL.Addr().String(),
		"http", httpL.Addr().String(), "admin", adminLog, "api", apiLog, "api_tls", apiTLS != nil,
		"targets", strings.Join(names, ","), "sentry", sentryLog, "sender_keys", string(cfg.Senders.Keys),
		"max_message_bytes", cfg.MaxMessageBytes, "smtp_size", acceptBytes(cfg),
		"smtp_tls", cfg.TLSCertificate != nil, "insecure_auth", cfg.AllowInsecureAuth,
		"store_encryption", sealer.Encrypting(), "pseudonymize_after", cfg.PseudonymizeAfter.String(),
		"min_free_bytes", cfg.MinFreeBytes, "queue_stale_after", cfg.QueueStaleAfter.String())
	// Ready means Postwerk can take and keep mail: the store answers and
	// can be written, the volume is above its reserve, the workers run. A
	// target that is down does not count: a restart cannot fix a provider,
	// and the mails wait in the queue for it (D52).
	var readyFn httpapi.Ready = func(ctx context.Context) error {
		if err := st.Ping(ctx); err != nil {
			return err
		}
		if err := disk.Ready(); err != nil {
			return err
		}
		if !mgr.Running() {
			return errors.New("delivery workers not running")
		}
		return nil
	}
	ready.Store(&readyFn)
	if listening != nil {
		listening(smtpL.Addr(), httpL.Addr(), adminAddr, apiAddr)
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
	// The API listener stops at the same time, within the same half; what
	// is left then is closed, so it can never use up the time the final
	// export needs. Ingress writes its folded refusal events when it has
	// stopped (D51), the API its folded failed authentications (H7), both
	// before the final export.
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
	apiDone := make(chan struct{})
	go func() {
		defer close(apiDone)
		if apiSrv == nil {
			return
		}
		if err := stopHTTP(sd, apiSrv, cfg.ShutdownTimeout/2); err != nil {
			logger.Warn("api connections still open at shutdown were closed", "err", err)
		}
		apiHandler.Close()
	}()
	if err := mgr.Stop(sd); err != nil {
		logger.Error("delivery workers did not stop in time; interrupted sends are recovered on the next start", "err", err)
	}
	<-ingDone
	<-apiDone
	for _, done := range []chan struct{}{repDone, retDone, diskDone, queueDone, volumeDone, usageDone} {
		select {
		case <-done:
		case <-sd.Done():
		}
	}
	// The last logins' times, before the store closes.
	if err := usage.Flush(sd); err != nil {
		logger.Warn("writing sender usage at shutdown failed", "err", err)
	}
	// The final export is bounded on its own, so a slow remote sink cannot
	// use up the deadline that closing the store still needs. stdout is
	// written at once; the other sinks catch up on the next start.
	fctx, fcancel := context.WithTimeout(sd, finalExportTimeout)
	if err := rep.Flush(fctx); err != nil {
		logger.Warn("final event export incomplete; it resumes on the next start", "err", err)
	}
	fcancel()
	if err := httpSrv.Shutdown(sd); err != nil {
		logger.Warn("http shutdown incomplete", "err", err)
	}
	if adminSrv != nil {
		if err := adminSrv.Shutdown(sd); err != nil {
			logger.Warn("admin listener shutdown incomplete", "err", err)
		}
	}
	// Closing the store releases the data lock (D61), after the final
	// export: a process waiting for it starts on a database nobody writes.
	if err := st.Close(); err != nil {
		logger.Warn("closing the store failed", "err", err)
	}
	logger.Info("postwerk stopped")
	return runErr
}

// stopHTTP shuts srv down within limit (and ctx), then closes the
// connections that are left. net/http counts a connection that a client
// opened but has not sent a request on yet as busy for five seconds; an
// HTTP client's spare keep-alive connection is one. Without the limit such a
// connection could hold up the shutdown that long.
func stopHTTP(ctx context.Context, srv *http.Server, limit time.Duration) error {
	c, cancel := context.WithTimeout(ctx, limit)
	defer cancel()
	err := srv.Shutdown(c)
	if err != nil {
		_ = srv.Close()
	}
	return err
}

// apiListener opens the JSON query API's listener (H2), with TLS from a
// reloaded certificate if configured. It returns nils when the API is off.
func apiListener(cfg *config.Config, st *store.Store, mgr *delivery.Manager, reg *senders.Registry,
	logger *slog.Logger) (*api.API, *http.Server, net.Listener, *tlsreload.Reloader, error) {
	if cfg.API.Listen == "" {
		return nil, nil, nil, nil, nil
	}
	qo := queryOptions(cfg, st, mgr, reg)
	q := query.New(qo)
	rs := resolve.New(resolve.Options{Store: st, Targets: qo.Targets, Wake: mgr.Wake})
	a, err := api.New(api.Options{Query: q, Resolve: rs, Tokens: cfg.API.Tokens, Events: st, Hosts: cfg.API.Hosts,
		Logger: logger})
	if err != nil {
		return nil, nil, nil, nil, err
	}
	var reloader *tlsreload.Reloader
	if cfg.API.TLSCertFile != "" {
		reloader, err = tlsreload.New(tlsreload.Options{
			Name: "api", CertFile: cfg.API.TLSCertFile, KeyFile: cfg.API.TLSKeyFile, Logger: logger,
			OnEvent: eventAppender(st, logger),
		})
		if err != nil {
			return nil, nil, nil, nil, err
		}
	}
	l, err := net.Listen("tcp", cfg.API.Listen)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	srv := api.NewServer(cfg.API.Listen, a, logger)
	if reloader != nil {
		srv.TLSConfig = &tls.Config{GetCertificate: reloader.GetCertificate, MinVersion: tls.VersionTLS12}
		l = tls.NewListener(l, srv.TLSConfig)
	}
	return a, srv, l, reloader, nil
}

// acceptBytes is the largest message ingress accepts (its SIZE):
// POSTWERK_MAX_MESSAGE_BYTES is the largest message sent to a target, so
// the header fields Postwerk adds are taken off (I17).
func acceptBytes(cfg *config.Config) int64 {
	return cfg.MaxMessageBytes - int64(delivery.MaxAddedBytes(cfg.Targets))
}

// eventAppender writes certificate reload events to the event log, so they
// reach every sink like other events.
func eventAppender(st *store.Store, logger *slog.Logger) func(tlsreload.Event) {
	appendEvent := appender(st, logger)
	return func(e tlsreload.Event) { appendEvent(e.Kind, string(e.Level), e.Text) }
}

// appender writes events that come from outside the store's transactions
// (certificate and senders-file reloads) to the event log.
func appender(st *store.Store, logger *slog.Logger) func(kind, level, text string) {
	return func(kind, level, text string) {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		err := st.Append(ctx, store.Event{Time: time.Now(), Kind: kind, Level: store.Level(level), Text: text})
		if err != nil {
			logger.Warn("writing an event failed", "kind", kind, "err", err)
		}
	}
}

// senderSources are the enabled sources of senders (D66). file and db are
// nil when off; db is also the managed write side (senders.Store) that the
// UI and the API use to create, rotate and revoke keys.
type senderSources struct {
	file     *senders.FileSource
	db       *senders.DBSource
	registry *senders.Registry
}

// openSenders opens the sources POSTWERK_SENDER_KEYS names and composes
// them. A name or key id in both stops the start.
func openSenders(ctx context.Context, cfg *config.Config, st *store.Store, logger *slog.Logger) (senderSources, error) {
	var s senderSources
	var err error
	if cfg.Senders.UsesFile() {
		appendEvent := appender(st, logger)
		s.file, err = senders.NewFileSource(senders.FileOptions{
			Path: cfg.Senders.File, AllowEmpty: cfg.Senders.Keys == config.SenderKeysBoth,
			OnEvent: func(e senders.Event) {
				if e.Kind == senders.KindReloadFailed {
					logger.Error(e.Text, "kind", e.Kind)
				} else {
					logger.Info(e.Text, "kind", e.Kind)
				}
				appendEvent(e.Kind, e.Level, e.Text)
			},
		})
		if err != nil {
			return s, fmt.Errorf("senders file: %w", err)
		}
	}
	if cfg.Senders.UsesDB() {
		if s.db, err = senders.NewDBSource(ctx, st, nil); err != nil {
			return s, err
		}
	}
	if s.registry, err = senders.NewRegistry(s.file, s.db); err != nil {
		return s, fmt.Errorf("senders: %w", err)
	}
	n := len(s.registry.List())
	if n == 0 {
		logger.Warn("no senders yet: nobody can send until one is created (docs/configuration.md, Senders)",
			"sender_keys", string(cfg.Senders.Keys))
	}
	logger.Info("senders loaded", "sender_keys", string(cfg.Senders.Keys), "senders", n)
	return s, nil
}

// checkUIAccounts makes sure someone can log in to the UI (U23): the first
// account comes from the tokens file, later ones live in the database. With
// neither, Postwerk does not start. Names in both are logged: the file
// wins at login.
func checkUIAccounts(ctx context.Context, tokens *auth.Tokens, st *store.Store, logger *slog.Logger) error {
	accounts, err := st.UIAccounts(ctx)
	if err != nil {
		return err
	}
	if tokens == nil && len(accounts) == 0 {
		return errors.New("the UI has no account yet: set POSTWERK_UI_TOKENS_FILE for the first login " +
			"(make a line with 'postwerk gen-token <name>'), create accounts with passwords in the UI, " +
			"then remove the file if you like; or set POSTWERK_ADMIN_LISTEN=off (docs/ui.md)")
	}
	for _, a := range accounts {
		if tokens != nil && tokens.Known(a.Name) {
			logger.Warn("UI account is in the tokens file and in the database; the tokens file wins at login",
				"account", a.Name)
		}
	}
	if tokens != nil {
		logger.Info("UI tokens file in use", "file_accounts", tokens.Len(), "database_accounts", len(accounts))
	}
	return nil
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

// queryOptions are the read layer's options for the UI and the API: the
// configured targets, which of them are optional, and the targets' health
// from the delivery workers (D45).
func queryOptions(cfg *config.Config, st *store.Store, mgr *delivery.Manager, reg *senders.Registry) query.Options {
	o := query.Options{Store: st, Hasher: hasher(cfg), Senders: reg}
	for _, t := range cfg.Targets {
		o.Targets = append(o.Targets, t.Name)
		if t.Optional {
			o.Optional = append(o.Optional, t.Name)
		}
	}
	o.Health = func() []query.TargetHealth {
		var out []query.TargetHealth
		for _, h := range mgr.Health() {
			out = append(out, query.TargetHealth{Target: h.Target, Down: h.Down, Since: h.Since, Reason: string(h.Reason)})
		}
		return out
	}
	return o
}
