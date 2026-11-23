package worker

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/more-than-code/defect-drainer-worker/internal/cgroup"
)

// Per-job limits (DEF-…-68ru): each run of a job gets its own cgroup v2
// under the worker's delegated one, with the policy's memory, task and CPU
// limits (internal/cgroup).

// openCgroups takes over the delegated cgroup when the options allow it.
func (w *Worker) openCgroups() {
	w.memTotal = hostMemTotal()
	if w.opts.Cgroups == nil {
		w.cgErr = fmt.Errorf("%w: not enabled in this process", cgroup.ErrUnavailable)
		return
	}
	m, err := w.opts.Cgroups()
	if err != nil {
		w.cgErr = err
		w.logf("per-job limits off: %s (the unit's limits still apply)", limitsReason(err))
		return
	}
	w.cg = m
	w.logf("per-job limits on: %s, controllers %s", m.Dir(), strings.Join(m.Controllers(), " "))
}

// jobCgroup makes run gen's cgroup, or nil when this host has none.
func (w *Worker) jobCgroup(jobID string, gen int) (*cgroup.Job, error) {
	if w.cg == nil {
		return nil, nil
	}
	pol := w.policyNow()
	l := pol.JobLimits(w.memTotal)
	// A default limit whose controller the unit did not delegate is
	// dropped (the start log names the controllers); one the policy sets
	// is an error.
	have := w.cg.Controllers()
	for _, c := range []struct {
		ctl string
		set bool
		val *int64
	}{{"memory", pol.JobMemoryMax > 0, &l.MemoryBytes}, {"pids", pol.JobTasksMax > 0, &l.Tasks}} {
		if !slices.Contains(have, c.ctl) {
			if c.set {
				return nil, fmt.Errorf("the %s controller is not delegated", c.ctl)
			}
			*c.val = 0
		}
	}
	if l.CPUs > 0 && !slices.Contains(have, "cpu") {
		return nil, errors.New("the cpu controller is not delegated")
	}
	return w.cg.Job(jobID+"-"+strconv.Itoa(gen), l)
}

// limitsUnavailable is why a policy that sets job limits can't take a job
// ("": it can, or the policy sets none).
func (w *Worker) limitsUnavailable(pol Policy) string {
	if !pol.JobLimitsSet || w.cg != nil {
		return ""
	}
	return "job limits set in policy.toml but unavailable: " + limitsReason(w.cgErr)
}

// limitsLine is the per-job limits as `dd-worker status` shows them.
func (w *Worker) limitsLine(pol Policy) string {
	if w.cg == nil {
		return "off (" + limitsReason(w.cgErr) + ")"
	}
	return pol.JobLimits(w.memTotal).String() + " per job"
}

func limitsReason(err error) string {
	if err == nil {
		return "unknown"
	}
	return strings.TrimPrefix(err.Error(), cgroup.ErrUnavailable.Error()+": ")
}

// adoptCgroup makes cg the job's for run gen and logs its limits. False:
// the run is gone, and cg was released.
func (j *job) adoptCgroup(gen int, cg *cgroup.Job) bool {
	j.mu.Lock()
	defer j.mu.Unlock()
	if gen != j.gen || j.doneSpooled {
		go j.w.releaseCgroup(cg)
		return false
	}
	j.cg = cg
	j.emitLocked(logEvent("job limits: "+cg.Limits.String(), "info"))
	return true
}

// oomLineLocked is the job-log line for a run whose processes reached the
// memory limit, or nil.
func (j *job) oomLineLocked() *string {
	if j.cg == nil {
		return nil
	}
	n := j.cg.OOMKills()
	if n == 0 {
		return nil
	}
	s := fmt.Sprintf("job limits: the job reached its memory limit (%s); the kernel killed %d of its processes", cgroup.FormatBytes(j.cg.Limits.MemoryBytes), n)
	return &s
}

// releaseCgroupLocked lets the run's processes finish (the cancel grace
// and a little more), then kills what is left and removes the cgroup.
func (j *job) releaseCgroupLocked() {
	if j.cg == nil {
		return
	}
	cg := j.cg
	j.cg = nil
	go j.w.releaseCgroup(cg)
}

func (w *Worker) releaseCgroup(cg *cgroup.Job) {
	deadline := time.Now().Add(w.t.CancelGrace + 5*time.Second)
	for !cg.Empty() && time.Now().Before(deadline) {
		time.Sleep(100 * time.Millisecond)
	}
	if err := cg.Close(); err != nil {
		w.logf("job limits: removing %s: %v", cg.Dir(), err)
	}
}

// hostMemTotal is MemTotal from /proc/meminfo in bytes (0: unknown).
func hostMemTotal() int64 {
	f, err := os.Open("/proc/meminfo")
	if err != nil {
		return 0
	}
	defer f.Close()
	s := bufio.NewScanner(f)
	for s.Scan() {
		if v, ok := strings.CutPrefix(s.Text(), "MemTotal:"); ok {
			fs := strings.Fields(v)
			if len(fs) == 2 && fs[1] == "kB" {
				n, _ := strconv.ParseInt(fs[0], 10, 64)
				return n << 10
			}
		}
	}
	return 0
}
