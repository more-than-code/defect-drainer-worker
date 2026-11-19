package exec

import (
	"errors"
	"os"
	osexec "os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/more-than-code/defect-drainer-worker/git"
)

// The stop order (RF-a §4.1 item 6): exit → artifacts → blocked →
// no_progress → budget → wall → iterations. Each row sets its rule and
// every rule after it, so the row's own rule must win.
func TestNextLoopStopOrder(t *testing.T) {
	all := func() LoopState {
		return LoopState{
			Iter: 8, MaxIters: 8, Elapsed: time.Hour, MaxWall: time.Minute,
			ExitErr: errors.New("exit 1"), Complete: true, Blocked: true, NoProgress: true,
			CostUSD: 6, MaxCostUSD: 5,
		}
	}
	cases := []struct {
		want  string
		unset func(*LoopState)
	}{
		{StoppedByExit, func(*LoopState) {}},
		{StoppedByArtifacts, func(s *LoopState) { s.ExitErr = nil }},
		{StoppedByBlocked, func(s *LoopState) { s.ExitErr, s.Complete = nil, false }},
		{StoppedByNoProgress, func(s *LoopState) { s.ExitErr, s.Complete, s.Blocked = nil, false, false }},
		{StoppedByBudget, func(s *LoopState) { s.ExitErr, s.Complete, s.Blocked, s.NoProgress = nil, false, false, false }},
		{StoppedByWall, func(s *LoopState) {
			s.ExitErr, s.Complete, s.Blocked, s.NoProgress, s.CostUSD = nil, false, false, false, 1
		}},
		{StoppedByIterations, func(s *LoopState) {
			s.ExitErr, s.Complete, s.Blocked, s.NoProgress, s.CostUSD, s.Elapsed = nil, false, false, false, 1, 0
		}},
		{"", func(s *LoopState) {
			s.ExitErr, s.Complete, s.Blocked, s.NoProgress, s.CostUSD, s.Elapsed, s.Iter = nil, false, false, false, 1, 0, 1
		}},
	}
	for _, c := range cases {
		st := all()
		c.unset(&st)
		if got := NextLoopStop(st); got != c.want {
			t.Errorf("want %q, got %q for %+v", c.want, got, st)
		}
	}
	// Same-iteration pairs (Main Dev, 2026-11-20): the more specific reason
	// wins. A stuck job at the iteration cap is no_progress; a job over its
	// cost cap and its wall clock is budget.
	for _, c := range []struct {
		name string
		st   LoopState
		want string
	}{
		{"no_progress + iterations", LoopState{Iter: 8, MaxIters: 8, MaxWall: time.Hour, NoProgress: true}, StoppedByNoProgress},
		{"budget + wall", LoopState{Iter: 1, MaxIters: 8, Elapsed: time.Hour, MaxWall: time.Minute, CostUSD: 5, MaxCostUSD: 5}, StoppedByBudget},
	} {
		if got := NextLoopStop(c.st); got != c.want {
			t.Errorf("%s: %q, want %q", c.name, got, c.want)
		}
	}
	// Turn exhaustion is not an exit; a cost equal to the cap is budget; no
	// cap means no budget rule.
	st := LoopState{Iter: 1, MaxIters: 8, MaxWall: time.Hour, ExitErr: errors.New("exit 1"), SawMaxTurns: true}
	if got := NextLoopStop(st); got != "" {
		t.Errorf("turn cap stopped the loop: %q", got)
	}
	st.CostUSD, st.MaxCostUSD = 5, 5
	if got := NextLoopStop(st); got != StoppedByBudget {
		t.Errorf("cost == cap: %q", got)
	}
	st.MaxCostUSD = 0
	if got := NextLoopStop(st); got != "" {
		t.Errorf("no cap, yet %q", got)
	}
}

func TestBlockedReason(t *testing.T) {
	dir := t.TempDir()
	notes := filepath.Join(dir, "NOTES.md")
	write := func(body string) {
		t.Helper()
		if err := os.WriteFile(notes, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if _, ok := BlockedReason(dir, timeZero, time.Time{}); ok {
		t.Fatal("no NOTES.md, yet blocked")
	}
	write("plan\nBLOCKED:\n  BLOCKED:   npm install is refused: no network \x07\nBLOCKED: second\n")
	r, ok := BlockedReason(dir, timeZero, time.Time{})
	if !ok || r != "npm install is refused: no network" {
		t.Fatalf("reason %q, %v", r, ok)
	}
	write("BLOCKED: " + strings.Repeat("é", 300))
	if r, _ := BlockedReason(dir, timeZero, time.Time{}); len([]rune(r)) != 200 {
		t.Fatalf("reason not cut to 200: %d", len([]rune(r)))
	}
	write("    BLOCKED: <reason>\nBLOCKED: \u202edir\n")
	if r, ok := BlockedReason(dir, timeZero, time.Time{}); !ok || r != "dir" {
		t.Fatalf("placeholder or format char: %q %v", r, ok)
	}
	write("BLOCKED: <reason>\n")
	if _, ok := BlockedReason(dir, timeZero, time.Time{}); ok {
		t.Fatal("the brief's placeholder counted")
	}
	write("UNFIXED: DEF-1\nnot BLOCKED: here\n")
	if _, ok := BlockedReason(dir, timeZero, time.Time{}); ok {
		t.Fatal("a mid-line BLOCKED: counted")
	}
	// A line written before this run's loop started does not count.
	write("BLOCKED: stale\n")
	old := time.Now().Add(-time.Hour)
	if err := os.Chtimes(notes, old, old); err != nil {
		t.Fatal(err)
	}
	if _, ok := BlockedReason(dir, timeZero, time.Now().Add(-time.Minute)); ok {
		t.Fatal("a stale BLOCKED: line counted")
	}
	// A symlinked NOTES.md is refused like harvest refuses it.
	_ = os.Remove(notes)
	target := filepath.Join(t.TempDir(), "x.md")
	_ = os.WriteFile(target, []byte("BLOCKED: via link\n"), 0o644)
	if err := os.Symlink(target, notes); err != nil {
		t.Fatal(err)
	}
	if _, ok := BlockedReason(dir, timeZero, time.Time{}); ok {
		t.Fatal("a symlinked NOTES.md counted")
	}
}

func gitT(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := osexec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1",
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

// The no_progress fingerprint changes on every kind of progress, including
// a second edit to an already-modified file, and never matches on a git
// error.
func TestProgressFingerprint(t *testing.T) {
	root := t.TempDir()
	primary, wt, handoff := filepath.Join(root, "primary"), filepath.Join(root, "wt"), filepath.Join(root, "handoff")
	_ = os.MkdirAll(primary, 0o755)
	_ = os.MkdirAll(handoff, 0o755)
	gitT(t, primary, "init", "-q", "-b", "main")
	_ = os.WriteFile(filepath.Join(primary, "a.txt"), []byte("one\n"), 0o644)
	gitT(t, primary, "add", "a.txt")
	gitT(t, primary, "commit", "-q", "-m", "init")
	gitT(t, primary, "worktree", "add", "-q", "-b", "fix", wt)
	wts := []git.WorktreeBinding{{Repo: "app", PrimaryAbs: primary, WorktreeAbs: wt, Branch: "fix"}}

	fp := func() string { t.Helper(); return ProgressFingerprint(handoff, wts) }
	prev := fp()
	if again := fp(); again != prev {
		t.Fatal("an unchanged tree gave two fingerprints")
	}
	steps := []struct {
		name string
		do   func()
	}{
		{"edit", func() { _ = os.WriteFile(filepath.Join(wt, "a.txt"), []byte("two\n"), 0o644) }},
		{"second edit of a modified file", func() { _ = os.WriteFile(filepath.Join(wt, "a.txt"), []byte("three\n"), 0o644) }},
		{"untracked file", func() { _ = os.WriteFile(filepath.Join(wt, "b.txt"), []byte("b\n"), 0o644) }},
		{"second edit of an untracked file", func() {
			_ = os.WriteFile(filepath.Join(wt, "b.txt"), []byte("bb\n"), 0o644)
			later := time.Now().Add(2 * time.Second)
			_ = os.Chtimes(filepath.Join(wt, "b.txt"), later, later)
		}},
		{"commit", func() { gitT(t, wt, "add", "-A"); gitT(t, wt, "commit", "-q", "-m", "x") }},
		{"NOTES.md", func() { _ = os.WriteFile(filepath.Join(handoff, "NOTES.md"), []byte("plan\n"), 0o644) }},
		{"fix-notes", func() {
			_ = os.MkdirAll(filepath.Join(handoff, "fix-notes"), 0o755)
			_ = os.WriteFile(filepath.Join(handoff, "fix-notes", "DEF-1.md"), []byte("n\n"), 0o644)
		}},
		{"fix-evidence", func() {
			_ = os.MkdirAll(filepath.Join(handoff, "fix-evidence", "DEF-1"), 0o755)
			_ = os.WriteFile(filepath.Join(handoff, "fix-evidence", "DEF-1", "fix-01.png"), []byte("png"), 0o644)
		}},
	}
	// What always changes is not progress: the agent's stream log, its
	// session and state files, review output, anything else in the handoff.
	for _, f := range []string{"agent.log", "agent-stream.jsonl", ".grok/session.json", ".claude/state", "review/spec/out.txt", "BRIEF.md"} {
		_ = os.MkdirAll(filepath.Dir(filepath.Join(handoff, f)), 0o755)
		_ = os.WriteFile(filepath.Join(handoff, f), []byte(time.Now().String()), 0o644)
	}
	if got := fp(); got != prev {
		t.Fatal("handoff logs or state changed the fingerprint")
	}
	seen := []string{prev}
	for _, s := range steps {
		s.do()
		got := fp()
		if slices.Contains(seen, got) {
			t.Errorf("%s: fingerprint did not change", s.name)
		}
		seen = append(seen, got)
	}
	// Spec §4.1.6 (amended 2026-11-20): the same bytes re-saved, with a new
	// mtime, are no progress, in the handoff and in the worktree alike.
	resave := func(p string) {
		b, _ := os.ReadFile(p)
		_ = os.WriteFile(p, b, 0o644)
		later := time.Now().Add(time.Hour)
		_ = os.Chtimes(p, later, later)
	}
	before := fp()
	for _, f := range []string{
		filepath.Join(handoff, "NOTES.md"), filepath.Join(handoff, "fix-notes", "DEF-1.md"),
		filepath.Join(handoff, "fix-evidence", "DEF-1", "fix-01.png"), filepath.Join(wt, "b.txt"),
	} {
		resave(f)
	}
	if fp() != before {
		t.Fatal("identical bytes re-saved counted as progress")
	}
	// Changed content of the same size is progress.
	_ = os.WriteFile(filepath.Join(handoff, "fix-evidence", "DEF-1", "fix-01.png"), []byte("PNG"), 0o644)
	if fp() == before {
		t.Fatal("changed evidence bytes are not progress")
	}
	// Over 10 MB the stamp is size and mtime: a touch counts there.
	big := filepath.Join(handoff, "fix-evidence", "DEF-1", "big.png")
	_ = os.WriteFile(big, make([]byte, maxHashedFile+1), 0o644)
	withBig := fp()
	later := time.Now().Add(2 * time.Hour)
	_ = os.Chtimes(big, later, later)
	if fp() == withBig {
		t.Fatal("a large file's mtime is not stamped")
	}
	// A broken binding never yields an equal fingerprint.
	bad := []git.WorktreeBinding{{Repo: "app", PrimaryAbs: primary, WorktreeAbs: filepath.Join(root, "gone")}}
	if ProgressFingerprint(handoff, bad) == ProgressFingerprint(handoff, bad) {
		t.Fatal("a git error gave equal fingerprints")
	}
}

// RF-a items 2 and 5: a deny entry equal to or inside a ReadOnly path is
// dropped; its ancestors stay denied. Local serve (no ReadOnly) keeps every
// entry.
func TestHostDenyReadKeepsAncestorsOfReadOnly(t *testing.T) {
	h := Host{DataRoot: "/state", DefectsRoot: "/state/jobs/j1/evidence", EnvFiles: []string{"/state/repos"}}
	local := h.DenyRead()
	if !slices.Contains(local, "/state/jobs/j1/evidence/evidence") {
		t.Fatalf("local deny lacks the evidence dir: %v", local)
	}
	h.ReadOnly = []string{"/state/repos/aaa/.git", "/state/jobs/j1/evidence"}
	deny := h.DenyRead()
	if slices.Contains(deny, "/state/jobs/j1/evidence/evidence") {
		t.Fatalf("this job's evidence is still denied: %v", deny)
	}
	for _, want := range []string{"/state/repos", "/state/jobs", "/state/agent-state"} {
		if !slices.Contains(deny, want) {
			t.Errorf("deny lacks %s: %v", want, deny)
		}
	}
}
