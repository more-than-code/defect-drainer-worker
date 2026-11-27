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
	selfUpdateCooldown = 6 * time.Hour
	// selfUpdateNoAnswer is how long a request may stay unread before the
	// worker gives up on it: the host has no update unit (installed with
	// an install.sh before self-update) or it is not running.
	selfUpdateNoAnswer = 10 * time.Minute
	// Reasons the worker itself reports (besides release.Reason*).
	reasonNoTarget      = "no_target"
	reasonNoRootStep    = "no_root_step"
	reasonPolicyInvalid = "policy_invalid"
)

// selfUpdateState is guarded by w.mu.
type selfUpdateState struct {
	target    string // the server's target_release from the last hello_ok
	state     string
	reason    string
	draining  bool
	requested time.Time // when the request was written ("" zero: none)
	last      *workerproto.SelfUpdateResult
	// cooled is the target that ended without success, and until when.
	cooled      string
	cooledUntil time.Time
}

func (w *Worker) selfUpdateRootDir() string {
	if w.opts.SelfUpdateRootDir != "" {
		return w.opts.SelfUpdateRootDir
	}
	return selfupdate.DefaultRootDir
}

// initSelfUpdate reads the root step's last result once at start.
func (w *Worker) initSelfUpdate() {
	last := selfupdate.ReadResult(w.selfUpdateRootDir())
	w.mu.Lock()
	w.su.state = workerproto.SelfUpdateIdle
	w.su.last = last
	if last != nil && last.Result != workerproto.SelfUpdateSucceeded && last.To != "" {
		if at, err := time.Parse(time.RFC3339, last.At); err == nil && time.Since(at) < selfUpdateCooldown {
			w.su.cooled, w.su.cooledUntil = last.To, at.Add(selfUpdateCooldown)
		}
	}
	w.mu.Unlock()
	if last != nil {
		w.logf("self-update: last result %s -> %s: %s %s", last.From, last.To, last.Result, last.Reason)
	}
	// A request left by an earlier process is stale: this one asks again.
	_ = os.Remove(filepath.Join(w.opts.Paths.StateDir, selfupdate.RequestDirName, selfupdate.RequestName))
}

// setTarget records hello_ok.target_release and re-checks at once.
func (w *Worker) setTarget(target string) {
	w.mu.Lock()
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
	before := *w.selfUpdateWire()
	w.advanceSelfUpdate(time.Now())
	after := *w.selfUpdateWire()
	if before.State != after.State || before.Reason != after.Reason || !sameResult(before.Last, after.Last) {
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
	// A request with the root step: wait for its answer.
	if su.state == workerproto.SelfUpdateUpdating {
		pending := selfupdate.RequestPending(w.opts.Paths.StateDir)
		res := selfupdate.ReadResult(w.selfUpdateRootDir())
		if res != nil && !sameResult(res, su.last) && res.To == su.target {
			// The root step answered without restarting this process: it
			// refused or failed before changing anything.
			su.last = res
			su.state, su.reason, su.draining, su.requested = workerproto.SelfUpdateCooldown, res.Reason, false, time.Time{}
			if res.Result != workerproto.SelfUpdateSucceeded {
				su.cooled, su.cooledUntil = res.To, now.Add(selfUpdateCooldown)
			}
			w.mu.Unlock()
			return
		}
		if pending && now.Sub(su.requested) > selfUpdateNoAnswer {
			_ = os.Remove(filepath.Join(w.opts.Paths.StateDir, selfupdate.RequestDirName, selfupdate.RequestName))
			su.state, su.reason, su.draining, su.requested = workerproto.SelfUpdateCooldown, reasonNoRootStep, false, time.Time{}
			su.cooled, su.cooledUntil = su.target, now.Add(selfUpdateCooldown)
			w.mu.Unlock()
			return
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
	case su.target == "":
		why = reasonNoTarget
	default:
		why = release.Allowed(Version, su.target)
	}
	if why == "" && su.cooled == su.target && now.Before(su.cooledUntil) {
		su.state, su.reason, su.draining = workerproto.SelfUpdateCooldown, "", false
		if su.last != nil && su.last.To == su.target {
			su.reason = su.last.Reason
		}
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
	w.su.state, w.su.requested = workerproto.SelfUpdateUpdating, now
	w.mu.Unlock()
}
