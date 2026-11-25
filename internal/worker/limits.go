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
	"github.com/more-than-code/defect-drainer-worker/workerproto"
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

// supervisorTasks is what effectiveLimits keeps of the unit's TasksMax
// for the worker itself when it shares the rest between max_jobs jobs.
const supervisorTasks = 512

// effectiveLimits are the limits a job's cgroup gets on this host, and the
// default limits it drops because their controller is not delegated. err
// says why limits the policy sets can't be enforced: no delegated cgroup,
// or a missing controller for a limit the policy names.
func (w *Worker) effectiveLimits(pol Policy) (cgroup.Limits, []string, error) {
	if w.cg == nil {
		return cgroup.Limits{}, nil, w.cgErr
	}
	l := pol.JobLimits(w.memTotal)
	// The default task limit leaves room under the unit's TasksMax for the
	// worker and every other job: a Go runtime that can't make a thread
	// dies.
	if pol.JobTasksMax == 0 {
		if unit := w.cg.PidsMax(); unit > 0 {
			share := (unit - supervisorTasks) / int64(max(pol.MaxJobs, 1))
			l.Tasks = max(min(l.Tasks, share), MinJobTasksMax)
		}
	}
	have := w.cg.Controllers()
	var dropped []string
	for _, c := range []struct {
		ctl, key string
		set      bool
		val      *int64
	}{
		{"memory", "job_memory_max", pol.JobMemoryMax > 0, &l.MemoryBytes},
		{"pids", "job_tasks_max", pol.JobTasksMax > 0, &l.Tasks},
	} {
		if *c.val == 0 || slices.Contains(have, c.ctl) {
			continue
		}
		if c.set {
			return l, nil, fmt.Errorf("%s is set but the %s controller is not delegated", c.key, c.ctl)
		}
		dropped = append(dropped, c.ctl)
		*c.val = 0
	}
	if l.CPUs > 0 && !slices.Contains(have, "cpu") {
		return l, nil, errors.New("job_cpu_percent is set but the cpu controller is not delegated")
	}
	return l, dropped, nil
}

// jobCgroup makes run gen's cgroup, or nil when this host has none.
func (w *Worker) jobCgroup(jobID string, gen int) (*cgroup.Job, error) {
	if w.cg == nil {
		return nil, nil
	}
	l, _, err := w.effectiveLimits(w.policyNow())
	if err != nil {
		return nil, err
	}
	return w.cg.Job(jobID+"-"+strconv.Itoa(gen), l)
}

// limitsUnavailable is why a policy that sets job limits can't take a job
// ("": it can, or the policy sets none). The offer is declined with it.
func (w *Worker) limitsUnavailable(pol Policy) string {
	if !pol.JobLimitsSet {
		return ""
	}
	if _, _, err := w.effectiveLimits(pol); err != nil {
		return "job limits set in policy.toml but not enforceable: " + limitsReason(err)
	}
	return ""
}

// limitsLine is the per-job limits as `dd-worker status` shows them: what
// is actually written, never the policy's wish.
func (w *Worker) limitsLine(pol Policy) string {
	l, dropped, err := w.effectiveLimits(pol)
	if err != nil {
		state := "off"
		if w.cg != nil {
			state = "not enforceable"
		}
		line := state + " (" + limitsReason(err) + ")"
		if pol.JobLimitsSet {
			line += "; declining every job until fixed, since policy.toml sets job_* limits"
		}
		return line
	}
	line := l.String() + " per job"
	if len(dropped) > 0 {
		line += "; not limited: " + strings.Join(dropped, ", ") + " (controller not delegated)"
	}
	return line
}

// limitsWire is hello.job_limits for the same state.
func (w *Worker) limitsWire(pol Policy) *workerproto.JobLimitsStatus {
	l, _, err := w.effectiveLimits(pol)
	if err != nil {
		return &workerproto.JobLimitsStatus{Reason: limitsReason(err)}
	}
	return &workerproto.JobLimitsStatus{Enforced: true, MemoryBytes: l.MemoryBytes, Tasks: l.Tasks, CPUPercent: int(l.CPUs * 100)}
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
		// Never handed to a spawn: its fd can go now.
		go func() { j.w.releaseCgroup(cg); cg.Release() }()
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
	s := fmt.Sprintf("job limits: the job reached its memory limit (%s); the kernel killed %d of its processes. Raise job_memory_max in policy.toml if the job needs more", cgroup.FormatBytes(j.cg.Limits.MemoryBytes), n)
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
	// Its fd stays open until the job is discarded (releaseCgroupFDs): a
	// late spawn then fails on the removed cgroup.
	j.oldCgs = append(j.oldCgs, cg)
	go j.w.releaseCgroup(cg)
}

// releaseCgroupFDs closes every cgroup fd of the job's ended runs, once the
// job is discarded and nothing can spawn for it.
func (j *job) releaseCgroupFDs() {
	j.mu.Lock()
	cgs := j.oldCgs
	j.oldCgs = nil
	j.mu.Unlock()
	for _, cg := range cgs {
		cg.Release()
	}
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

// seccompLine is this process's seccomp state from /proc/self/status: the
// unit's SystemCallFilter puts the worker in filter mode.
func seccompLine() string {
	data, err := os.ReadFile("/proc/self/status")
	if err != nil {
		return ""
	}
	mode, count := "", ""
	for _, l := range strings.Split(string(data), "\n") {
		if v, ok := strings.CutPrefix(l, "Seccomp:"); ok {
			mode = strings.TrimSpace(v)
		}
		if v, ok := strings.CutPrefix(l, "Seccomp_filters:"); ok {
			count = strings.TrimSpace(v)
		}
	}
	switch {
	case mode == "2" && count == "1":
		return "on (1 filter)"
	case mode == "2" && count != "":
		return "on (" + count + " filters)"
	case mode == "2":
		return "on"
	case mode == "":
		return ""
	}
	return "off (the unit's SystemCallFilter is not applied)"
}
