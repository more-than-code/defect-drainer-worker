package worker

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/more-than-code/defect-drainer-worker/git"
	"github.com/more-than-code/defect-drainer-worker/internal/botagent/config"
	"github.com/more-than-code/defect-drainer-worker/internal/botagent/redact"
	"github.com/more-than-code/defect-drainer-worker/internal/cgroup"
	"github.com/more-than-code/defect-drainer-worker/internal/sandbox"
	"github.com/more-than-code/defect-drainer-worker/workerproto"
)

// Version is the dd-worker version; cmd/dd-worker sets it from ldflags.
var Version = "0.1.0"

// Exit reasons of Run. cmd/dd-worker maps each to a non-zero exit.
var (
	ErrRevoked         = errors.New("credential revoked: re-enrol this host (dd-worker init)")
	ErrReplaced        = errors.New("socket replaced 3 times in 10 minutes: another host uses this credential; re-enrol one of them")
	ErrProtocol        = errors.New("server does not speak worker protocol 1")
	ErrOrgMismatch     = errors.New("policy.toml orgs does not list this worker's org")
	ErrNotEnrolled     = errors.New("not enrolled: run dd-worker init")
	errHelloRefused    = errors.New("hello refused")
	errSocketClosed    = errors.New("socket closed")
	errUnauthenticated = errors.New("unauthenticated")
)

// Timing are the protocol's waits, overridable so tests run in seconds.
// Zero fields take the protocol values.
type Timing struct {
	AcceptGrace     time.Duration // §5.2: 5 s with no lease_lost before starting
	CancelGrace     time.Duration // §4.4, §6.3: SIGINT then SIGKILL after 3 s
	BatchInterval   time.Duration // §6.1: 1 s
	Readiness       time.Duration // §4.2: 5 min
	ReplacedBackoff time.Duration // §4.5: at least 60 s plus jitter
	ReplacedWindow  time.Duration // §4.5: 10 min
	LeaseWait       time.Duration // replay a busy job with no lease frame after this
	HelloTimeout    time.Duration
	BackoffMax      time.Duration // reconnect backoff cap (conn.Backoff caps at 30 s)
}

func (t Timing) withDefaults() Timing {
	def := func(d *time.Duration, v time.Duration) {
		if *d <= 0 {
			*d = v
		}
	}
	def(&t.AcceptGrace, workerproto.LateAcceptGrace)
	def(&t.CancelGrace, workerproto.CancelGrace)
	def(&t.BatchInterval, workerproto.BatchInterval)
	def(&t.Readiness, workerproto.ReadinessRecheck)
	def(&t.ReplacedBackoff, workerproto.ReplacedBackoffMin)
	def(&t.ReplacedWindow, 10*time.Minute)
	def(&t.LeaseWait, 10*time.Second)
	def(&t.HelloTimeout, 15*time.Second)
	def(&t.BackoffMax, 30*time.Second)
	return t
}

// Options configure a Worker.
type Options struct {
	Paths    Paths
	Adapters AdapterConfig
	Timing   Timing
	// SpoolMax is the worker-wide spool cap (§6.1, 200 MiB).
	SpoolMax int64
	// Log receives the worker's log lines, already redacted. Nil is stderr.
	Log io.Writer
	// OwnerUID overrides the owner check of policy.toml and secrets.env
	// (tests: a temp file cannot be chowned to root).
	OwnerUID func(os.FileInfo) (uint32, bool)
	// VerifySandbox finds the verify sandbox and runs its self-test (nil:
	// bwrap and socat on PATH, then the real self-test). Tests replace it.
	VerifySandbox func(context.Context) (sandbox.Launcher, error)
	// Cgroups takes over the worker's delegated cgroup for per-job limits
	// (cgroup.Open; `dd-worker run` sets it). Nil: no per-job cgroups, as
	// in tests, which must never move the test process.
	Cgroups func() (*cgroup.Manager, error)
}

// Worker is one dd-worker process.
type Worker struct {
	opts Options
	t    Timing

	logMu  sync.Mutex
	logOut io.Writer

	mu       sync.Mutex
	cfg      config.Config
	policy   Policy
	secrets  Secrets
	adapters map[string]*hostAdapter
	statuses []workerproto.AdapterStatus
	tools    []workerproto.ToolStatus
	jobs     map[string]*job
	offers   map[string]*pendingOffer // by job id
	tokens   map[string]bool          // every job token seen, for log redaction
	// serverCaps are the capabilities of the current hello_ok.
	serverCaps []string
	envRefs    map[string]int // SecretEnv names set in this process
	sock       *socket
	revoked    bool

	redactor redact.Exact

	// verify is the last verify-sandbox self-test.
	verify verifyState

	// cg is the delegated cgroup per-job limits use; nil with cgErr saying
	// why. memTotal is the host's RAM, for the default memory limit.
	cg       *cgroup.Manager
	cgErr    error
	memTotal int64

	// masked is the last socket scan's list (policy agent_browser only).
	masked []string
	// scanOverride, set by tests, adjusts the socket scan (roots, uid).
	scanOverride func(socketScan) socketScan
}

// New loads the credential, policy and secrets and recovers jobs left by a
// previous run (§6.4).
func New(opts Options) (*Worker, error) {
	if opts.SpoolMax <= 0 {
		opts.SpoolMax = workerproto.SpoolMaxBytes
	}
	w := &Worker{
		opts:    opts,
		t:       opts.Timing.withDefaults(),
		logOut:  opts.Log,
		jobs:    map[string]*job{},
		offers:  map[string]*pendingOffer{},
		tokens:  map[string]bool{},
		envRefs: map[string]int{},
	}
	if w.logOut == nil {
		w.logOut = os.Stderr
	}
	// Every worker worktree is linked to a clone: no plain-checkout fallback.
	git.RequireLinkedWorktrees()
	if err := MigrateCredential(opts.Paths, w.logf); err != nil {
		return nil, err
	}
	cfg, err := config.Load(opts.Paths.Env())
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, ErrNotEnrolled
		}
		return nil, err
	}
	w.cfg = cfg
	w.openCgroups()
	w.reloadHostFiles()
	if err := os.MkdirAll(opts.Paths.JobsDir(), 0o700); err != nil {
		return nil, err
	}
	if err := w.recoverJobs(); err != nil {
		return nil, err
	}
	return w, nil
}

// reloadHostFiles re-reads policy.toml and secrets.env and rebuilds the
// adapters. An unreadable secrets.env leaves the previous set and is logged.
func (w *Worker) reloadHostFiles() {
	pol := LoadPolicy(w.opts.Paths.Policy(), false, w.opts.OwnerUID)
	sec, err := w.loadSecrets()
	w.mu.Lock()
	w.policy = pol
	if err == nil {
		w.secrets = sec
	} else if w.secrets.vals == nil {
		w.secrets = Secrets{vals: map[string]string{}}
	}
	w.adapters = buildAdapters(w.opts.Adapters, w.secrets)
	w.rebuildRedactorLocked()
	w.mu.Unlock()
	if err != nil {
		w.logf("secrets: %v", err)
	}
	if !pol.Valid {
		w.logf("policy.toml is not usable (%s): every offer is declined", pol.Reason)
	}
}

// loadSecrets reads the LoadCredential copy when systemd gave one, else
// the file on disk. The copy is fixed while the service runs, so an edit to
// secrets.env takes effect on the next restart.
func (w *Worker) loadSecrets() (Secrets, error) {
	if d := w.opts.Paths.CredentialsDir; d != "" {
		return LoadSecretsCredential(d)
	}
	return LoadSecrets(w.opts.Paths.Secrets(), w.opts.OwnerUID)
}

func (w *Worker) rebuildRedactorLocked() {
	extra := []string{w.cfg.Credential}
	for t := range w.tokens {
		extra = append(extra, t)
	}
	w.redactor = w.secrets.Redactor(extra...)
}

// rememberToken adds a job token to the log redaction set.
func (w *Worker) rememberToken(tok string) {
	if tok == "" {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if !w.tokens[tok] {
		w.tokens[tok] = true
		w.rebuildRedactorLocked()
	}
}

// logf writes one worker log line with every secret value, the credential
// and job tokens replaced.
func (w *Worker) logf(format string, args ...any) {
	line := fmt.Sprintf(format, args...)
	w.mu.Lock()
	r := w.redactor
	w.mu.Unlock()
	line = r.Values(line)
	line = strings.Map(func(c rune) rune {
		if c < 0x20 && c != '\t' || c == 0x7f {
			return '?'
		}
		return c
	}, strings.TrimRight(line, "\n"))
	w.logMu.Lock()
	defer w.logMu.Unlock()
	fmt.Fprintf(w.logOut, "%s dd-worker: %s\n", time.Now().UTC().Format(time.RFC3339), strings.TrimRight(line, "\n"))
}

// origin is the server's https origin; wsURL its socket URL (§1).
func (w *Worker) origin() string { return strings.TrimRight(w.cfg.ServerURL, "/") }

func wsURL(origin string) (string, error) {
	u, err := url.Parse(strings.TrimRight(origin, "/"))
	if err != nil {
		return "", err
	}
	switch u.Scheme {
	case "https":
		u.Scheme = "wss"
	case "http":
		u.Scheme = "ws"
	default:
		return "", fmt.Errorf("server url scheme %q", u.Scheme)
	}
	u.Path = workerproto.PathSocket
	return u.String(), nil
}

// osArch are the enrol frame's os and arch.
func osArch() (string, string) { return runtime.GOOS, runtime.GOARCH }
