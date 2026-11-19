//go:build linux

package worker

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	aa "github.com/more-than-code/defect-drainer-worker/agentadapter"
	"github.com/more-than-code/defect-drainer-worker/agentadapter/claude"
	dexec "github.com/more-than-code/defect-drainer-worker/exec"
	"github.com/more-than-code/defect-drainer-worker/git"
)

// TestSandboxLayoutBwrap is criteria 2 and 5 of the remote-job-fixes spec on
// Linux. Claude's own sandbox needs the real CLI, so this test cannot run it
// headless. It does the next best thing: it builds the real settings file
// for a worker job (worker host() → exec.Host → claude.Settings), turns its
// filesystem rules into a bubblewrap command line the way the sandbox applies
// them (denyRead hidden, allowRead re-allowed read-only, Edit allows
// writable), and runs a fake agent's commands through it. It proves the
// layout and our rules, not Claude's translation of them (probe P1).
//
// Run by packaging/test/sandbox_test.sh in a container with bwrap and git;
// skipped elsewhere.
func TestSandboxLayoutBwrap(t *testing.T) {
	if os.Getenv("DD_SANDBOX_E2E") != "1" {
		t.Skip("set DD_SANDBOX_E2E=1 (packaging/test/sandbox_test.sh)")
	}
	if _, err := exec.LookPath("bwrap"); err != nil {
		t.Fatal("bwrap not installed")
	}
	tmp := t.TempDir()
	// The worker's $HOME, outside {state} as in the packaged layout, with a
	// file to protect and a denyRead-masked dir.
	home := filepath.Join(tmp, "home")
	t.Setenv("HOME", home)
	writeFile(t, filepath.Join(home, "keep.txt"), "home-bytes", 0o600)
	writeFile(t, filepath.Join(home, ".claude", "settings.json"), "{}", 0o600)
	p := Paths{ConfigDir: filepath.Join(tmp, "etc"), StateDir: filepath.Join(tmp, "state")}
	run := func(dir string, args ...string) string {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com",
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	origin := filepath.Join(tmp, "origin")
	_ = os.MkdirAll(origin, 0o755)
	run(origin, "init", "-q", "-b", "main")
	writeFile(t, filepath.Join(origin, "a.txt"), "one\n", 0o644)
	run(origin, "add", "a.txt")
	run(origin, "commit", "-q", "-m", "init")
	base := run(origin, "rev-parse", "HEAD")

	canonA, _ := CanonicalRepoURL("https://github.com/acme/app")
	canonB, _ := CanonicalRepoURL("https://github.com/acme/other")
	cloneA, cloneB := p.RepoClone(canonA), p.RepoClone(canonB)
	_ = os.MkdirAll(p.ReposDir(), 0o700)
	run(tmp, "clone", "-q", "--no-checkout", origin, cloneA)
	run(tmp, "clone", "-q", "--no-checkout", origin, cloneB)
	wt, _ := p.Worktree("bjob_sbx1", "app")
	_ = os.MkdirAll(p.WorktreeRoot("bjob_sbx1"), 0o755)
	run(cloneA, "worktree", "add", "-q", "-b", "dd/bjob_sbx1/app", wt, "origin/main")
	handoff := p.Handoff("bjob_sbx1")
	writeFile(t, filepath.Join(handoff, "BRIEF.md"), "brief\n", 0o644)
	mine := filepath.Join(p.Evidence("bjob_sbx1"), "evidence", "DEF-1", "shot.png")
	theirs := filepath.Join(p.Evidence("bjob_sbx2"), "evidence", "DEF-2", "shot.png")
	writeFile(t, mine, "png-mine", 0o600)
	writeFile(t, theirs, "png-theirs", 0o600)
	cred := p.Env()
	writeFile(t, cred, "DD_WORKER_CREDENTIAL=secret\n", 0o600)
	// The worker's own files at the state root (F11).
	status, ready := p.StatusFile(), filepath.Join(p.ReadinessDir(), "claude", "check.json")
	writeFile(t, status, "status-bytes", 0o600)
	writeFile(t, ready, "ready-bytes", 0o600)
	protected := map[string]string{status: "status-bytes", ready: "ready-bytes", filepath.Join(home, "keep.txt"): "home-bytes"}

	wts := []git.WorktreeBinding{{Repo: "app", RepoURL: canonA, PrimaryAbs: cloneA, WorktreeAbs: wt, Branch: "dd/bjob_sbx1/app"}}
	host := (&hostAdapter{}).host(p, "bjob_sbx1", "", wts, nil)
	if !slices.Contains(host.DenyWrite, home) || !slices.Contains(host.DenyWrite, status) {
		t.Fatalf("worker deny-write %v", host.DenyWrite)
	}
	implementer := aa.Task{Role: aa.Implementer, Handoff: handoff, Worktrees: []string{wt}, ReadOnly: host.ReadOnly, DenyRead: host.DenyRead(), DenyWrite: host.DenyWrite}
	lens := filepath.Join(handoff, "review", "spec")
	_ = os.MkdirAll(lens, 0o755)
	reviewer := aa.Task{Role: aa.Reviewer, Handoff: lens, Worktrees: []string{wt},
		ReadOnly: append(dexec.ReviewReadOnly(handoff), host.ReadOnly...), DenyRead: host.DenyRead(), DenyWrite: host.DenyWrite}

	// Negative control: the same job under the rc1 rules (no ReadOnly) must
	// reproduce F2 and F4, or the checks below prove nothing.
	oldHost := host
	oldHost.ReadOnly = nil
	before := aa.Task{Role: aa.Implementer, Handoff: handoff, Worktrees: []string{wt}, DenyRead: oldHost.DenyRead()}
	// And under b86213b's rules (no DenyWrite) the worker's own files are
	// writable: F11. Opened for append only, so no bytes change.
	noDenyWrite := implementer
	noDenyWrite.DenyWrite = nil
	masked := filepath.Join(p.JobsDir(), "sibling-probe")
	maskedHome := filepath.Join(home, ".claude", "probe")
	residual := filepath.Join(p.StateDir, "new-root-file")

	type check struct {
		what string
		sh   string
		ok   bool
	}
	gitc := "git -c user.name=t -c user.email=t@example.com "
	for _, c := range []struct {
		task   aa.Task
		checks []check
	}{
		{before, []check{
			{"rc1 rules: git status fails (F2)", "git -C " + wt + " status --porcelain", false},
			{"rc1 rules: report evidence unreadable (F4)", "cat " + mine, false},
		}},
		{noDenyWrite, []check{
			{"b86213b rules: status.json writable (F11)", "( : >> " + status + " )", true},
			{"b86213b rules: $HOME writable (F11)", "( : >> " + home + "/keep.txt )", true},
		}},
		{implementer, []check{
			{"git status in the worktree", gitc + "-C " + wt + " status --porcelain", true},
			{"edit in the worktree", "echo two > " + wt + "/a.txt", true},
			{"git diff in the worktree", gitc + "-C " + wt + " diff --exit-code >/dev/null; [ $? -eq 1 ]", true},
			{"read this job's report evidence", "cat " + mine, true},
			{"write into the clone's .git", "touch " + cloneA + "/.git/planted", false},
			{"commit (writes the clone's .git)", gitc + "-C " + wt + " commit -qam x", false},
			{"read another repo's clone", "cat " + cloneB + "/.git/HEAD", false},
			{"read another job's evidence", "cat " + theirs, false},
			{"read the worker credential", "cat " + cred, false},
			{"write the evidence", "echo x > " + mine, false},
			// §4.1.11 / criterion 2 (amended): the worker's own files and
			// $HOME are read-only; their bytes are checked after the run.
			{"write status.json", "echo x > " + status, false},
			{"write into readiness/", "echo x > " + ready, false},
			{"new file in readiness/", "echo x > " + p.ReadinessDir() + "/planted", false},
			{"write $HOME", "echo x > " + home + "/keep.txt", false},
			{"new file in $HOME", "echo x > " + home + "/planted", false},
			{"npm-style cache in the handoff", "mkdir -p " + handoff + "/.cache/npm && echo x > " + handoff + "/.cache/npm/c", true},
			// Masked dirs are private empty views: the write works inside and
			// never reaches the disk (checked below).
			{"write into a masked dir ({state}/jobs)", "echo x > " + masked, true},
			{"write into a masked dir ($HOME/.claude)", "echo x > " + maskedHome, true},
			// The accepted residual: a new file directly in {state}.
			{"new file directly in {state} (residual)", "echo x > " + residual, true},
		}},
		{reviewer, []check{
			{"reviewer git diff <base>", gitc + "-C " + wt + " diff " + base + " >/dev/null", true},
			{"reviewer reads BRIEF.md", "cat " + handoff + "/BRIEF.md", true},
			{"reviewer reads this job's evidence", "cat " + mine, true},
			{"reviewer writes the worktree", "echo x > " + wt + "/b.txt", false},
			{"reviewer reads another clone", "cat " + cloneB + "/.git/HEAD", false},
			{"reviewer writes status.json", "echo x > " + status, false},
			{"reviewer writes $HOME", "echo x > " + home + "/keep.txt", false},
		}},
	} {
		body, err := claude.Settings(c.task)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(body), `"/`+p.ReposDir()+`"`) {
			t.Fatalf("{state}/repos left denyRead: %s", body)
		}
		args := bwrapArgs(t, body)
		for _, ch := range c.checks {
			cmd := exec.Command("bwrap", append(args, "sh", "-c", ch.sh)...)
			out, err := cmd.CombinedOutput()
			if (err == nil) != ch.ok {
				t.Errorf("%s: ok=%v, want %v\n%s", ch.what, err == nil, ch.ok, out)
			} else {
				t.Logf("%s: ok=%v as expected", ch.what, err == nil)
			}
		}
	}
	for f, want := range protected {
		if b, err := os.ReadFile(f); err != nil || string(b) != want {
			t.Errorf("%s changed: %q %v", f, b, err)
		}
	}
	for _, f := range []string{masked, maskedHome, filepath.Join(p.ReadinessDir(), "planted"), filepath.Join(home, "planted")} {
		if _, err := os.Lstat(f); err == nil {
			t.Errorf("%s reached the real disk", f)
		}
	}
	if _, err := os.Lstat(residual); err != nil {
		t.Errorf("the residual changed (a new {state} file is now refused?): update the docs: %v", err)
	}
}

// bwrapArgs applies a Claude settings file's filesystem rules the way probe
// V3 observed them: the host writable (the worker user can write its own
// state, F11), every existing denyWrite path read-only, every existing
// denyRead path hidden (an empty private tmpfs, or /dev/null over a file;
// mounted after denyWrite so a masked dir inside $HOME stays a throwaway
// view), every allowRead path bound back read-only on top, and every Edit
// allow bound writable last. No tmpfs over /tmp: the test tree lives there,
// so only the rules may hide it.
func bwrapArgs(t *testing.T, body []byte) []string {
	t.Helper()
	var s struct {
		Permissions struct {
			Allow []string `json:"allow"`
		} `json:"permissions"`
		Sandbox struct {
			Filesystem struct {
				DenyRead  []string `json:"denyRead"`
				AllowRead []string `json:"allowRead"`
				DenyWrite []string `json:"denyWrite"`
			} `json:"filesystem"`
		} `json:"sandbox"`
	}
	if err := json.Unmarshal(body, &s); err != nil {
		t.Fatal(err)
	}
	path := func(rule string) string { return strings.TrimPrefix(rule, "/") }
	args := []string{"--die-with-parent", "--unshare-net", "--bind", "/", "/", "--dev", "/dev", "--proc", "/proc"}
	for _, d := range s.Sandbox.Filesystem.DenyWrite {
		if _, err := os.Stat(path(d)); err == nil {
			args = append(args, "--ro-bind", path(d), path(d))
		}
	}
	for _, d := range s.Sandbox.Filesystem.DenyRead {
		fi, err := os.Stat(path(d))
		switch {
		case err != nil:
		case fi.IsDir():
			args = append(args, "--tmpfs", path(d))
		default:
			args = append(args, "--ro-bind", "/dev/null", path(d))
		}
	}
	for _, a := range s.Sandbox.Filesystem.AllowRead {
		if _, err := os.Stat(path(a)); err == nil {
			args = append(args, "--ro-bind", path(a), path(a))
		}
	}
	for _, r := range s.Permissions.Allow {
		if rest, ok := strings.CutPrefix(r, "Edit("); ok {
			dir := path(strings.TrimSuffix(strings.TrimSuffix(rest, ")"), "/**"))
			args = append(args, "--bind", dir, dir)
		}
	}
	return args
}
