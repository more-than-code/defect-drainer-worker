package worker

import (
	"context"
	"errors"
	"fmt"

	"github.com/more-than-code/defect-drainer-worker/exec"
	"github.com/more-than-code/defect-drainer-worker/internal/sandbox"
)

// Sandboxed verify (V6–V9): a project's verify commands run only in the
// verify sandbox. Its self-test runs with the tools check (on hello and in
// the readiness loop); while it passes the hello lists verify_sandbox.v1,
// and while it fails every job's checks are skipped with
// verify_sandbox_unavailable. There is no unsandboxed path.

// verifyState is the last self-test.
type verifyState struct {
	checked  bool
	launcher sandbox.Launcher
	err      error
}

// findVerifySandbox is the production lookup and self-test.
func findVerifySandbox(ctx context.Context) (sandbox.Launcher, error) {
	l, err := sandbox.Find()
	if err != nil {
		return sandbox.Launcher{}, err
	}
	if err := l.SelfTest(ctx); err != nil {
		return sandbox.Launcher{}, err
	}
	return l, nil
}

// recheckVerifySandbox runs the self-test and logs a change.
func (w *Worker) recheckVerifySandbox(ctx context.Context) {
	find := w.opts.VerifySandbox
	if find == nil {
		find = findVerifySandbox
	}
	l, err := find(ctx)
	if err != nil && !errors.Is(err, sandbox.ErrUnavailable) {
		err = fmt.Errorf("%w: %v", sandbox.ErrUnavailable, err)
	}
	w.mu.Lock()
	prev := w.verify
	w.verify = verifyState{checked: true, launcher: l, err: err}
	w.mu.Unlock()
	if !prev.checked || (prev.err == nil) != (err == nil) || sandbox.Reason(prev.err) != sandbox.Reason(err) {
		w.logf("verify sandbox: %s", verifySandboxLine(err))
	}
}

// verifyNow is the last self-test; one that never ran is unavailable.
func (w *Worker) verifyNow() verifyState {
	w.mu.Lock()
	defer w.mu.Unlock()
	v := w.verify
	if !v.checked {
		v.err = fmt.Errorf("%w: self-test: not run yet", sandbox.ErrUnavailable)
	}
	return v
}

// verifySandboxLine is "ready" or "unavailable (<reason>)", as status and
// the log print it.
func verifySandboxLine(err error) string {
	if err == nil {
		return "ready"
	}
	return "unavailable (" + verifyReason(err) + ")"
}

func verifyReason(err error) string {
	r := sandbox.Reason(err)
	if r == "" {
		r = "unknown"
	}
	return r
}

// verifyBox is job jobID's verify sandbox. It is set on every job's host,
// ready or not: an unavailable one skips the checks.
func (w *Worker) verifyBox(jobID string) *exec.VerifySandbox {
	v := w.verifyNow()
	return newVerifyBox(w.opts.Paths, jobID, v.launcher, v.err, w.logf)
}

// newVerifyBox hides the worker's own dirs (its config, the LoadCredential
// copy, its whole state dir) on top of the host's DenyRead. Bound paths may
// lie only in the job's dir and the clones dir; the run's tmp and cache
// dirs are made in the job's dir, which the agent can't write.
func newVerifyBox(p Paths, jobID string, l sandbox.Launcher, err error, logf func(string, ...any)) *exec.VerifySandbox {
	hide := append(p.hostSecretDirs(), p.ConfigDir, p.StateDir)
	return &exec.VerifySandbox{
		Launcher: l, Unavailable: err, Hide: hide,
		Roots:    []string{p.JobDir(jobID), p.ReposDir()},
		WorkDir:  p.JobDir(jobID),
		OnRefuse: func(host string) { logf("verify egress: refused %s", host) },
	}
}
