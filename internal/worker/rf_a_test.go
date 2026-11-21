package worker

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	dexec "github.com/more-than-code/defect-drainer-worker/exec"
	"github.com/more-than-code/defect-drainer-worker/git"
	"github.com/more-than-code/defect-drainer-worker/workerproto"
)

// Remote job fixes (RF-a): the worker half of the remote-job-fixes spec.

func lastAgentRun(events []dexec.Event) *dexec.AgentRun {
	var r *dexec.AgentRun
	for _, e := range events {
		if e.AgentRun != nil {
			r = e.AgentRun
		}
	}
	return r
}

func lastHarvest(events []dexec.Event) *dexec.FixHarvest {
	var h *dexec.FixHarvest
	for _, e := range events {
		if e.Harvest != nil {
			h = e.Harvest
		}
	}
	return h
}

const policyBase = "enabled = true\norgs = [\"org_a\"]\n"

// Criterion 4 (validation half): bad hosts and costs invalidate the policy.
func TestPolicyAllowedDomainsAndMaxCost(t *testing.T) {
	path := filepath.Join(t.TempDir(), "policy.toml")
	writeFile(t, path, policyBase+"agent_allowed_domains = [\"registry.npmjs.org\", \"api.example-1.com\"]\nmax_cost_usd = 3\n", 0o644)
	p := LoadPolicy(path, false, rootUID)
	if !p.Valid || strings.Join(p.AllowedDomains, ",") != "registry.npmjs.org,api.example-1.com" || p.MaxCostUSD != 3 {
		t.Fatalf("valid policy %+v", p)
	}
	writeFile(t, path, policyBase, 0o644)
	if p := LoadPolicy(path, false, rootUID); !p.Valid || len(p.AllowedDomains) != 0 || p.MaxCostUSD != 0 {
		t.Fatalf("defaults %+v", p)
	}
	many := make([]string, MaxAllowedDomains+1)
	for i := range many {
		many[i] = "\"h" + string(rune('a'+i)) + ".example.com\""
	}
	for body, reason := range map[string]string{
		"agent_allowed_domains = [\"Registry.npmjs.org\"]\n":           "bad_agent_allowed_domains",
		"agent_allowed_domains = [\"*.npmjs.org\"]\n":                  "bad_agent_allowed_domains",
		"agent_allowed_domains = [\"203.0.113.7\"]\n":                  "bad_agent_allowed_domains",
		"agent_allowed_domains = [\"::1\"]\n":                          "bad_agent_allowed_domains",
		"agent_allowed_domains = [\"registry.npmjs.org:443\"]\n":       "bad_agent_allowed_domains",
		"agent_allowed_domains = [\"localhost\"]\n":                    "bad_agent_allowed_domains",
		"agent_allowed_domains = [\"-a.example.com\"]\n":               "bad_agent_allowed_domains",
		"agent_allowed_domains = [\"\"]\n":                             "bad_agent_allowed_domains",
		"agent_allowed_domains = [\"a.com\", \"a.com\"]\n":             "bad_agent_allowed_domains",
		"agent_allowed_domains = [\"https://registry.npmjs.org\"]\n":   "bad_agent_allowed_domains",
		"agent_allowed_domains = [" + strings.Join(many, ", ") + "]\n": "bad_agent_allowed_domains",
		"max_cost_usd = 0\n":                                           "bad_max_cost_usd",
		"max_cost_usd = 101\n":                                         "bad_max_cost_usd",
		"max_cost_usd = -1\n":                                          "parse", // the parser takes no sign
	} {
		writeFile(t, path, policyBase+body, 0o644)
		if p := LoadPolicy(path, false, rootUID); p.Valid || p.Reason != reason {
			t.Errorf("%q: valid=%v reason=%q", body, p.Valid, p.Reason)
		}
	}
}

// Items 2 and 5: every agent of a worker job may read its clones' .git and
// its own evidence, computed from the worker's paths, never wt/.git.
func TestJobReadOnlyPaths(t *testing.T) {
	p := Paths{StateDir: "/state"}
	canon, err := CanonicalRepoURL("https://github.com/acme/app.git")
	if err != nil {
		t.Fatal(err)
	}
	wts := []git.WorktreeBinding{
		{Repo: "app", RepoURL: "https://github.com/acme/app.git", PrimaryAbs: "/somewhere/else", WorktreeAbs: "/state/jobs/bjob_1/worktrees/app"},
		{Repo: "dup", RepoURL: canon},
	}
	got := jobReadOnly(p, "bjob_1", wts)
	want := []string{filepath.Join(p.RepoClone(canon), ".git"), "/state/jobs/bjob_1/evidence"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("read-only %v, want %v", got, want)
	}
	h := (&hostAdapter{}).host(p, "bjob_1", "", wts, []string{"registry.npmjs.org"})
	deny := h.DenyRead()
	for _, d := range deny {
		if d == "/state/jobs/bjob_1/evidence/evidence" {
			t.Fatalf("this job's evidence still denied: %v", deny)
		}
	}
	if !slices.Contains(deny, "/state/repos") || !slices.Contains(deny, "/state/jobs") {
		t.Fatalf("clones or jobs no longer denied: %v", deny)
	}
	if strings.Join(h.AllowedDomains, ",") != "registry.npmjs.org" {
		t.Fatalf("domains %v", h.AllowedDomains)
	}
}

// Criterion 1: a claude job on a workspace app runs strict, BRIEF.md and the
// prompt both say so, and NO-SCREENSHOT releases the loop. The default cost
// cap reaches the stored spec.
func TestClaudeRunsStrictAndNoScreenshotReleases(t *testing.T) {
	body := "mkdir -p fix-notes\nprintf 'fixed' > fix-notes/" + testDefect + ".md\n" +
		"printf 'plan\\nNO-SCREENSHOT: " + testDefect + "\\n' > NOTES.md\n" +
		"cat '" + claudeFixture(t, "c1-basic") + "'\nexit 0\n"
	h := newHarness(t, harnessOpts{agent: body})
	spec := fixSpec("bjob_rfa_strict")
	spec.Project.Sandbox = "workspace"
	spec.Project.SimulatorWrites = true
	spec.Caps.AgentMaxIters = 4
	h.offerJob(spec, nil)
	h.waitDone(spec.JobID, 60*time.Second)
	events := h.fs.jobEvents(spec.JobID)
	log := joinLines(events)
	if !strings.Contains(log, "sandbox strict for claude") {
		t.Errorf("no narrowing note:\n%s", log)
	}
	meta, _ := readMeta(h.paths.JobMeta(spec.JobID))
	if meta.Spec.Project.Sandbox != "strict" || meta.Spec.Project.SimulatorWrites || meta.Spec.Caps.AgentMaxCostUSD != DefaultAgentMaxCostUSD {
		t.Fatalf("stored spec app=%+v caps=%+v", meta.Spec.Project, meta.Spec.Caps)
	}
	brief, _ := os.ReadFile(filepath.Join(h.paths.Handoff(spec.JobID), "BRIEF.md"))
	if !strings.Contains(string(brief), "| Sandbox profile | **strict** (Project settings) |") || !strings.Contains(string(brief), "Network: none") {
		t.Errorf("BRIEF.md:\n%s", brief)
	}
	argv, _ := os.ReadFile(h.argv)
	if !strings.Contains(string(argv), "strict only") || strings.Contains(string(argv), "SANDBOX: --sandbox workspace") {
		t.Errorf("prompt does not say strict:\n%s", argv)
	}
	if r := lastAgentRun(events); r == nil || r.StoppedBy != dexec.StoppedByArtifacts || r.Iterations != 1 {
		t.Fatalf("agent run %+v\n%s", r, log)
	}
}

// Criterion 6: each new stop rule, through the real worker loop.
func TestStopRulesEndToEnd(t *testing.T) {
	fixture := func(t *testing.T) string { return "cat '" + claudeFixture(t, "c1-basic") + "'\nexit 0\n" }
	cases := []struct {
		name   string
		agent  func(t *testing.T) string
		mutate func(*dexec.JobSpec)
		want   string
		iters  int
		reason string
	}{
		{"blocked", func(t *testing.T) string {
			return "printf 'plan\\nBLOCKED: npm install refused: no network\\n' > NOTES.md\n" + fixture(t)
		}, nil, dexec.StoppedByBlocked, 1, "npm install refused: no network"},
		{"no progress", fixture, nil, dexec.StoppedByNoProgress, 2, ""},
		// An agent that only appends to its own log, every iteration, has
		// made no progress either.
		{"only appends to its log", func(t *testing.T) string {
			return "date >> agent-own.log\nmkdir -p .state && date > .state/session\n" + fixture(t)
		}, nil, dexec.StoppedByNoProgress, 2, ""},
		// Re-saving the same screenshot every iteration is no progress
		// (spec §4.1.6, amended 2026-11-20).
		{"re-saves the same screenshot", func(t *testing.T) string {
			return "mkdir -p fix-evidence/" + testDefect + "\nprintf '\\211PNG\\r\\n\\032\\nsame' > fix-evidence/" + testDefect + "/fix-01.png\n" + fixture(t)
		}, nil, dexec.StoppedByNoProgress, 2, ""},
		{"budget", fixture, func(s *dexec.JobSpec) { s.Caps.AgentMaxCostUSD = 0.01 }, dexec.StoppedByBudget, 1, ""},
	}
	for i, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := newHarness(t, harnessOpts{agent: c.agent(t)})
			defer h.stop()
			spec := fixSpec("bjob_rfa_stop" + string(rune('a'+i)))
			spec.Caps.AgentMaxIters = 5
			if c.mutate != nil {
				c.mutate(&spec)
			}
			h.offerJob(spec, nil)
			h.waitDone(spec.JobID, 60*time.Second)
			events := h.fs.jobEvents(spec.JobID)
			r := lastAgentRun(events)
			if r == nil || r.StoppedBy != c.want || r.Iterations != c.iters || r.StopReason != c.reason {
				t.Fatalf("agent run %+v\n%s", r, joinLines(events))
			}
			if c.want == dexec.StoppedByBudget && !strings.Contains(joinLines(events), "reached the job's cap") {
				t.Errorf("no budget line:\n%s", joinLines(events))
			}
			// A stopped loop still verifies and harvests.
			if lastHarvest(events) == nil {
				t.Fatalf("no harvest after %s", c.want)
			}
		})
	}
}

// Criterion 6, last case: a restart carries the job's cost so far, so it
// cannot buy a fresh budget. $4.99 earlier + one iteration ($0.045 in the
// fixture) reaches the $5 cap at once. Rerun and Resume share the seed in
// exec (any mode but fresh); the Rerun is the one the harness can offer.
func TestRestartKeepsCostTotal(t *testing.T) {
	h := newHarness(t, harnessOpts{agent: "cat '" + claudeFixture(t, "c1-basic") + "'\nexit 0\n"})
	spec := fixSpec("bjob_rfa_restart")
	spec.Caps.AgentMaxIters = 1
	h.offerJob(spec, nil)
	h.waitDone(spec.JobID, 60*time.Second)
	eventually(t, 5*time.Second, "acked", func() bool { return len(h.w.busyIDs()) == 0 })
	n := len(h.fs.jobEvents(spec.JobID))

	h.fs.mu.Lock()
	rs := &h.fs.jobs[spec.JobID].spec
	rs.PriorAgentRun = &dexec.AgentRun{Iterations: 8, TotalCostUSD: 4.99, Runner: "claude"}
	rs.Caps.AgentMaxIters, rs.Caps.AgentMaxCostUSD = 5, 5
	h.fs.mu.Unlock()
	h.fs.offerMode(spec.JobID, "ls_restart2", "dd_wjt_restart2", dexec.ModeRerun)
	eventually(t, 60*time.Second, "second done", func() bool {
		ev := h.fs.jobEvents(spec.JobID)
		return len(ev) > n && lastIsDone(ev)
	})
	events := h.fs.jobEvents(spec.JobID)[n:]
	r := lastAgentRun(events)
	if r == nil || r.StoppedBy != dexec.StoppedByBudget || r.Iterations != 9 || r.TotalCostUSD < 5 {
		t.Fatalf("agent run %+v\n%s", r, joinLines(events))
	}
}

// Criterion 7: a verify skip by host policy is logged with the count, is
// recorded, and resolves nothing; a server-sent VerifySkipped is dropped.
func TestVerifySkippedByHostPolicy(t *testing.T) {
	pol := strings.Replace(defaultPolicy, "allow_verify_commands = true", "allow_verify_commands = false", 1)
	h := newHarness(t, harnessOpts{policy: pol})
	spec := fixSpec("bjob_rfa_vskip")
	spec.Project.VerifyCommands = []dexec.VerifyCommand{{Repo: "app", Command: "true"}, {Repo: "app", Command: "false"}}
	h.offerJob(spec, nil)
	h.waitDone(spec.JobID, 60*time.Second)
	events := h.fs.jobEvents(spec.JobID)
	log := joinLines(events)
	if !strings.Contains(log, "verify: skipped by host policy (2 command(s) configured)") {
		t.Fatalf("no skip line:\n%s", log)
	}
	hv := lastHarvest(events)
	if hv == nil || hv.AllowResolve || hv.Verification == nil || hv.Verification.Ran || hv.Verification.SkippedReason != "host policy" {
		t.Fatalf("harvest %+v verification %+v", hv, hv.Verification)
	}

	// Allowed verify: a VerifySkipped from the server never reaches the run.
	h2 := newHarness(t, harnessOpts{})
	spec2 := fixSpec("bjob_rfa_vforged")
	spec2.Project.VerifyCommands = []dexec.VerifyCommand{{Repo: "app", Command: "true"}}
	spec2.Project.VerifySkipped, spec2.Project.VerifySkippedCommands = "host policy", 9
	h2.offerJob(spec2, nil)
	h2.waitDone(spec2.JobID, 60*time.Second)
	if hv := lastHarvest(h2.fs.jobEvents(spec2.JobID)); hv == nil || !hv.AllowResolve || !hv.Verification.Ran {
		t.Fatalf("forged skip honoured: %+v", hv)
	}
}

// D5: the worker's policy lowers the server's cap, never raises it, and
// stands in for a server that sends none.
func TestCostCapResolution(t *testing.T) {
	for _, c := range []struct {
		policy string
		server float64
		want   float64
		note   bool
	}{
		{"", 0, DefaultAgentMaxCostUSD, false},
		{"", 7.5, 7.5, false},
		{"max_cost_usd = 3\n", 0, 3, false},
		{"max_cost_usd = 3\n", 5, 3, true},
		{"max_cost_usd = 9\n", 5, 5, false},
	} {
		h := newHarness(t, harnessOpts{policy: defaultPolicy + c.policy})
		spec := fixSpec("bjob_rfa_cap")
		spec.Caps.AgentMaxCostUSD = c.server
		h.offerJob(spec, nil)
		h.waitDone(spec.JobID, 60*time.Second)
		meta, _ := readMeta(h.paths.JobMeta(spec.JobID))
		if meta.Spec.Caps.AgentMaxCostUSD != c.want {
			t.Errorf("policy %q server %v: cap %v, want %v", c.policy, c.server, meta.Spec.Caps.AgentMaxCostUSD, c.want)
		}
		if got := strings.Contains(joinLines(h.fs.jobEvents(spec.JobID)), "max cost clamped"); got != c.note {
			t.Errorf("policy %q server %v: clamp note %v", c.policy, c.server, got)
		}
		h.stop()
	}
}

// F9: the worker names why a socket ended.
func TestSessionEndReason(t *testing.T) {
	for _, c := range []struct {
		err  error
		want string
	}{
		{closeError{code: workerproto.CloseRevoked}, "socket closed: code 4001 (revoked)"},
		{closeError{code: workerproto.CloseReplaced}, "socket closed: code 4000 (replaced)"},
		{websocket.CloseError{Code: websocket.StatusGoingAway, Reason: "server restart"}, "socket closed: code 1001 (StatusGoingAway): server restart"},
		// The read loop's own wrapping (closeErr) keeps the server's reason.
		{closeErr(websocket.CloseError{Code: websocket.StatusGoingAway, Reason: "server restart"}), "socket closed: code 1001 (StatusGoingAway): server restart"},
		{dialError{errors.New("dial tcp: connection refused")}, "connect failed: dial tcp: connection refused"},
		{errors.New("failed to read frame header: EOF"), "socket error: failed to read frame header: EOF"},
	} {
		if got := sessionEndReason(c.err); got != c.want {
			t.Errorf("%v: %q, want %q", c.err, got, c.want)
		}
	}
}

// §4.1.11 (F11): a worker job may not write the worker's own files or
// $HOME; the list comes from Paths. A $HOME that holds the state dir is
// left out (it would cover the agent's own dirs).
func TestJobDenyWrite(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	p := Paths{ConfigDir: "/etc/w", StateDir: "/state"}
	got := (&hostAdapter{}).host(p, "bjob_1", "", nil, nil).DenyWrite
	want := []string{p.StatusFile(), p.ReadinessDir(), home}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("deny-write %v, want %v", got, want)
	}
	if p.StatusFile() != "/state/status.json" || p.ReadinessDir() != "/state/readiness" {
		t.Fatalf("paths %s %s", p.StatusFile(), p.ReadinessDir())
	}
	p.StateDir = filepath.Join(home, "state")
	if got := jobDenyWrite(p); slices.Contains(got, home) {
		t.Fatalf("$HOME holding the state dir was denied: %v", got)
	}
}

// Guard for §4.1.11: after a real job, every entry directly in {state} is
// either masked by DenyRead or listed in DenyWrite, so a new worker file
// cannot slip through unnoticed.
func TestStateEntriesMaskedOrDenied(t *testing.T) {
	h := newHarness(t, harnessOpts{})
	spec := fixSpec("bjob_rfa_state")
	h.offerJob(spec, nil)
	h.waitDone(spec.JobID, 60*time.Second)
	eventually(t, 5*time.Second, "status.json", func() bool { _, err := os.Stat(h.paths.StatusFile()); return err == nil })
	host := (&hostAdapter{}).host(h.paths, spec.JobID, "", nil, nil)
	covered := append(host.DenyRead(), host.DenyWrite...)
	ents, err := os.ReadDir(h.paths.StateDir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range ents {
		names = append(names, e.Name())
	}
	// Not vacuous: the job and the worker wrote their usual entries.
	for _, want := range []string{"status.json", "readiness", "jobs", "repos", "credential"} {
		if !slices.Contains(names, want) {
			t.Fatalf("{state} lacks %s after a job: %v", want, names)
		}
	}
	for _, e := range ents {
		if p := filepath.Join(h.paths.StateDir, e.Name()); !slices.Contains(covered, p) {
			t.Errorf("{state}/%s is neither masked nor write-denied", e.Name())
		}
	}
}
