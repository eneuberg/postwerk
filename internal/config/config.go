// Package config builds Postwerk's configuration from environment variables
// and secret files (O1).
//
// Every POSTWERK_* variable must be known; a typo is an error, not a silently
// ignored setting. All problems are collected and returned together. Error
// texts name variables and files but never quote secret values.
package config

import (
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"math"
	"net"
	"net/url"
	"path"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/eneuberg/postwerk/internal/auth"
	"github.com/eneuberg/postwerk/internal/keys"
)

// Prefix starts every Postwerk environment variable.
const Prefix = "POSTWERK_"

// Defaults. docs/configuration.md is the reference for every variable.
const (
	DefaultSMTPListen = ":2525"
	DefaultHTTPListen = "127.0.0.1:8080"
	// DefaultAdminListen is loopback only, also inside containers (U1, U2).
	DefaultAdminListen = "127.0.0.1:8081"
	DefaultDataPath    = "/data/postwerk.db"
	DefaultHostname    = "postwerk"
	// DefaultMaxMessageBytes (recommended) is 15 MiB, the SIZE that
	// Mailjet's relay offers. It is the largest message a target receives;
	// ingress accepts that minus Postwerk's own header lines (I17).
	DefaultMaxMessageBytes = 15 << 20
	DefaultMaxRecipients   = 50
	DefaultMaxConnections  = 100
	// DefaultMaxInflightBytes (recommended): memory for messages being
	// received at once, 256 MiB (17 messages of the default 15 MiB).
	DefaultMaxInflightBytes = 256 << 20
	DefaultRetryWindow      = 24 * time.Hour
	// DefaultShutdownTimeout is longer than one delivery attempt can take
	// (delivery.DefaultAttemptTimeout, 90 s), so a send in progress always
	// finishes. The orchestrator's grace period must be longer still, for
	// example terminationGracePeriodSeconds: 120 in Kubernetes.
	DefaultShutdownTimeout = 100 * time.Second
	// DefaultQueueStaleAfter: a target whose oldest queued mail waits
	// longer is reported as stale (D49). Password resets and login codes
	// commonly expire after 15 to 30 minutes.
	DefaultQueueStaleAfter = 15 * time.Minute
	// DefaultMinFreeBytes is the free space kept on the data volume (D50):
	// room for the events, state changes and WAL growth of the mails
	// already accepted, four messages of the default maximum size.
	DefaultMinFreeBytes = 64 << 20
)

// Retention defaults (S6–S10). Each is the recommended value from
// docs/privacy-and-security.md section 3.2 and marked "recommended" in
// docs/configuration.md.
const (
	// DefaultRawKeepAfterHandoff (recommended): the message leaves the store
	// in the same transaction in which the last target took it.
	DefaultRawKeepAfterHandoff = time.Duration(0)
	// DefaultRawKeepAfterFailure (recommended): three days for an admin to
	// resend after failed or unknown.
	DefaultRawKeepAfterFailure = 72 * time.Hour
	// DefaultRawMaxAge (recommended): hard cap for any state.
	DefaultRawMaxAge = 7 * Day
	// DefaultPseudonymizeAfter (recommended): plain addresses for a month,
	// keyed hashes after that.
	DefaultPseudonymizeAfter = 30 * Day
	// DefaultRecordRetention (recommended): mails, recipients, deliveries.
	DefaultRecordRetention = 90 * Day
	// DefaultEventRetention (recommended): equal to the record retention, so
	// events never outlive their mail without a reason.
	DefaultEventRetention = 90 * Day
)

// Day is 24 hours, the unit of the "d" suffix in durations.
const Day = 24 * time.Hour

// maxDuration bounds every duration setting (100 years).
const maxDuration = 36500 * Day

// Limits for numeric settings.
const (
	minMessageBytes  = 1 << 10
	maxMessageBytes  = 1 << 30
	maxRecipients    = 1000
	maxTargets       = 16
	maxSecretFile    = 1 << 20
	maxConnections   = 10000
	maxInflightBytes = 1 << 40
	maxMinFreeBytes  = 1 << 50
)

// Scheme is how Postwerk talks to a target.
type Scheme string

// Target URL schemes (D7).
const (
	SchemeSMTPS    Scheme = "smtps"      // implicit TLS, default port 465
	SchemeSTARTTLS Scheme = "smtp"       // STARTTLS required, default port 587
	SchemePlain    Scheme = "smtp+plain" // cleartext, default port 25, never with credentials
)

var defaultPorts = map[Scheme]string{SchemeSMTPS: "465", SchemeSTARTTLS: "587", SchemePlain: "25"}

// OnUnknown is the policy for an unclear outcome after DATA (D5).
type OnUnknown string

// ON_UNKNOWN policies.
const (
	Hold  OnUnknown = "hold"
	Retry OnUnknown = "retry"
	// Check asks the provider. The status adapter is not built yet, so for
	// now it behaves like Hold.
	Check OnUnknown = "check"
)

// StatusMailjet is the only provider status adapter name accepted so far.
const StatusMailjet = "mailjet"

// Target is one delivery target.
type Target struct {
	Name     string
	Scheme   Scheme
	Host     string // for TLS server name verification
	Addr     string // host:port to dial
	Username string // empty: no AUTH
	Password string
	// Status names the provider adapter ("mailjet") or is empty.
	Status          string
	Optional        bool
	RetryWindow     time.Duration
	OnUnknown       OnUnknown
	AllowRecipients []string // lower-case path.Match patterns; empty: all allowed
	// MailjetTracking is TrackingOff or TrackingAccount; set only with
	// Status mailjet (D47).
	MailjetTracking string
	// RootCAs, if set, are the only certificate authorities trusted for
	// this target (CA_FILE, D48); nil: the system roots.
	RootCAs *x509.CertPool
	// CAFile names the file RootCAs came from, for the start log.
	CAFile string
}

// Values of POSTWERK_TARGET_<NAME>_MAILJET_TRACKING (D47).
const (
	// TrackingOff turns Mailjet's open and click tracking off for every
	// mail and removes the sender's own tracking headers. The default.
	TrackingOff = "off"
	// TrackingAccount leaves tracking to the Mailjet account settings and
	// passes the sender's tracking headers on.
	TrackingAccount = "account"
)

// Allowed reports whether every recipient matches the target's allow-list.
// A target without a list allows everything (D8).
func (t Target) Allowed(recipients []string) bool {
	if len(t.AllowRecipients) == 0 {
		return true
	}
	for _, r := range recipients {
		if foldsToASCII(r) {
			return false
		}
		r = strings.ToLower(r)
		ok := false
		for _, p := range t.AllowRecipients {
			if m, _ := path.Match(p, r); m {
				ok = true
				break
			}
		}
		if !ok {
			return false
		}
	}
	return true
}

// foldsToASCII reports whether s has a non-ASCII character that lower-cases
// to an ASCII one (the Kelvin sign K becomes k). Such an address would match
// an ASCII pattern while the target receives a different address.
func foldsToASCII(s string) bool {
	for _, r := range s {
		if r >= utf8.RuneSelf && unicode.ToLower(r) < utf8.RuneSelf {
			return true
		}
	}
	return false
}

// Config is the validated configuration.
type Config struct {
	SMTPListen     string
	HTTPListen     string
	DataPath       string
	Senders        *auth.Senders
	TLSCertificate *tls.Certificate // nil: no STARTTLS on ingress
	// TLSCertFile and TLSKeyFile name the pair, so it can be reloaded
	// (I16); empty without STARTTLS.
	TLSCertFile, TLSKeyFile string
	AllowInsecureAuth       bool
	Hostname                string
	MaxMessageBytes         int64
	MaxRecipients           int
	MaxConnections          int
	MaxInflightBytes        int64
	RetryWindow             time.Duration
	ShutdownTimeout         time.Duration
	Instance                string
	// QueueStaleAfter: see DefaultQueueStaleAfter; 0 turns the check off.
	QueueStaleAfter time.Duration
	// QueueStaleRepeat: queue.stale again after this time while a target
	// stays stale; 0 (default) only once per change.
	QueueStaleRepeat time.Duration
	// MinFreeBytes: see DefaultMinFreeBytes; 0 turns the check off.
	MinFreeBytes int64
	Targets      []Target
	// ProbeInterval and ProbeMaxInterval space the probes of a target
	// that is down (D45): the first after ProbeInterval, then doubling up
	// to ProbeMaxInterval.
	ProbeInterval, ProbeMaxInterval time.Duration
	// DownAfterProbes is how many probes of an unreachable target must
	// fail before target.down is written; 0 writes it at the first failed
	// attempt, as a rejected login always is (D58).
	DownAfterProbes int

	// StoreKeys encrypts raw messages at rest; nil only with
	// POSTWERK_STORE_ENCRYPTION=off.
	StoreKeys *keys.Ring
	// HashKeys hashes addresses; nil when pseudonymization is off and no
	// hash key file is set.
	HashKeys     *keys.Ring
	SecureDelete bool

	RawKeepAfterHandoff time.Duration
	RawKeepAfterFailure time.Duration
	RawMaxAge           time.Duration
	PseudonymizeAfter   time.Duration // 0: off
	RecordRetention     time.Duration
	EventRetention      time.Duration

	// Admin is the admin listener with the UI (U1–U4).
	Admin Admin
	// API is the JSON query API listener (H2); off by default.
	API API

	// Sentry configures the Sentry-protocol sink (A4); nil when it is off.
	Sentry *Sentry

	// SecretFiles maps each variable that names a secret file to its path,
	// so the caller can check file permissions.
	SecretFiles map[string]string
}

// Admin configures the admin listener.
type Admin struct {
	// Listen is the address; empty when the admin listener is off.
	Listen string
	// NonLoopback is set when Listen is not a loopback address, which needs
	// POSTWERK_ADMIN_ALLOW_NON_LOOPBACK=true.
	NonLoopback bool
	// Hosts are extra host names the UI answers to besides the loopback
	// names (lower case).
	Hosts []string
	// Tokens are the UI tokens from POSTWERK_UI_TOKENS_FILE, all admin;
	// nil without the file (the accounts are then in the database only).
	Tokens *auth.Tokens
}

// Sentry configures the Sentry-protocol sink.
type Sentry struct {
	DSN SentryDSN
	// MinLevel is "error" or "warn", the same words as event levels.
	MinLevel string
}

// SentryDSN is a parsed Sentry DSN.
type SentryDSN struct {
	// Endpoint is the envelope URL: {scheme}://{host}{path}/api/{project}/envelope/.
	Endpoint string
	// Host is the host name of the endpoint, for logs and network rules.
	Host string
	// PublicKey goes into X-Sentry-Auth as sentry_key.
	PublicKey string
	// SecretKey is deprecated in the protocol and optional; it is passed on
	// if the DSN has one.
	SecretKey string
}

var (
	dsnKeyRe     = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)
	dsnProjectRe = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)
)

// ParseSentryDSN parses {scheme}://{public key}[:{secret key}]@{host}[:port]{path}/{project id}.
// The scheme must be https; http only with allowHTTP, for local tests.
// Errors never quote the DSN, which holds the key.
func ParseSentryDSN(raw string, allowHTTP bool) (SentryDSN, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return SentryDSN{}, errors.New("not a valid URL")
	}
	switch {
	case u.Scheme == "https":
	case u.Scheme == "http" && allowHTTP:
	case u.Scheme == "http":
		return SentryDSN{}, errors.New("scheme http sends events in cleartext; use https, or allow http explicitly for a local test")
	default:
		return SentryDSN{}, errors.New("scheme must be https")
	}
	if u.Opaque != "" || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" {
		return SentryDSN{}, errors.New("no query or fragment allowed")
	}
	if u.User == nil || !dsnKeyRe.MatchString(u.User.Username()) {
		return SentryDSN{}, errors.New("public key missing or invalid (expected https://<key>@<host>/<project id>)")
	}
	secret, hasSecret := u.User.Password()
	if hasSecret && !dsnKeyRe.MatchString(secret) {
		return SentryDSN{}, errors.New("secret key invalid")
	}
	host := u.Hostname()
	if host == "" {
		return SentryDSN{}, errors.New("host missing")
	}
	if p := u.Port(); p != "" {
		if n, err := strconv.Atoi(p); err != nil || n < 1 || n > 65535 {
			return SentryDSN{}, errors.New("invalid port")
		}
	}
	prefix, project, _ := cutLast(strings.TrimSuffix(u.Path, "/"), "/")
	if !dsnProjectRe.MatchString(project) {
		return SentryDSN{}, errors.New("project id missing or invalid (the last path segment)")
	}
	return SentryDSN{
		Endpoint:  u.Scheme + "://" + u.Host + prefix + "/api/" + project + "/envelope/",
		Host:      host,
		PublicKey: u.User.Username(),
		SecretKey: secret,
	}, nil
}

func cutLast(s, sep string) (before, after string, found bool) {
	if i := strings.LastIndex(s, sep); i >= 0 {
		return s[:i], s[i+len(sep):], true
	}
	return "", s, false
}

// Warnings returns problems that do not stop the start but must be logged.
func (c *Config) Warnings() []string {
	var w []string
	if c.StoreKeys == nil {
		w = append(w, envStoreEncryption+"=off: raw messages are stored unencrypted; use this for local development only")
	}
	if !c.SecureDelete {
		w = append(w, envSecureDelete+"=off: deleted content can stay readable in free pages of the database file")
	}
	if c.PseudonymizeAfter == 0 {
		w = append(w, envPseudonymizeAfter+"=0: addresses stay in plain text until the records are deleted")
	}
	if c.Sentry != nil && strings.HasPrefix(c.Sentry.DSN.Endpoint, "http://") {
		w = append(w, envSentryAllowHTTP+"=true: events go to Sentry in cleartext; use this for a local test only")
	}
	if c.Admin.NonLoopback {
		w = append(w, envAdminListen+" is not a loopback address ("+envAdminAllowNonLoopback+"=true): "+
			"publish the port on the host's loopback only and never expose it (docs/ui.md)")
	}
	if c.QueueStaleAfter == 0 {
		w = append(w, envQueueStaleAfter+"=0: no event when mail waits too long; watch the queues yourself")
	}
	switch {
	case c.MinFreeBytes == 0:
		w = append(w, envMinFreeBytes+"=0: no free-space check; a full data volume refuses mail without an event")
	case c.MinFreeBytes < 2*c.MaxMessageBytes:
		w = append(w, envMinFreeBytes+" is less than twice "+envMaxMessageBytes+
			": the reserve may not hold the writes of the mails already accepted (docs/configuration.md)")
	}
	w = append(w, c.apiWarnings()...)
	return w
}

// ReadFile reads a secret file. os.ReadFile fits; tests pass a map.
type ReadFile func(name string) ([]byte, error)

// Global variable names.
const (
	envSMTPListen        = "POSTWERK_SMTP_LISTEN"
	envHTTPListen        = "POSTWERK_HTTP_LISTEN"
	envData              = "POSTWERK_DATA"
	envSendersFile       = "POSTWERK_SENDERS_FILE"
	envTLSCertFile       = "POSTWERK_SMTP_TLS_CERT_FILE"
	envTLSKeyFile        = "POSTWERK_SMTP_TLS_KEY_FILE"
	envAllowInsecureAuth = "POSTWERK_SMTP_ALLOW_INSECURE_AUTH"
	envHostname          = "POSTWERK_HOSTNAME"
	envMaxMessageBytes   = "POSTWERK_MAX_MESSAGE_BYTES"
	envMaxRecipients     = "POSTWERK_MAX_RECIPIENTS"
	envMaxConnections    = "POSTWERK_MAX_CONNECTIONS"
	envMaxInflightBytes  = "POSTWERK_MAX_INFLIGHT_BYTES"
	envRetryWindow       = "POSTWERK_RETRY_WINDOW"
	envShutdownTimeout   = "POSTWERK_SHUTDOWN_TIMEOUT"
	envInstance          = "POSTWERK_INSTANCE"
	envTargets           = "POSTWERK_TARGETS"
	envQueueStaleAfter   = "POSTWERK_QUEUE_STALE_AFTER"
	envQueueStaleRepeat  = "POSTWERK_QUEUE_STALE_REPEAT"
	envMinFreeBytes      = "POSTWERK_MIN_FREE_BYTES"

	envAdminListen           = "POSTWERK_ADMIN_LISTEN"
	envAdminAllowNonLoopback = "POSTWERK_ADMIN_ALLOW_NON_LOOPBACK"
	envAdminHosts            = "POSTWERK_ADMIN_HOSTS"
	envUITokensFile          = "POSTWERK_UI_TOKENS_FILE"

	envSentryDSNFile   = "POSTWERK_SENTRY_DSN_FILE"
	envSentryMinLevel  = "POSTWERK_SENTRY_MIN_LEVEL"
	envSentryAllowHTTP = "POSTWERK_SENTRY_ALLOW_HTTP"
	// envSentryDSN is refused: the DSN holds a key, so it comes from a file.
	envSentryDSN = "POSTWERK_SENTRY_DSN"

	envStoreKeyFile        = "POSTWERK_STORE_KEY_FILE"
	envStoreEncryption     = "POSTWERK_STORE_ENCRYPTION"
	envHashKeyFile         = "POSTWERK_HASH_KEY_FILE"
	envSecureDelete        = "POSTWERK_SECURE_DELETE"
	envRawKeepAfterHandoff = "POSTWERK_RAW_KEEP_AFTER_HANDOFF"
	envRawKeepAfterFailure = "POSTWERK_RAW_KEEP_AFTER_FAILURE"
	envRawMaxAge           = "POSTWERK_RAW_MAX_AGE"
	envPseudonymizeAfter   = "POSTWERK_PSEUDONYMIZE_AFTER"
	envRecordRetention     = "POSTWERK_RECORD_RETENTION"
	envEventRetention      = "POSTWERK_EVENT_RETENTION"
)

// laterPhase lists variables from the documentation that belong to features
// not built yet. They are known names, so configurations written from the
// README do not fail as typos, but a value is refused.
var laterPhase = []string{"POSTWERK_OTEL_ENDPOINT", "POSTWERK_WEBHOOK_URL"}

var globalVars = []string{
	envSMTPListen, envHTTPListen, envData, envSendersFile, envTLSCertFile, envTLSKeyFile,
	envAllowInsecureAuth, envHostname, envMaxMessageBytes, envMaxRecipients, envMaxConnections, envMaxInflightBytes, envRetryWindow,
	envShutdownTimeout, envInstance, envTargets, envQueueStaleAfter, envQueueStaleRepeat, envMinFreeBytes,
	envStoreKeyFile, envStoreEncryption, envHashKeyFile, envSecureDelete,
	envRawKeepAfterHandoff, envRawKeepAfterFailure, envRawMaxAge, envPseudonymizeAfter,
	envRecordRetention, envEventRetention,
	envAdminListen, envAdminAllowNonLoopback, envAdminHosts, envUITokensFile,
	envSentryDSNFile, envSentryMinLevel, envSentryAllowHTTP, envSentryDSN,
}

var targetSuffixes = []string{
	"URL", "CREDENTIALS_FILE", "STATUS", "OPTIONAL", "RETRY_WINDOW", "ON_UNKNOWN", "ALLOW_RECIPIENTS",
	"MAILJET_TRACKING", "CA_FILE",
}

var (
	targetNameRe = regexp.MustCompile(`^[a-z0-9_]{1,32}$`)
	hostnameRe   = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9-]{0,61}[A-Za-z0-9])?(\.[A-Za-z0-9]([A-Za-z0-9-]{0,61}[A-Za-z0-9])?)*$`)
	instanceRe   = regexp.MustCompile(`^[A-Za-z0-9._-]{0,64}$`)
)

// loader collects values and errors.
type loader struct {
	env         map[string]string
	readFile    ReadFile
	errs        []error
	secretFiles map[string]string
}

func (l *loader) errorf(format string, args ...any) {
	l.errs = append(l.errs, fmt.Errorf(format, args...))
}

func (l *loader) get(name, def string) string {
	if v, ok := l.env[name]; ok && v != "" {
		return v
	}
	return def
}

// Load builds the configuration from environ (as from os.Environ) and secret
// files read through readFile. It returns all problems at once, joined with
// errors.Join.
func Load(environ []string, readFile ReadFile) (*Config, error) {
	l := &loader{env: make(map[string]string), readFile: readFile}
	for _, kv := range environ {
		k, v, _ := strings.Cut(kv, "=")
		if strings.HasPrefix(k, Prefix) {
			l.env[k] = v
		}
	}

	c := &Config{
		SMTPListen:  l.listen(envSMTPListen, DefaultSMTPListen),
		HTTPListen:  l.listen(envHTTPListen, DefaultHTTPListen),
		DataPath:    l.get(envData, DefaultDataPath),
		Hostname:    l.get(envHostname, DefaultHostname),
		Instance:    l.get(envInstance, ""),
		SecretFiles: make(map[string]string),
	}
	l.secretFiles = c.SecretFiles
	if len(c.Hostname) > 253 || !hostnameRe.MatchString(c.Hostname) {
		l.errorf("%s: not a valid host name", envHostname)
	}
	if !instanceRe.MatchString(c.Instance) {
		l.errorf("%s: use up to 64 letters, digits, '.', '_' or '-'", envInstance)
	}
	c.MaxMessageBytes = int64(l.integer(envMaxMessageBytes, DefaultMaxMessageBytes, minMessageBytes, maxMessageBytes))
	c.MaxRecipients = l.integer(envMaxRecipients, DefaultMaxRecipients, 1, maxRecipients)
	c.MaxConnections = l.integer(envMaxConnections, DefaultMaxConnections, 1, maxConnections)
	c.MaxInflightBytes = int64(l.integer(envMaxInflightBytes, DefaultMaxInflightBytes, minMessageBytes, maxInflightBytes))
	if c.MaxInflightBytes < c.MaxMessageBytes {
		l.errorf("%s must be at least %s, or no message of the maximum size could be received",
			envMaxInflightBytes, envMaxMessageBytes)
	}
	c.RetryWindow = l.duration(envRetryWindow, DefaultRetryWindow, false)
	c.ShutdownTimeout = l.duration(envShutdownTimeout, DefaultShutdownTimeout, false)
	c.QueueStaleAfter = l.duration(envQueueStaleAfter, DefaultQueueStaleAfter, true)
	c.QueueStaleRepeat = l.duration(envQueueStaleRepeat, 0, true)
	if c.QueueStaleRepeat > 0 && c.QueueStaleRepeat < time.Minute {
		l.errorf("%s: use 0 (once per change) or at least 1m", envQueueStaleRepeat)
	}
	c.MinFreeBytes = int64(l.integer(envMinFreeBytes, DefaultMinFreeBytes, 0, maxMinFreeBytes))
	c.AllowInsecureAuth = l.boolean(envAllowInsecureAuth)

	c.Senders = l.senders()
	c.TLSCertificate = l.tlsCertificate()
	if c.TLSCertificate != nil {
		c.TLSCertFile, c.TLSKeyFile = l.env[envTLSCertFile], l.env[envTLSKeyFile]
	}
	tlsSet := l.env[envTLSCertFile] != "" || l.env[envTLSKeyFile] != ""
	switch {
	case !tlsSet && !c.AllowInsecureAuth:
		l.errorf("ingress needs TLS for AUTH: set %s and %s (STARTTLS), "+
			"or set %s=true to accept passwords over cleartext SMTP, "+
			"only on a network you trust (local development, or traffic encrypted between nodes)",
			envTLSCertFile, envTLSKeyFile, envAllowInsecureAuth)
	case tlsSet && c.AllowInsecureAuth:
		l.errorf("%s=true together with %s/%s is ambiguous: with a certificate, AUTH requires STARTTLS; "+
			"remove one of them", envAllowInsecureAuth, envTLSCertFile, envTLSKeyFile)
	}

	l.storeAndKeys(c)
	l.retention(c)
	l.admin(c)
	l.sentry(c)
	l.api(c)

	for _, name := range laterPhase {
		if l.env[name] != "" {
			l.errorf("%s: not implemented yet; leave it empty", name)
		}
	}

	l.probe(c)

	names, namesOK := l.targetNames()
	for _, name := range names {
		if t, ok := l.target(name, c.RetryWindow); ok {
			c.Targets = append(c.Targets, t)
		}
	}
	l.retryWindowsFit(c)

	l.unknownVars(names, namesOK)

	if len(l.errs) > 0 {
		return nil, errors.Join(l.errs...)
	}
	return c, nil
}

func (l *loader) unknownVars(targets []string, targetsOK bool) {
	known := make(map[string]bool)
	for _, k := range globalVars {
		known[k] = true
	}
	for _, k := range laterPhase {
		known[k] = true
	}
	for _, k := range apiVars {
		known[k] = true
	}
	for _, k := range probeVars {
		known[k] = true
	}
	for _, name := range targets {
		for _, s := range targetSuffixes {
			known[targetVar(name, s)] = true
		}
	}
	var unknown []string
	for k := range l.env {
		if known[k] {
			continue
		}
		if !targetsOK && strings.HasPrefix(k, "POSTWERK_TARGET_") {
			continue // already reported through POSTWERK_TARGETS
		}
		unknown = append(unknown, k)
	}
	slices.Sort(unknown)
	for _, k := range unknown {
		l.errorf("%s: unknown variable (typo, or a target missing from %s?)", k, envTargets)
	}
}

func targetVar(name, suffix string) string {
	return "POSTWERK_TARGET_" + strings.ToUpper(name) + "_" + suffix
}

func (l *loader) listen(name, def string) string {
	v := l.get(name, def)
	host, port, err := net.SplitHostPort(v)
	if err != nil {
		l.errorf("%s: expected host:port or :port", name)
		return v
	}
	if p, err := strconv.Atoi(port); err != nil || p < 0 || p > 65535 {
		l.errorf("%s: invalid port", name)
	}
	if strings.ContainsAny(host, "/ ") {
		l.errorf("%s: invalid host", name)
	}
	return v
}

func (l *loader) integer(name string, def, lo, hi int) int {
	v, ok := l.env[name]
	if !ok || v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < lo || n > hi {
		l.errorf("%s: expected a whole number between %d and %d", name, lo, hi)
		return def
	}
	return n
}

// duration reads a duration in Go syntax (30s, 15m, 24h) or in days (7d),
// also combined (1d12h). zeroOK allows 0.
func (l *loader) duration(name string, def time.Duration, zeroOK bool) time.Duration {
	v, ok := l.env[name]
	if !ok || v == "" {
		return def
	}
	d, err := ParseDuration(v)
	if err != nil || d < 0 || (d == 0 && !zeroOK) || d > maxDuration {
		if zeroOK {
			l.errorf("%s: expected 0 or a positive duration such as 30m, 72h or 7d", name)
		} else {
			l.errorf("%s: expected a positive duration such as 30s, 15m, 24h or 7d", name)
		}
		return def
	}
	return d
}

var daysRe = regexp.MustCompile(`^([0-9]{1,6})d(.*)$`)

// ParseDuration parses Go duration syntax plus a leading number of days with
// the suffix "d", for example "7d", "1d12h" or "90m".
func ParseDuration(s string) (time.Duration, error) {
	if m := daysRe.FindStringSubmatch(s); m != nil {
		days, _ := strconv.Atoi(m[1])
		if time.Duration(days) > math.MaxInt64/Day {
			// The product would wrap around, possibly to a short period.
			return 0, fmt.Errorf("invalid duration %q: too long", s)
		}
		d := time.Duration(days) * Day
		if m[2] == "" {
			return d, nil
		}
		rest, err := time.ParseDuration(m[2])
		if err != nil || rest < 0 || rest > math.MaxInt64-d {
			return 0, fmt.Errorf("invalid duration %q", s)
		}
		return d + rest, nil
	}
	return time.ParseDuration(s)
}

// onOff reads a setting that is "on" (also the default when empty) or "off".
func (l *loader) onOff(name string) bool {
	switch l.env[name] {
	case "", "on":
		return true
	case "off":
		return false
	}
	l.errorf("%s: expected on or off", name)
	return true
}

func (l *loader) boolean(name string) bool {
	switch l.env[name] {
	case "", "false":
		return false
	case "true":
		return true
	}
	l.errorf("%s: expected true or false", name)
	return false
}

func (l *loader) file(varName string) ([]byte, bool) {
	p := l.env[varName]
	l.secretFiles[varName] = p
	b, err := l.readFile(p)
	if err != nil {
		l.errorf("%s: cannot read file: %v", varName, err)
		return nil, false
	}
	if len(b) > maxSecretFile {
		l.errorf("%s: file larger than %d bytes", varName, maxSecretFile)
		return nil, false
	}
	return b, true
}

func (l *loader) senders() *auth.Senders {
	if l.env[envSendersFile] == "" {
		l.errorf("%s: required (file with one name:hash line per sender; create hashes with 'postwerk hash-password')", envSendersFile)
		return nil
	}
	b, ok := l.file(envSendersFile)
	if !ok {
		return nil
	}
	s, err := auth.ParseSenders(bytes.NewReader(b))
	if err != nil {
		for _, e := range unjoin(err) {
			l.errorf("%s: %v", envSendersFile, e)
		}
		return nil
	}
	return s
}

// admin reads the admin listener and the UI tokens (U1–U4). Unlike other
// variables, an empty POSTWERK_ADMIN_LISTEN does not mean the default: it
// turns the listener off, like "off".
func (l *loader) admin(c *Config) {
	allow := l.boolean(envAdminAllowNonLoopback)
	listen, set := l.env[envAdminListen]
	if !set {
		listen = DefaultAdminListen
	}
	if listen == "" || listen == "off" {
		for _, name := range []string{envUITokensFile, envAdminHosts, envAdminAllowNonLoopback} {
			if l.env[name] != "" {
				l.errorf("%s is set but the admin listener is off (%s=%s); remove one of them",
					name, envAdminListen, listen)
			}
		}
		return
	}
	c.Admin.Listen = l.listen(envAdminListen, listen)
	if _, port, _ := net.SplitHostPort(c.Admin.Listen); port != "0" &&
		(c.Admin.Listen == c.HTTPListen || c.Admin.Listen == c.SMTPListen) {
		l.errorf("%s: must differ from %s and %s; the UI never shares the health or SMTP listener",
			envAdminListen, envHTTPListen, envSMTPListen)
	}
	if host, _, err := net.SplitHostPort(c.Admin.Listen); err == nil && !loopbackHost(host) {
		if allow {
			c.Admin.NonLoopback = true
		} else {
			l.errorf("%s: must be a loopback address such as 127.0.0.1:8081, [::1]:8081 or localhost:8081; "+
				"people reach it through kubectl port-forward or an SSH tunnel (docs/ui.md). "+
				"Only inside a container whose port is published on the host's loopback, set %s=true",
				envAdminListen, envAdminAllowNonLoopback)
		}
	}
	if v := l.env[envAdminHosts]; v != "" {
		for _, h := range strings.Split(v, ",") {
			h = strings.ToLower(strings.TrimSpace(h))
			if h == "" || len(h) > 253 || (!hostnameRe.MatchString(h) && net.ParseIP(h) == nil) {
				l.errorf("%s: %q is not a host name or IP address (no port, no scheme)", envAdminHosts, h)
				continue
			}
			c.Admin.Hosts = append(c.Admin.Hosts, h)
		}
	}
	// The tokens file is optional: accounts in the database are enough once
	// there is one. Whether there is one is checked at start, with the
	// store open (U23).
	if l.env[envUITokensFile] == "" {
		return
	}
	b, ok := l.file(envUITokensFile)
	if !ok || noLines(b) {
		return // a file without lines: every account is in the database
	}
	t, err := auth.ParseTokens(bytes.NewReader(b))
	if err != nil {
		for _, e := range unjoin(err) {
			l.errorf("%s: %v", envUITokensFile, e)
		}
		return
	}
	// The UI has one kind of account (D35): every token is admin. A read
	// line from an older file must not silently become admin.
	for _, name := range t.Names() {
		if t.Role(name) != auth.RoleAdmin {
			l.errorf("%s: %q has role %s; the UI has admin accounts only, so change the line to "+
				"%s:admin:… or remove it (docs/ui.md)", envUITokensFile, name, t.Role(name), name)
		}
	}
	c.Admin.Tokens = t
}

// sentry reads the Sentry-protocol sink (A4). The DSN comes from a file,
// never from a variable, because it holds the project key.
func (l *loader) sentry(c *Config) {
	if l.env[envSentryDSN] != "" {
		l.errorf("%s: not supported; put the DSN into a file and set %s", envSentryDSN, envSentryDSNFile)
	}
	allowHTTP := l.boolean(envSentryAllowHTTP)
	if l.env[envSentryDSNFile] == "" {
		for _, name := range []string{envSentryMinLevel, envSentryAllowHTTP} {
			if l.env[name] != "" {
				l.errorf("%s is set but the Sentry sink is off (%s is empty); remove one of them", name, envSentryDSNFile)
			}
		}
		return
	}
	s := &Sentry{MinLevel: l.get(envSentryMinLevel, "error")}
	if s.MinLevel != "error" && s.MinLevel != "warn" {
		l.errorf("%s: expected error or warn", envSentryMinLevel)
	}
	b, ok := l.file(envSentryDSNFile)
	if !ok {
		return
	}
	line, _, _ := bytes.Cut(b, []byte("\n"))
	d, err := ParseSentryDSN(string(line), allowHTTP)
	if err != nil {
		// The error never quotes the DSN.
		l.errorf("%s: %v", envSentryDSNFile, err)
		return
	}
	s.DSN = d
	c.Sentry = s
}

// noLines reports whether a file has nothing but empty lines and comments.
func noLines(b []byte) bool {
	for _, line := range strings.Split(string(b), "\n") {
		if line = strings.TrimSpace(line); line != "" && !strings.HasPrefix(line, "#") {
			return false
		}
	}
	return true
}

// loopbackHost reports whether a listen host is loopback only: localhost,
// or an IP address in 127.0.0.0/8 or ::1. An empty host means every
// interface.
func loopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// storeAndKeys reads the store and hash key files and the secure delete
// setting.
func (l *loader) storeAndKeys(c *Config) {
	c.SecureDelete = l.onOff(envSecureDelete)
	encrypt := l.onOff(envStoreEncryption)
	switch {
	case encrypt && l.env[envStoreKeyFile] == "":
		l.errorf("%s: required (create a key with 'postwerk gen-key'), "+
			"unless %s=off for local development", envStoreKeyFile, envStoreEncryption)
	case encrypt:
		c.StoreKeys = l.keyFile(envStoreKeyFile)
	case l.env[envStoreKeyFile] != "":
		l.errorf("%s=off together with %s is ambiguous: remove one of them", envStoreEncryption, envStoreKeyFile)
	}
}

func (l *loader) keyFile(varName string) *keys.Ring {
	b, ok := l.file(varName)
	if !ok {
		return nil
	}
	r, err := keys.Parse(b)
	if err != nil {
		for _, e := range unjoin(err) {
			l.errorf("%s: %v", varName, e)
		}
		return nil
	}
	return r
}

// retention reads the retention periods and the hash key file, and checks
// that the periods fit together.
func (l *loader) retention(c *Config) {
	c.RawKeepAfterHandoff = l.duration(envRawKeepAfterHandoff, DefaultRawKeepAfterHandoff, true)
	c.RawKeepAfterFailure = l.duration(envRawKeepAfterFailure, DefaultRawKeepAfterFailure, true)
	c.RawMaxAge = l.duration(envRawMaxAge, DefaultRawMaxAge, false)
	c.PseudonymizeAfter = l.duration(envPseudonymizeAfter, DefaultPseudonymizeAfter, true)
	c.RecordRetention = l.duration(envRecordRetention, DefaultRecordRetention, false)
	c.EventRetention = l.duration(envEventRetention, DefaultEventRetention, false)

	switch {
	case l.env[envHashKeyFile] != "":
		c.HashKeys = l.keyFile(envHashKeyFile)
	case c.PseudonymizeAfter > 0:
		l.errorf("%s: required while %s is set (create a key with 'postwerk gen-key'; "+
			"use a different key than in %s)", envHashKeyFile, envPseudonymizeAfter, envStoreKeyFile)
	}
	if c.HashKeys != nil && c.StoreKeys != nil && c.HashKeys.Shares(c.StoreKeys) {
		l.errorf("%s and %s contain the same key; use separate keys, so one leak does not give both",
			envHashKeyFile, envStoreKeyFile)
	}

	notAfter := func(a string, av time.Duration, b string, bv time.Duration) {
		if av > bv {
			l.errorf("%s (%s) must not be longer than %s (%s)", a, av, b, bv)
		}
	}
	notAfter(envRawKeepAfterHandoff, c.RawKeepAfterHandoff, envRawMaxAge, c.RawMaxAge)
	notAfter(envRawKeepAfterFailure, c.RawKeepAfterFailure, envRawMaxAge, c.RawMaxAge)
	notAfter(envRawMaxAge, c.RawMaxAge, envRecordRetention, c.RecordRetention)
	notAfter(envRawMaxAge, c.RawMaxAge, envEventRetention, c.EventRetention)
	if c.PseudonymizeAfter > 0 {
		// Addresses are needed as long as a delivery may still be sent.
		notAfter(envRawMaxAge, c.RawMaxAge, envPseudonymizeAfter, c.PseudonymizeAfter)
		notAfter(envPseudonymizeAfter, c.PseudonymizeAfter, envRecordRetention, c.RecordRetention)
	}
}

// retryWindowsFit checks that every retry window ends before the raw message
// reaches its maximum age, so retention never cuts a retry short.
func (l *loader) retryWindowsFit(c *Config) {
	if c.RetryWindow >= c.RawMaxAge {
		l.errorf("%s (%s) must be shorter than %s (%s)", envRetryWindow, c.RetryWindow, envRawMaxAge, c.RawMaxAge)
	}
	for _, t := range c.Targets {
		if t.RetryWindow >= c.RawMaxAge && t.RetryWindow != c.RetryWindow {
			l.errorf("%s (%s) must be shorter than %s (%s)",
				targetVar(t.Name, "RETRY_WINDOW"), t.RetryWindow, envRawMaxAge, c.RawMaxAge)
		}
	}
}

func (l *loader) tlsCertificate() *tls.Certificate {
	certSet, keySet := l.env[envTLSCertFile] != "", l.env[envTLSKeyFile] != ""
	if !certSet && !keySet {
		return nil
	}
	if certSet != keySet {
		l.errorf("%s and %s must be set together", envTLSCertFile, envTLSKeyFile)
		return nil
	}
	certPEM, ok1 := l.file(envTLSCertFile)
	keyPEM, ok2 := l.file(envTLSKeyFile)
	if !ok1 || !ok2 {
		return nil
	}
	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		l.errorf("%s, %s: not a valid PEM certificate and key pair", envTLSCertFile, envTLSKeyFile)
		return nil
	}
	return &cert
}

func (l *loader) targetNames() ([]string, bool) {
	v := l.env[envTargets]
	if v == "" {
		l.errorf("%s: required (comma-separated target names such as mailjet,capture)", envTargets)
		return nil, false
	}
	var names []string
	ok := true
	for _, n := range strings.Split(v, ",") {
		n = strings.TrimSpace(n)
		switch {
		case !targetNameRe.MatchString(n):
			l.errorf("%s: target name %q must be 1 to 32 of a-z, 0-9 and _", envTargets, n)
			ok = false
		case slices.Contains(names, n):
			l.errorf("%s: target %q listed twice", envTargets, n)
			ok = false
		default:
			names = append(names, n)
		}
	}
	if len(names) > maxTargets {
		l.errorf("%s: at most %d targets", envTargets, maxTargets)
		ok = false
	}
	return names, ok
}

func (l *loader) target(name string, globalWindow time.Duration) (Target, bool) {
	before := len(l.errs)
	v := func(s string) string { return l.env[targetVar(name, s)] }
	t := Target{Name: name}

	urlVar := targetVar(name, "URL")
	if v("URL") == "" {
		l.errorf("%s: required (smtps://host[:465], smtp://host[:587] or smtp+plain://host[:25])", urlVar)
	} else {
		l.targetURL(urlVar, v("URL"), &t)
	}

	if credVar := targetVar(name, "CREDENTIALS_FILE"); v("CREDENTIALS_FILE") != "" {
		if t.Scheme == SchemePlain {
			l.errorf("%s: credentials are not allowed with smtp+plain://, they would cross the network in cleartext", credVar)
		} else if b, ok := l.file(credVar); ok {
			line, _, _ := bytes.Cut(b, []byte("\n"))
			line = bytes.TrimSuffix(line, []byte("\r"))
			user, pass, found := bytes.Cut(line, []byte(":"))
			switch {
			case !found || len(user) == 0 || len(pass) == 0:
				l.errorf("%s: first line must be username:password", credVar)
			case bytes.ContainsAny(line, "\x00\r"):
				l.errorf("%s: control characters in username or password", credVar)
			default:
				t.Username, t.Password = string(user), string(pass)
			}
		}
	}

	switch s := v("STATUS"); s {
	case "", StatusMailjet:
		t.Status = s
	default:
		l.errorf("%s: unknown status adapter (supported: %s)", targetVar(name, "STATUS"), StatusMailjet)
	}

	t.Optional = l.boolean(targetVar(name, "OPTIONAL"))
	t.RetryWindow = l.duration(targetVar(name, "RETRY_WINDOW"), globalWindow, false)

	onVar := targetVar(name, "ON_UNKNOWN")
	switch p := OnUnknown(v("ON_UNKNOWN")); p {
	case "":
		t.OnUnknown = Hold
		if t.Status != "" {
			t.OnUnknown = Check
		}
	case Hold, Retry:
		t.OnUnknown = p
	case Check:
		if v("STATUS") == "" {
			l.errorf("%s: check needs a status adapter (%s)", onVar, targetVar(name, "STATUS"))
		}
		t.OnUnknown = p
	default:
		l.errorf("%s: expected hold, retry or check", onVar)
	}

	trackVar := targetVar(name, "MAILJET_TRACKING")
	switch tr := v("MAILJET_TRACKING"); {
	case t.Status != StatusMailjet && tr != "":
		l.errorf("%s: only for a target with %s=%s", trackVar, targetVar(name, "STATUS"), StatusMailjet)
	case t.Status != StatusMailjet:
	case tr == "" || tr == TrackingOff:
		t.MailjetTracking = TrackingOff
	case tr == TrackingAccount:
		t.MailjetTracking = TrackingAccount
	default:
		l.errorf("%s: expected %s or %s", trackVar, TrackingOff, TrackingAccount)
	}

	if caVar := targetVar(name, "CA_FILE"); v("CA_FILE") != "" {
		t.CAFile = v("CA_FILE")
		if t.Scheme == SchemePlain {
			l.errorf("%s: smtp+plain:// uses no TLS, so a CA file has no effect; remove it", caVar)
		} else {
			t.RootCAs = l.caFile(caVar, t.CAFile)
		}
	}

	if a := v("ALLOW_RECIPIENTS"); a != "" {
		allowVar := targetVar(name, "ALLOW_RECIPIENTS")
		for _, p := range strings.Split(a, ",") {
			p = strings.ToLower(strings.TrimSpace(p))
			if p == "" {
				l.errorf("%s: empty pattern", allowVar)
				continue
			}
			if _, err := path.Match(p, ""); err != nil {
				l.errorf("%s: invalid pattern %q", allowVar, p)
				continue
			}
			if !strings.Contains(p, "@") {
				l.errorf("%s: pattern %q has no @ (use *@example.test for a domain)", allowVar, p)
				continue
			}
			t.AllowRecipients = append(t.AllowRecipients, p)
		}
	}

	return t, len(l.errs) == before
}

func (l *loader) targetURL(varName, raw string, t *Target) {
	u, err := url.Parse(raw)
	if err != nil {
		l.errorf("%s: not a valid URL", varName)
		return
	}
	t.Scheme = Scheme(u.Scheme)
	if _, ok := defaultPorts[t.Scheme]; !ok {
		l.errorf("%s: scheme must be smtps, smtp (STARTTLS) or smtp+plain", varName)
		return
	}
	if u.User != nil {
		l.errorf("%s: no user or password in the URL; use the CREDENTIALS_FILE variable", varName)
		return
	}
	if u.Opaque != "" || u.Path != "" || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" {
		l.errorf("%s: no path, query or fragment allowed", varName)
		return
	}
	t.Host = u.Hostname()
	if t.Host == "" {
		l.errorf("%s: host missing", varName)
		return
	}
	port := u.Port()
	if port == "" {
		port = defaultPorts[t.Scheme]
	} else if p, err := strconv.Atoi(port); err != nil || p < 1 || p > 65535 {
		l.errorf("%s: invalid port", varName)
		return
	}
	t.Addr = net.JoinHostPort(t.Host, port)
}

func unjoin(err error) []error {
	if j, ok := err.(interface{ Unwrap() []error }); ok {
		return j.Unwrap()
	}
	return []error{err}
}
