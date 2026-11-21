package worker

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	dexec "github.com/more-than-code/defect-drainer-worker/exec"
	"github.com/more-than-code/defect-drainer-worker/git"
	"github.com/more-than-code/defect-drainer-worker/workerproto"
)

// Worker protocol v1 §16: a verify_submitted job checks a ref pushed from
// outside DD. These tests use a local bare origin (setupGit) and a fake
// claude; no real agent CLI runs.

// vsChecks: the first is red on main and green once fixed.txt exists; the
// second is green on main and red once broken.txt exists.
var vsChecks = []dexec.VerifyCommand{
	{Repo: "app", Command: "test -f fixed.txt"},
	{Repo: "app", Command: "test ! -f broken.txt"},
}

// vsSpec is a remote verify_submitted spec for ref.
func vsSpec(jobID, ref string) dexec.JobSpec {
	s := fixSpec(jobID)
	s.Mode = dexec.ModeVerifySubmitted
	s.Worktrees[0].Branch = ""
	s.SubmittedRefs = []dexec.SubmittedRef{{Repo: "app", Ref: ref}}
	s.App.VerifyCommands = vsChecks
	return s
}

// gitIn runs git in dir with the test's git env and returns its output.
func gitIn(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// pushBranch commits files on a new branch of the source repo (from main,
// or with no parent when orphan) and pushes it to the origin. It returns
// the commit.
func pushBranch(t *testing.T, tmp, branch string, orphan bool, files ...string) string {
	t.Helper()
	src := filepath.Join(tmp, "src")
	gitIn(t, src, "checkout", "-q", "main")
	if orphan {
		gitIn(t, src, "checkout", "-q", "--orphan", branch)
		gitIn(t, src, "rm", "-rq", "--cached", ".")
	} else {
		gitIn(t, src, "checkout", "-q", "-b", branch)
	}
	for _, f := range files {
		writeFile(t, filepath.Join(src, f), f+"\n", 0o644)
		gitIn(t, src, "add", f)
	}
	gitIn(t, src, "commit", "-q", "-m", "submitted "+branch)
	gitIn(t, src, "push", "-q", filepath.Join(tmp, "origin", "app.git"), branch)
	sha := gitIn(t, src, "rev-parse", "HEAD")
	gitIn(t, src, "checkout", "-q", "-f", "main")
	return sha
}

// originRefs lists every ref of the bare origin with its commit.
func originRefs(t *testing.T, tmp string) string {
	t.Helper()
	return gitIn(t, filepath.Join(tmp, "origin", "app.git"), "for-each-ref", "--format=%(refname) %(objectname)")
}

// noWriteMints fails the test if any write token was asked for.
func noWriteMints(t *testing.T, h *harness) {
	t.Helper()
	for _, m := range h.fs.mints() {
		if m.access != workerproto.GitAccessRead {
			t.Fatalf("a %s token was minted for %s", m.access, m.repo)
		}
	}
}

// reviewerAgent is a fake claude that records each call as "review" or
// "other" and, as a reviewer, reads the worktree's diff and answers with
// no findings.
func reviewerAgent(t *testing.T, tmp, calls, wt string) string {
	t.Helper()
	real, err := filepath.EvalSymlinks(tmp)
	if err != nil {
		t.Fatal(err)
	}
	realWT := filepath.Join(real, strings.TrimPrefix(wt, tmp))
	stream := strings.Join([]string{
		`{"type":"system","subtype":"init","session_id":"vs-review-1"}`,
		`{"type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","id":"toolu_vs1","name":"Bash","input":{"command":"git -C ` + wt + ` diff; git -C ` + realWT + ` diff"}}]}}`,
		`{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_vs1","content":"diff --git a/fixed.txt b/fixed.txt","is_error":false}]}}`,
		`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"{\"findings\":[]}"}]}}`,
		`{"type":"result","subtype":"success","is_error":false,"num_turns":2,"session_id":"vs-review-1","total_cost_usd":0.01,"result":"{\"findings\":[]}"}`,
	}, "\n")
	return "case \"$*\" in *'Read REVIEW.md'*) echo review >> '" + calls + "';; *) echo other >> '" + calls + "';; esac\n" +
		"cat <<'EOF'\n" + stream + "\nEOF\nexit 0\n"
}

// A submitted branch that fixes the check passes verify and the review; the
// job ends with the usual verdict, only the reviewer ran, and nothing was
// pushed or written to the origin.
func TestVerifySubmittedPasses(t *testing.T) {
	id := "bjob_vs_pass"
	h := newHarness(t, harnessOpts{noStart: true})
	calls := filepath.Join(h.tmp, "calls.log")
	wt := filepath.Join(h.paths.WorktreeRoot(id), "app")
	h.agent = fakeAgent(t, h.tmp, h.argv, reviewerAgent(t, h.tmp, calls, wt))
	h.start(Timing{})
	sha := pushBranch(t, h.tmp, "fix-good", false, "fixed.txt")
	mainSHA := gitIn(t, filepath.Join(h.tmp, "src"), "rev-parse", "main")
	before := originRefs(t, h.tmp)

	spec := vsSpec(id, "fix-good")
	spec.Caps.ReviewOff, spec.Caps.ReviewMaxTurns, spec.Caps.ReviewMaxWallSec = false, 5, 60
	h.offerJob(spec, nil)
	d := h.waitDone(id, 90*time.Second)
	events := h.fs.jobEvents(id)
	log := joinLines(events)
	if d.State != "completed" || !strings.Contains(d.Line, "nothing pushed") {
		t.Fatalf("done = %+v\n%s", d, log)
	}

	var running *dexec.Status
	var verification *dexec.VerificationRun
	var review *dexec.ReviewRun
	for _, e := range events {
		switch {
		case e.Kind == dexec.EventAgentRun:
			t.Fatalf("an agent_run event in verify_submitted: %+v", e.AgentRun)
		case e.Status != nil && e.Status.Phase == dexec.PhaseRunning:
			running = e.Status
		case e.Kind == dexec.EventVerification:
			verification = e.Verification
		case e.Review != nil:
			review = e.Review
		}
	}
	if running == nil || running.SubmittedHeads["app"] != sha || running.DiffBase["app"] != mainSHA {
		t.Fatalf("running = %+v, want head %s and merge-base %s", running, sha, mainSHA)
	}
	if verification == nil || !verification.Ran || !verification.Ok {
		t.Fatalf("verification = %+v\n%s", verification, log)
	}
	if v := dexec.JudgeVerification(verification, baselineOf(events)); !v.Ok {
		t.Fatalf("verdict %+v", v)
	}
	if review == nil || !review.OK || len(review.Lenses) == 0 {
		t.Fatalf("review = %+v\n%s", review, log)
	}
	hv := lastHarvest(events)
	if hv == nil || !hv.AllowResolve || hv.Verification == nil || hv.Review == nil {
		t.Fatalf("harvest = %+v", hv)
	}
	// Only the reviewer ran: one call per lens, no implementer.
	b, _ := os.ReadFile(calls)
	got := strings.Fields(string(b))
	if len(got) != len(review.Lenses) || slices.Contains(got, "other") {
		t.Fatalf("agent calls %v for %d lenses", got, len(review.Lenses))
	}
	// The submitted worktree is detached at the ref; the baseline ran in its
	// own worktree at the base.
	if head := gitIn(t, wt, "rev-parse", "HEAD"); head != sha {
		t.Fatalf("worktree at %s, want %s", head, sha)
	}
	if br := gitIn(t, wt, "rev-parse", "--abbrev-ref", "HEAD"); br != "HEAD" {
		t.Fatalf("worktree on branch %q", br)
	}
	bwt := filepath.Join(h.paths.BaselineRoot(id), "app")
	if head := gitIn(t, bwt, "rev-parse", "HEAD"); head != mainSHA {
		t.Fatalf("baseline worktree at %s, want %s", head, mainSHA)
	}
	if after := originRefs(t, h.tmp); after != before {
		t.Fatalf("origin refs changed:\n%s\n---\n%s", before, after)
	}
	noWriteMints(t, h)

	// Ship and resume are refused; nothing reaches the origin.
	ship := dexec.Control{Kind: dexec.ControlShip, Repos: []dexec.ShipRepo{{Repo: "app", Files: []string{"fixed.txt"}, Message: "m", Title: "t"}}}
	if r := h.controlAndWait(id, "rq_vs_ship", ship, 1); r.OK || r.Code != workerproto.DeclineBadSpec {
		t.Fatalf("ship of a verify_submitted job: %+v", r)
	}
	if r := h.controlAndWait(id, "rq_vs_resume", dexec.Control{Kind: dexec.ControlResume, SpecURL: "/v1/jobs/" + id + "/spec"}, 1); r.OK || r.Code != workerproto.DeclineBadSpec {
		t.Fatalf("resume of a verify_submitted job: %+v", r)
	}
	h.fs.offerMode(id, "ls_vs_rerun", "dd_wjt_vs_rerun", dexec.ModeRerun)
	eventually(t, 10*time.Second, "rerun declined", func() bool { return h.fs.declineFor(id) == workerproto.DeclineBadSpec })
	if after := originRefs(t, h.tmp); after != before {
		t.Fatalf("origin refs changed after ship:\n%s", after)
	}
	noWriteMints(t, h)
}

func baselineOf(events []dexec.Event) *dexec.VerificationRun {
	var b *dexec.VerificationRun
	for _, e := range events {
		if e.Baseline != nil {
			b = e.Baseline
		}
	}
	return b
}

// A branch that breaks a check that is green on the base is held: the
// verdict names the regression, and nothing is pushed.
func TestVerifySubmittedRegressionHeld(t *testing.T) {
	id := "bjob_vs_regress"
	h := newHarness(t, harnessOpts{})
	sha := pushBranch(t, h.tmp, "fix-bad", false, "fixed.txt", "broken.txt")
	before := originRefs(t, h.tmp)
	h.offerJob(vsSpec(id, sha), nil) // by commit id this time
	d := h.waitDone(id, 60*time.Second)
	events := h.fs.jobEvents(id)
	log := joinLines(events)
	if d.State != "completed" {
		t.Fatalf("done = %+v\n%s", d, log)
	}
	hv := lastHarvest(events)
	if hv == nil || hv.Verification == nil || hv.Verification.Ok {
		t.Fatalf("harvest = %+v\n%s", hv, log)
	}
	v := dexec.JudgeVerification(hv.Verification, baselineOf(events))
	if v.Ok || v.Regressions != 1 {
		t.Fatalf("verdict %+v", v)
	}
	if !strings.Contains(log, "verify: "+dexec.SummarizeVerdict(v)) {
		t.Fatalf("no verdict line:\n%s", log)
	}
	if after := originRefs(t, h.tmp); after != before {
		t.Fatalf("origin refs changed:\n%s", after)
	}
	noWriteMints(t, h)
}

// A ref the origin lacks, a commit id it lacks, and a branch with no
// history in common with the base each fail before any check, with the
// reason, and push nothing.
func TestVerifySubmittedFailsCleanly(t *testing.T) {
	h := newHarness(t, harnessOpts{})
	pushBranch(t, h.tmp, "orphan", true, "fixed.txt")
	before := originRefs(t, h.tmp)
	for _, c := range []struct{ id, ref, want string }{
		{"bjob_vs_noref", "no-such-branch", "submitted_ref_missing: app no-such-branch"},
		{"bjob_vs_nosha", strings.Repeat("ab", 20), "submitted_ref_missing: app " + strings.Repeat("ab", 20)},
		{"bjob_vs_orphan", "orphan", "no_merge_base: app orphan"},
	} {
		h.offerJob(vsSpec(c.id, c.ref), nil)
		d := h.waitDone(c.id, 60*time.Second)
		events := h.fs.jobEvents(c.id)
		if d.State != "failed" || d.Line != c.want {
			t.Fatalf("%s: done = %+v, want %q\n%s", c.id, d, c.want, joinLines(events))
		}
		for _, e := range events {
			if e.Kind == dexec.EventBaseline || e.Kind == dexec.EventVerification || e.Kind == dexec.EventAgentRun {
				t.Fatalf("%s: %s event after a failed fetch", c.id, e.Kind)
			}
		}
	}
	if after := originRefs(t, h.tmp); after != before {
		t.Fatalf("origin refs changed:\n%s", after)
	}
	noWriteMints(t, h)
	if b, _ := os.ReadFile(h.argv); len(b) != 0 {
		t.Fatalf("the agent ran:\n%s", b)
	}
}

// hello advertises the capability.
func TestHelloAdvertisesVerifySubmitted(t *testing.T) {
	h := newHarness(t, harnessOpts{})
	defer h.stop()
	if caps := h.fs.hellos[0].Capabilities; !slices.Contains(caps, workerproto.CapVerifySubmittedV1) {
		t.Fatalf("hello capabilities = %v", caps)
	}
}

// validateSpec: the shape §16 accepts and the ones it refuses.
func TestValidateSpecVerifySubmitted(t *testing.T) {
	ok := vsSpec("bjob_vs_v", "fix-good")
	if err := validateSpec(ok); err != nil {
		t.Fatalf("valid spec refused: %v", err)
	}
	sha := vsSpec("bjob_vs_v", strings.Repeat("0a", 20))
	if err := validateSpec(sha); err != nil {
		t.Fatalf("commit id refused: %v", err)
	}
	cases := map[string]func(s *dexec.JobSpec){
		"no submittedRefs": func(s *dexec.JobSpec) { s.SubmittedRefs = nil },
		"extra repo": func(s *dexec.JobSpec) {
			s.SubmittedRefs = append(s.SubmittedRefs, dexec.SubmittedRef{Repo: "other", Ref: "x"})
		},
		"wrong repo": func(s *dexec.JobSpec) { s.SubmittedRefs[0].Repo = "other" },
		"duplicate repo": func(s *dexec.JobSpec) {
			s.Worktrees = append(s.Worktrees, s.Worktrees[0])
			s.Worktrees[1].Repo = "web"
			s.SubmittedRefs = append(s.SubmittedRefs, s.SubmittedRefs[0])
		},
		"empty ref":            func(s *dexec.JobSpec) { s.SubmittedRefs[0].Ref = "" },
		"option ref":           func(s *dexec.JobSpec) { s.SubmittedRefs[0].Ref = "--upload-pack=x" },
		"dotdot ref":           func(s *dexec.JobSpec) { s.SubmittedRefs[0].Ref = "a..b" },
		"job branch":           func(s *dexec.JobSpec) { s.Worktrees[0].Branch = "dd/" + s.JobID + "/app" },
		"diffBase from server": func(s *dexec.JobSpec) { s.DiffBase = map[string]string{"app": strings.Repeat("0a", 20)} },
		"baseline from server": func(s *dexec.JobSpec) { s.Baseline = &dexec.VerificationRun{} },
		"prior agent run":      func(s *dexec.JobSpec) { s.PriorAgentRun = &dexec.AgentRun{} },
		"resume session":       func(s *dexec.JobSpec) { s.ResumeSession = "abc" },
		"baselineWorktrees":    func(s *dexec.JobSpec) { s.BaselineWorktrees = []git.WorktreeBinding{s.Worktrees[0]} },
		"test run kind":        func(s *dexec.JobSpec) { s.Kind = dexec.KindTestRun },
	}
	for name, mut := range cases {
		s := vsSpec("bjob_vs_v", "fix-good")
		mut(&s)
		if err := validateSpec(s); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	// Submitted refs only travel with the mode, and a server never sends
	// baseline worktrees for any mode.
	fresh := fixSpec("bjob_vs_v")
	fresh.SubmittedRefs = []dexec.SubmittedRef{{Repo: "app", Ref: "main"}}
	if err := validateSpec(fresh); err == nil {
		t.Error("submittedRefs on a fresh spec accepted")
	}
	fresh = fixSpec("bjob_vs_v")
	fresh.BaselineWorktrees = fresh.Worktrees
	if err := validateSpec(fresh); err == nil {
		t.Error("baselineWorktrees on a fresh spec accepted")
	}
	if err := validateSpec(fixSpec("bjob_vs_v")); err != nil {
		t.Fatalf("rc4 fresh spec refused: %v", err)
	}
}
