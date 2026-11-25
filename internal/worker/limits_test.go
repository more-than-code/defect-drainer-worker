package worker

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/more-than-code/defect-drainer-worker/internal/cgroup"
	"github.com/more-than-code/defect-drainer-worker/workerproto"
)

func TestPolicyJobLimits(t *testing.T) {
	p := CheckPolicyData([]byte(defaultPolicy + "max_jobs = 2\n"))
	if !p.Valid || p.JobLimitsSet {
		t.Fatalf("%+v", p)
	}
	// Defaults: 75% of RAM shared by max_jobs, 4096 tasks, no CPU quota.
	l := p.JobLimits(8 << 30)
	if l.MemoryBytes != 3<<30 || l.Tasks != DefaultJobTasksMax || l.CPUs != 0 {
		t.Fatalf("defaults %+v", l)
	}
	if l := p.JobLimits(0); l.MemoryBytes != 0 {
		t.Fatalf("unknown RAM: %+v", l)
	}
	p = CheckPolicyData([]byte(defaultPolicy + "job_memory_max = \"2G\"\njob_tasks_max = 512\njob_cpu_percent = 150\n"))
	if !p.Valid || !p.JobLimitsSet {
		t.Fatalf("%+v", p)
	}
	if l := p.JobLimits(8 << 30); l.MemoryBytes != 2<<30 || l.Tasks != 512 || l.CPUs != 1.5 {
		t.Fatalf("set %+v", l)
	}
	for _, bad := range []string{
		`job_memory_max = "100M"`, `job_memory_max = "lots"`, `job_memory_max = 3`,
		`job_tasks_max = 10`, `job_tasks_max = 9999999`,
		`job_cpu_percent = 5`, `job_cpu_percent = 999999`,
	} {
		if p := CheckPolicyData([]byte(defaultPolicy + bad + "\n")); p.Valid {
			t.Errorf("%s accepted", bad)
		}
	}
}

// A host whose policy sets job limits it can't enforce declines the job.
func TestJobLimitsUnavailableDeclines(t *testing.T) {
	h := newHarness(t, harnessOpts{policy: defaultPolicy + "job_memory_max = \"2G\"\n"})
	id := "bjob_lim0"
	h.offerJob(fixSpec(id), nil)
	eventually(t, 30*time.Second, "decline", func() bool { return h.fs.declineFor(id) != "" })
	if got := h.fs.declineFor(id); got != workerproto.DeclinePolicyDenied {
		t.Fatalf("decline %q", got)
	}
	if !strings.Contains(h.log.String(), "job limits set in policy.toml but not enforceable") {
		t.Fatalf("no reason logged:\n%s", h.log.String())
	}
}

// With a delegated cgroup (a scratch cgroup2 dir standing in for the
// unit's), the agent runs in the run's own cgroup, and the cgroup is gone
// once the job ends.
func TestJobRunsInItsCgroup(t *testing.T) {
	dir := scratchCgroup(t)
	seen := filepath.Join(t.TempDir(), "cgroup.txt")
	h := newHarness(t, harnessOpts{
		cgroups: func() (*cgroup.Manager, error) { return cgroup.OpenDir(dir) },
		agent: "cat /proc/self/cgroup >> '" + seen + "'\n" + fixArtifacts(testDefect) +
			"cat '" + claudeFixture(t, "c1-basic") + "'\nexit 0\n",
	})
	id := "bjob_lim1"
	h.offerJob(fixSpec(id), nil)
	d := h.waitDone(id, 60*time.Second)
	events := joinLines(h.fs.jobEvents(id))
	if d.State != "completed" {
		t.Fatalf("done %+v\n%s\n%s", d, events, h.log.String())
	}
	got, err := os.ReadFile(seen)
	if err != nil || !strings.Contains(string(got), "/job-"+id+"-1-") {
		t.Fatalf("agent's cgroup: %q %v", got, err)
	}
	if !strings.Contains(events, "job limits: ") {
		t.Fatalf("no limits line:\n%s", events)
	}
	eventually(t, 15*time.Second, "job cgroup removed", func() bool {
		m, _ := filepath.Glob(filepath.Join(dir, "job-"+id+"-1-*"))
		return len(m) == 0
	})
	// This test's cgroup has no controllers: the default memory and task
	// limits are dropped, and status says so rather than claiming them.
	if st := h.w.limitsLine(h.w.policyNow()); !strings.Contains(st, "not limited: memory, pids (controller not delegated)") {
		t.Fatalf("status %q", st)
	}
	if !slices.Contains(h.w.capabilitiesWith(verifyState{}, h.w.policyNow()), workerproto.CapJobLimitsV1) {
		t.Fatal("no job_limits.v1 while limits are enforced")
	}
}

// A limit the policy sets whose controller is not delegated declines the
// offer up front, and the hello says the limits are not enforced.
func TestJobLimitMissingControllerDeclines(t *testing.T) {
	dir := scratchCgroup(t)
	h := newHarness(t, harnessOpts{
		policy:  defaultPolicy + "job_memory_max = \"2G\"\n",
		cgroups: func() (*cgroup.Manager, error) { return cgroup.OpenDir(dir) },
	})
	id := "bjob_lim2"
	h.offerJob(fixSpec(id), nil)
	eventually(t, 30*time.Second, "decline", func() bool { return h.fs.declineFor(id) != "" })
	if got := h.fs.declineFor(id); got != workerproto.DeclinePolicyDenied || h.fs.accepted(id) {
		t.Fatalf("decline %q, accepted %v", got, h.fs.accepted(id))
	}
	pol := h.w.policyNow()
	if st := h.w.limitsLine(pol); !strings.Contains(st, "not enforceable (job_memory_max is set but the memory controller is not delegated); declining every job") {
		t.Fatalf("status %q", st)
	}
	if slices.Contains(h.w.capabilitiesWith(verifyState{}, pol), workerproto.CapJobLimitsV1) {
		t.Fatal("job_limits.v1 while not enforced")
	}
	if wire := h.w.limitsWire(pol); wire.Enforced || wire.Reason == "" {
		t.Fatalf("hello.job_limits %+v", wire)
	}
}

// scratchCgroup is a fresh writable cgroup v2 dir, or a skip.
func scratchCgroup(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile("/proc/self/mountinfo")
	if err != nil {
		t.Skip("no /proc")
	}
	mnt := ""
	for _, l := range strings.Split(string(data), "\n") {
		pre, post, ok := strings.Cut(l, " - ")
		if f := strings.Fields(pre); ok && len(f) >= 5 && strings.HasPrefix(post, "cgroup2 ") {
			mnt = f[4]
		}
	}
	if mnt == "" {
		t.Skip("no cgroup2 mount")
	}
	dir := filepath.Join(mnt, "dd-worker-test-"+strings.ReplaceAll(t.Name(), "/", "_"))
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Skipf("no writable cgroup2 dir: %v", err)
	}
	t.Cleanup(func() {
		ents, _ := os.ReadDir(dir)
		for _, e := range ents {
			if e.IsDir() {
				_ = os.WriteFile(filepath.Join(dir, e.Name(), "cgroup.kill"), []byte("1"), 0)
				for i := 0; i < 50 && os.Remove(filepath.Join(dir, e.Name())) != nil; i++ {
					time.Sleep(20 * time.Millisecond)
				}
			}
		}
		_ = os.Remove(dir)
	})
	return dir
}
