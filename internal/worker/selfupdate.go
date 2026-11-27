package worker

import (
	"context"
	"os"
	"path/filepath"
	"time"

	"github.com/more-than-code/defect-drainer-worker/internal/release"
	"github.com/more-than-code/defect-drainer-worker/internal/selfupdate"
	"github.com/more-than-code/defect-drainer-worker/workerproto"
)

// The worker's side of self-update (docs/adr/0002-self-update.md §6.1).
// The worker only asks: once idle it writes the server's target to its
// request file, and the host's root step (dd-worker update-run) checks
// everything again and installs it.

// selfUpdateTick is how often a pending target is re-checked (a var for
// tests).
var selfUpdateTick = time.Minute

const (
	// selfUpdateCooldown keeps a target that did not succeed from being
	// asked for again, unless the server's target changes.
	selfUpdateCooldown = selfupdate.Cooldown
	// selfUpdateRetry is the wait after a refusal about timing (busy,
	// outside the window): ask again soon, not in 6 hours.
	selfUpdateRetry = 5 * time.Minute
	// selfUpdateNoAnswer is how long a request may stay unread before the
	// worker gives up on it: the host has no update unit (installed with
	// an install.sh before self-update) or it is not running.
	selfUpdateNoAnswer = 10 * time.Minute
	// selfUpdateNoResult is how long the worker waits for a result once the
	// root step took the request (its unit stops it after 15 minutes).
	selfUpdateNoResult = 20 * time.Minute
	// Reasons the worker itself reports (besides release.Reason*).
	reasonNoTarget      = "no_target"
	reasonNoRootStep    = "no_root_step"
	reasonPolicyInvalid = "policy_invalid"
	reasonNoResult      = "no_result"
)

// selfUpdateState is guarded by w.mu.
type selfUpdateState struct {
	target    string // the server's target_release from the last hello_ok
	state     string
	reason    string
	draining  bool
	requested time.Time // when the request was written (zero: none)
	// requestedTo is the version the request named: the root step's
	// result answers it, whatever the server's target is by then.
	requestedTo string
	// badTarget: the server sent a target that is not a version.
	badTarget bool
	last      *workerproto.SelfUpdateResult
	// cooled is the target that ended without success, until when, and why.
	cooled      string
	cooledUntil time.Time
	coolReason  string
}

func (w *Worker) selfUpdateRootDir() string {
	if w.opts.SelfUpdateRootDir != "" {
		return w.opts.SelfUpdateRootDir
	}
	return selfupdate.DefaultRootDir
}

// initSelfUpdate reads the root step's last result once at start.
func (w *Worker) initSelfUpdate() {
	w.mu.Lock()
	w.su.state = workerproto.SelfUpdateIdle
	last := w.adoptResultLocked(time.Now())
	w.mu.Unlock()
	if last != nil {
		w.logf("self-update: last result %s -> %s: %s %s", last.From, last.To, last.Result, last.Reason)
	}
	// A request left by an earlier process is stale: this one asks again.
	_ = os.Remove(filepath.Join(w.opts.Paths.StateDir, selfupdate.RequestDirName, selfupdate.RequestName))
}

// adoptResultLocked reads result.json and, when it holds a result this
// worker has not seen, records it and its cool-down. The root step writes
// it last, often after it restarted this worker, so every step reads it
// again. It returns the new result, or nil. Called with w.mu held.
func (w *Worker) adoptResultLocked(now time.Time) *workerproto.SelfUpdateResult {
	res := selfupdate.ReadResult(w.selfUpdateRootDir())
	if res == nil || sameResult(res, w.su.last) || !resultNewer(res, w.su.last) {
		return nil
	}
	su := &w.su
	su.last = res
	switch {
	case res.To == "" || res.Result == workerproto.SelfUpdateSucceeded:
	case selfupdate.Retryable(res):
		su.cooled, su.cooledUntil, su.coolReason = res.To, now.Add(selfUpdateRetry), res.Reason
	default:
		until := now.Add(selfUpdateCooldown)
		if at, err := time.Parse(time.RFC3339, res.At); err == nil {
			until = at.Add(selfUpdateCooldown)
		}
		su.cooled, su.cooledUntil, su.coolReason = res.To, until, res.Reason
	}
	// It answers this worker's request (or the root step could not read it).
	if su.state == workerproto.SelfUpdateUpdating && (res.To == su.requestedTo || res.To == "") {
		su.state, su.draining, su.requested, su.requestedTo = workerproto.SelfUpdateIdle, false, time.Time{}, ""
	}
	return res
}

// resultNewer: a has a later At than b (a result with no time is newer
// than none).
func resultNewer(a, b *workerproto.SelfUpdateResult) bool {
	if b == nil {
		return true
	}
	ta, ea := time.Parse(time.RFC3339, a.At)
	tb, eb := time.Parse(time.RFC3339, b.At)
	if ea != nil || eb != nil {
		return a.At != b.At
	}
	return ta.After(tb)
}

// setTarget records hello_ok.target_release and re-checks at once.
func (w *Worker) setTarget(target string) {
	w.mu.Lock()
	// The server is not trusted with more than a version string.
	_, err := release.ParseVersion(target)
	w.su.badTarget = target != "" && err != nil
	if w.su.badTarget {
		target = ""
	}
	w.su.target = target
	w.mu.Unlock()
	w.stepSelfUpdate()
}

// selfUpdateLoop re-checks a pending target every selfUpdateTick.
func (w *Worker) selfUpdateLoop(ctx context.Context) {
	t := time.NewTicker(selfUpdateTick)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			w.stepSelfUpdate()
		}
	}
}

// drainingForUpdate reports whether new offers are declined busy.
func (w *Worker) drainingForUpdate() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.su.draining
}

// selfUpdateWire is hello.self_update and status.self_update.
func (w *Worker) selfUpdateWire() *workerproto.SelfUpdateStatus {
	w.mu.Lock()
	defer w.mu.Unlock()
	mode := w.policy.AutoUpdate
	if mode == "" {
		mode = workerproto.SelfUpdateOff
	}
	st := w.su.state
	if st == "" {
		st = workerproto.SelfUpdateIdle
	}
	out := &workerproto.SelfUpdateStatus{Mode: mode, State: st, Target: w.su.target, Reason: w.su.reason}
	if w.su.last != nil {
		l := *w.su.last
		out.Last = &l
	}
	return out
}

// stepSelfUpdate moves the state machine one step and sends a status frame
// when the state changed.
func (w *Worker) stepSelfUpdate() {
	// One step at a time: the ticker and a hello_ok may both call this.
	w.suStep.Lock()
	defer w.suStep.Unlock()
	before := *w.selfUpdateWire()
	w.advanceSelfUpdate(time.Now())
	after := *w.selfUpdateWire()
	if before.State != after.State || before.Reason != after.Reason || before.Target != after.Target || !sameResult(before.Last, after.Last) {
		w.logf("self-update: %s %s (target %s)", after.State, after.Reason, after.Target)
		w.send(w.statusFrame())
		w.writeSnapshot(w.sockUp())
	}
}

func sameResult(a, b *workerproto.SelfUpdateResult) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

func (w *Worker) advanceSelfUpdate(now time.Time) {
	w.mu.Lock()
	pol := w.policy
	su := &w.su
	w.adoptResultLocked(now)
	// A request with the root step: wait for its answer.
	if su.state == workerproto.SelfUpdateUpdating {
		pending := selfupdate.RequestPending(w.opts.Paths.StateDir)
		giveUp := ""
		switch {
		case pending && now.Sub(su.requested) > selfUpdateNoAnswer:
			_ = os.Remove(filepath.Join(w.opts.Paths.StateDir, selfupdate.RequestDirName, selfupdate.RequestName))
			giveUp = reasonNoRootStep
		case !pending && now.Sub(su.requested) > selfUpdateNoResult:
			// Taken, never answered: the root step was stopped or died.
			giveUp = reasonNoResult
		}
		if giveUp != "" {
			su.cooled, su.cooledUntil, su.coolReason = su.requestedTo, now.Add(selfUpdateCooldown), giveUp
			su.state, su.reason, su.draining, su.requested, su.requestedTo = workerproto.SelfUpdateCooldown, giveUp, false, time.Time{}, ""
		}
		w.mu.Unlock()
		return
	}
	why := ""
	switch {
	case !pol.Valid:
		why = reasonPolicyInvalid
	case pol.AutoUpdate != workerproto.SelfUpdateServer:
		why = selfupdate.ReasonOff
	case su.badTarget:
		why = release.ReasonNotAllowed
	case su.target == "":
		why = reasonNoTarget
	default:
		why = release.Allowed(Version, su.target)
	}
	if why == "" && su.cooled == su.target && now.Before(su.cooledUntil) {
		su.state, su.reason, su.draining = workerproto.SelfUpdateCooldown, su.coolReason, false
		w.mu.Unlock()
		return
	}
	if why == "" && !pol.AutoUpdateWindow.Open(now) {
		why = selfupdate.ReasonOutsideWindow
	}
	if why != "" {
		su.state, su.reason, su.draining = workerproto.SelfUpdateIdle, why, false
		w.mu.Unlock()
		return
	}
	// Drain: no new offers; ask once nothing runs and nothing is offered.
	su.state, su.reason, su.draining = workerproto.SelfUpdateDraining, "", true
	target := su.target
	offers := len(w.offers)
	w.mu.Unlock()
	if offers > 0 || len(w.busyIDs()) > 0 {
		return
	}
	if err := selfupdate.WriteRequest(w.opts.Paths.StateDir, target); err != nil {
		w.logf("self-update: request %s: %v", target, err)
		return
	}
	w.mu.Lock()
	w.su.state, w.su.requested, w.su.requestedTo = workerproto.SelfUpdateUpdating, now, target
	w.mu.Unlock()
}
