package selfupdate

import (
	"context"
	"crypto/ed25519"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/more-than-code/defect-drainer-worker/internal/release"
	"github.com/more-than-code/defect-drainer-worker/workerproto"
)

// Reasons the root step reports in SelfUpdateResult.Reason (ADR 0002 §6.2,
// §7). The version rule reasons are release.Reason*.
const (
	ReasonBadRequest       = "bad_request"
	ReasonOff              = "off"
	ReasonPolicyInvalid    = "policy_invalid"
	ReasonOutsideWindow    = "outside_window"
	ReasonBusy             = "busy"
	ReasonKeyPlaceholder   = "key_placeholder"
	ReasonDownloadFailed   = "download_failed"
	ReasonSignatureInvalid = "signature_invalid"
	ReasonSumMismatch      = "sum_mismatch"
	ReasonBundleInvalid    = "bundle_invalid"
	ReasonVersionMismatch  = "version_mismatch"
	ReasonNotConnected     = "not_connected"
	ReasonHealth           = "health"
	ReasonInstallFailed    = "install_failed"
	ReasonRollbackFailed   = "rollback_failed"
	ReasonInternal         = "internal"
	// ReasonCooldown: the same target ended without success less than
	// Cooldown ago (the worker cools down too; this holds whatever it does).
	ReasonCooldown = "cooldown"
)

// Cooldown is how long a target that failed or was rolled back is not
// tried again (the worker's own cool-down is the same).
const Cooldown = 6 * time.Hour

// Retryable reports whether a refusal is about timing, not the release:
// the worker asks again soon instead of cooling down for Cooldown.
func Retryable(res *workerproto.SelfUpdateResult) bool {
	return res != nil && res.Result == workerproto.SelfUpdateRefused &&
		(res.Reason == ReasonBusy || res.Reason == ReasonOutsideWindow || res.Reason == ReasonCooldown)
}

// coolsDown reports whether res holds target back until res.At+Cooldown.
func coolsDown(res *workerproto.SelfUpdateResult, target string, now time.Time) bool {
	if res == nil || res.To != target || res.Result == workerproto.SelfUpdateSucceeded || Retryable(res) {
		return false
	}
	at, err := time.Parse(time.RFC3339, res.At)
	return err == nil && now.Before(at.Add(Cooldown))
}

// connectFailLine is what install.sh upgrade prints before its own
// rollback when the new version does not connect.
const connectFailLine = "did not connect within"

// HostPolicy is what the root step reads from policy.toml itself.
type HostPolicy struct {
	Valid  bool
	Mode   string
	Window Window
	Proxy  string
}

// Env is everything one run touches. Production wiring is in cmd/dd-worker;
// tests point each at fakes and temp dirs.
type Env struct {
	StateDir string // the worker's state dir (the request is in it)
	RootDir  string // root's dir: result.json and the staging dir
	LibDir   string // install.sh's versions dir (/usr/local/lib/dd-worker); "" skips its check
	LockPath string // "" skips the lock (tests)
	// MinInterval spaces runs out: a run less than this after the last one
	// waits first, so a request the worker keeps writing costs one run a
	// minute, never a spin. 0 (tests) does not wait.
	MinInterval time.Duration
	Arch        string // linux tarball arch: arm64 or amd64
	Keys        []ed25519.PublicKey

	// Installed returns the installed version (/usr/local/bin/dd-worker's target).
	Installed func() (string, error)
	Policy    func() HostPolicy
	Busy      func(ctx context.Context) bool
	Fetch     Fetcher
	// VersionOf runs `<bin> version` unprivileged and returns its output.
	VersionOf func(ctx context.Context, bin string) (string, error)
	// Install runs `/bin/sh <script> upgrade <version>` with
	// DD_WORKER_RELEASE_BASE=<base>; exit code and combined output.
	Install func(ctx context.Context, script, version, base string) (int, string, error)
	// Healthy waits for the new worker (dd-worker health --wait).
	Healthy func(ctx context.Context) error
	// Rollback runs the installed install.sh rollback.
	Rollback func(ctx context.Context) error

	Now  func() time.Time
	Logf func(format string, args ...any)
}

// Run takes the worker's request, if any, and carries it out. It writes
// result.json for every request it read and returns that result (nil when
// there was no request or another run holds the lock).
func Run(ctx context.Context, env Env) (*workerproto.SelfUpdateResult, error) {
	if env.Now == nil {
		env.Now = time.Now
	}
	if env.Logf == nil {
		env.Logf = func(string, ...any) {}
	}
	if env.LockPath != "" {
		unlock, err := holdLock(env.LockPath)
		if errors.Is(err, errLocked) {
			env.Logf("update-run: %v", err)
			return nil, nil
		}
		if err != nil {
			// Not another run: this one can't run at all. Answer the
			// request so the worker stops waiting and the unit stops firing.
			env.Logf("update-run: lock %s: %v", env.LockPath, err)
			discardRequest(env.StateDir)
			from, _ := env.Installed()
			return finish(env, workerproto.SelfUpdateResult{From: from, Result: workerproto.SelfUpdateFailed, Reason: ReasonInternal})
		}
		defer unlock()
	}
	if err := pace(ctx, env); err != nil {
		return nil, err
	}
	prev := ReadResult(env.RootDir)
	raw, err := takeRequest(env.StateDir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	from, _ := env.Installed()
	res := workerproto.SelfUpdateResult{From: from}
	if err != nil {
		env.Logf("update-run: request refused: %v", err)
		res.Result, res.Reason = workerproto.SelfUpdateRefused, ReasonBadRequest
		return finish(env, res)
	}
	target, ok := ValidRequest(raw)
	if !ok {
		res.Result, res.Reason = workerproto.SelfUpdateRefused, ReasonBadRequest
		return finish(env, res)
	}
	res.To = target
	if coolsDown(prev, target, env.Now()) {
		// Keep the result that started the cool-down: it says why.
		env.Logf("update-run: %s ended %s %s at %s; not again before %s", target, prev.Result, prev.Reason, prev.At, Cooldown)
		return nil, nil
	}
	res.Result, res.Reason = attempt(ctx, env, from, target)
	return finish(env, res)
}

// lastRunName records when the last run started (pace).
const lastRunName = "last-run"

// pace waits until MinInterval has passed since the last run started.
func pace(ctx context.Context, env Env) error {
	if env.MinInterval <= 0 {
		return nil
	}
	p := filepath.Join(env.RootDir, lastRunName)
	if st, err := os.Lstat(p); err == nil {
		if wait := st.ModTime().Add(env.MinInterval).Sub(env.Now()); wait > 0 {
			if wait > env.MinInterval {
				wait = env.MinInterval
			}
			t := time.NewTimer(wait)
			defer t.Stop()
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-t.C:
			}
		}
	}
	_ = os.Remove(p)
	return os.WriteFile(p, nil, 0o600)
}

func finish(env Env, res workerproto.SelfUpdateResult) (*workerproto.SelfUpdateResult, error) {
	res.At = env.Now().UTC().Format(time.RFC3339)
	env.Logf("update-run: %s -> %s: %s %s", res.From, res.To, res.Result, res.Reason)
	if err := writeResult(env.RootDir, res); err != nil {
		return &res, fmt.Errorf("write result: %w", err)
	}
	return &res, nil
}

func refused(reason string) (string, string) { return workerproto.SelfUpdateRefused, reason }
func failed(reason string) (string, string)  { return workerproto.SelfUpdateFailed, reason }

func attempt(ctx context.Context, env Env, from, target string) (string, string) {
	// 1. The host's own rules, re-checked here: nothing from the worker but
	// the version is trusted.
	pol := env.Policy()
	if !pol.Valid {
		return refused(ReasonPolicyInvalid)
	}
	if pol.Mode != workerproto.SelfUpdateServer {
		return refused(ReasonOff)
	}
	if why := release.Allowed(from, target); why != "" {
		return refused(why)
	}
	if !pol.Window.Open(env.Now()) {
		return refused(ReasonOutsideWindow)
	}
	if env.Busy(ctx) {
		return refused(ReasonBusy)
	}
	if release.OnlyPlaceholder(env.Keys) {
		return failed(ReasonKeyPlaceholder)
	}

	// 2. Download and verify: the sums and their signature first, then the
	// one tarball, checked against the signed sums.
	sums, err := env.Fetch.Fetch(ctx, target, "SHA256SUMS")
	if err != nil {
		env.Logf("update-run: download SHA256SUMS: %v", err)
		return failed(ReasonDownloadFailed)
	}
	sig, err := env.Fetch.Fetch(ctx, target, "SHA256SUMS.sig")
	if err != nil {
		env.Logf("update-run: download SHA256SUMS.sig: %v", err)
		return failed(ReasonDownloadFailed)
	}
	if err := release.Verify(env.Keys, sums, sig); err != nil {
		env.Logf("update-run: signature: %v", err)
		return failed(ReasonSignatureInvalid)
	}
	tarball := "dd-worker_" + target + "_linux_" + env.Arch + ".tar.gz"
	want, err := release.SumFor(sums, tarball)
	if err != nil {
		env.Logf("update-run: %s: %v", tarball, err)
		return failed(ReasonSumMismatch)
	}
	wantInstaller, err := release.SumFor(sums, "install.sh")
	if err != nil {
		return failed(ReasonSumMismatch)
	}
	tgz, err := env.Fetch.Fetch(ctx, target, tarball)
	if err != nil {
		env.Logf("update-run: download %s: %v", tarball, err)
		return failed(ReasonDownloadFailed)
	}
	if sha256Hex(tgz) != want {
		return failed(ReasonSumMismatch)
	}

	// 3. Stage: {root}/stage/v<target>/ as install.sh's file:// base wants
	// it, and the unpacked bundle in {root}/stage/bundle/.
	stage := filepath.Join(env.RootDir, "stage")
	if err := os.RemoveAll(stage); err != nil {
		return failed(ReasonInternal)
	}
	rel := filepath.Join(stage, "v"+target)
	bundle := filepath.Join(stage, "bundle")
	for _, d := range []string{stage, rel, bundle} {
		if err := os.Mkdir(d, 0o755); err != nil {
			env.Logf("update-run: stage: %v", err)
			return failed(ReasonInternal)
		}
	}
	defer os.RemoveAll(stage)
	if os.WriteFile(filepath.Join(rel, "SHA256SUMS"), sums, 0o644) != nil ||
		os.WriteFile(filepath.Join(rel, tarball), tgz, 0o644) != nil {
		return failed(ReasonInternal)
	}
	if err := unpack(tgz, bundle); err != nil {
		env.Logf("update-run: unpack: %v", err)
		return failed(ReasonBundleInvalid)
	}
	// The bundled install.sh must be the signed one.
	if got, err := fileSHA256(filepath.Join(bundle, "install.sh")); err != nil || got != wantInstaller {
		return failed(ReasonBundleInvalid)
	}
	out, err := env.VersionOf(ctx, filepath.Join(bundle, "dd-worker"))
	if err != nil || strings.TrimSpace(out) != target {
		env.Logf("update-run: bundled dd-worker version %q: %v", strings.TrimSpace(out), err)
		return failed(ReasonVersionMismatch)
	}

	// install.sh reuses an installed version dir as it is: one that is not
	// byte for byte this signed release (an older manual install, a failed
	// earlier try) goes first.
	if env.LibDir != "" && target != from {
		dir := filepath.Join(env.LibDir, target)
		if _, err := os.Lstat(dir); err == nil {
			have, herr := fileSHA256(filepath.Join(dir, "dd-worker"))
			want, werr := fileSHA256(filepath.Join(bundle, "dd-worker"))
			if herr != nil || werr != nil || have != want {
				env.Logf("update-run: %s differs from the signed release; replacing it", dir)
				if err := os.RemoveAll(dir); err != nil {
					return failed(ReasonInternal)
				}
			}
		}
	}

	// 4. Install with the release's own install.sh: it re-checks the sum,
	// switches, restarts, waits for the connect line and rolls back when it
	// does not come.
	code, log, err := env.Install(ctx, filepath.Join(bundle, "install.sh"), target, "file://"+stage)
	if err != nil || code != 0 {
		env.Logf("update-run: install.sh upgrade exit %d: %v\n%s", code, err, tail(log, 20))
		if strings.Contains(log, connectFailLine) && strings.Contains(log, "back on") {
			return workerproto.SelfUpdateRolledBack, ReasonNotConnected
		}
		// install.sh stopped after it switched (a failed restart, a kill):
		// switch back rather than leave the host half-updated.
		if now, ierr := env.Installed(); ierr != nil || now != from {
			env.Logf("update-run: install.sh stopped on %q; rolling back", now)
			if rerr := env.Rollback(ctx); rerr != nil {
				env.Logf("update-run: rollback: %v", rerr)
				return failed(ReasonRollbackFailed)
			}
			return workerproto.SelfUpdateRolledBack, ReasonInstallFailed
		}
		return failed(ReasonInstallFailed)
	}

	// 5. Healthy, or back to the previous version.
	if err := env.Healthy(ctx); err != nil {
		env.Logf("update-run: health: %v; rolling back", err)
		if rerr := env.Rollback(ctx); rerr != nil {
			env.Logf("update-run: rollback: %v", rerr)
			return failed(ReasonRollbackFailed)
		}
		return workerproto.SelfUpdateRolledBack, ReasonHealth
	}
	return workerproto.SelfUpdateSucceeded, ""
}

func tail(s string, n int) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}
