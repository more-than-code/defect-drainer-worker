package worker

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	dexec "github.com/more-than-code/defect-drainer-worker/exec"
	"github.com/more-than-code/defect-drainer-worker/workerproto"
)

// M1 (docs/worker-w3.md §3.5, criterion 4): an agent-written git config
// must not make DD's own git run a command on the host. Each case plants
// one attack whose command writes a marker file, then drives the job, the
// post-agent steps, ship_preview, diff and ship. No marker may appear.
//
// R4 (core.pager) is a guard: git starts no pager when its output is a pipe,
// which it always is for DD, so it cannot fire before the fix either.
func TestM1AgentGitConfigRunsNothing(t *testing.T) {
	cases := []struct {
		name string
		// plant is shell run by the fake agent in the handoff dir. $CL is the
		// clone's git dir, $WT the worktree, $EVIL the marker command.
		plant string
		// global, when set, is appended to the host's global git config.
		global string
		// gitfile retargets <wt>/.git at a fake repo whose config is this.
		gitfile string
		// submodule gives the origin's main a gitlink at sub/ (a repo that
		// has a submodule), which the agent then populates.
		submodule bool
	}{
		{name: "R1-gitfile", gitfile: "[core]\n\tfsmonitor = EVIL\n[filter \"evil\"]\n\tclean = EVIL\n", plant: `echo '* filter=evil' > "$WT/.gitattributes"`},
		{name: "R2-filter", plant: `git config -f "$CL/config" filter.evil.clean "$EVIL"; echo '* filter=evil' > "$WT/.gitattributes"`},
		{name: "R3-fsmonitor", plant: `git config -f "$CL/config" core.fsmonitor "$EVIL"`},
		{name: "R4-pager", plant: `git config -f "$CL/config" core.pager "$EVIL"`},
		{name: "R5-textconv", plant: `git config -f "$CL/config" diff.x.textconv "$EVIL"; echo '* diff=x' > "$WT/.gitattributes"`},
		{name: "R6-global-include", global: "[core]\n\tfsmonitor = EVIL\n"},
		// R7 (security review): a repo with a submodule. The agent makes sub/
		// a repo of its own whose config and attributes name a filter, and
		// leaves a modified file there: a parent status/diff that recurses
		// would start git in sub/ with the agent's config.
		{name: "R7-submodule", submodule: true, plant: `(cd "$WT/sub" && git init -q && echo a > f && git add f && git -c user.name=a -c user.email=a@b commit -qm s && git config filter.x.clean "$EVIL" && echo '* filter=x' > .git/info/attributes && echo b > f)`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fakeGH(t)
			dir := t.TempDir()
			marker := filepath.Join(dir, "marker")
			evil := filepath.Join(dir, "evil.sh")
			// A filter reads stdin, textconv gets a path, fsmonitor gets a
			// version: pass content through so git keeps working.
			writeFile(t, evil, "#!/bin/sh\necho \"$0 $*\" >> '"+marker+"'\nif [ -f \"$1\" ]; then cat \"$1\"; else cat; fi\n", 0o755)
			plant := "WT=../worktrees/app\nEVIL='" + evil + "'\nCL=$(cd \"$WT\" && git rev-parse --path-format=absolute --git-common-dir)\n"
			fake := filepath.Join(dir, "fake")
			if tc.gitfile != "" {
				plant += "printf 'gitdir: %s\\n' '" + filepath.Join(fake, ".git") + "' > \"$WT/.git\"\n"
			}
			plant += tc.plant + "\n"
			agent := fixArtifacts(testDefect) + plant +
				"echo changed >> ../worktrees/app/README.md\n" +
				"cat '" + claudeFixture(t, "c1-basic") + "'\nexit 0\n"
			h := newHarness(t, harnessOpts{agent: agent, noStart: true})
			if tc.gitfile != "" { // after newHarness: it sets the git identity
				gitRun(t, dir, "init", "-q", "-b", "main", fake)
				writeFile(t, filepath.Join(fake, "README.md"), "fake\n", 0o644)
				gitRun(t, fake, "add", ".")
				gitRun(t, fake, "commit", "-q", "-m", "fake")
				cfg := filepath.Join(fake, ".git", "config")
				b, err := os.ReadFile(cfg)
				if err != nil {
					t.Fatal(err)
				}
				writeFile(t, cfg, string(b)+strings.ReplaceAll(tc.gitfile, "EVIL", evil), 0o644)
			}
			if tc.submodule {
				src := filepath.Join(h.tmp, "src")
				head := strings.TrimSpace(gitOut(t, src, "rev-parse", "HEAD"))
				gitRun(t, src, "update-index", "--add", "--cacheinfo", "160000,"+head+",sub")
				writeFile(t, filepath.Join(src, ".gitmodules"), "[submodule \"sub\"]\n\tpath = sub\n\turl = ./sub\n", 0o644)
				gitRun(t, src, "add", ".gitmodules")
				gitRun(t, src, "commit", "-q", "-m", "add a submodule")
				gitRun(t, src, "push", "-q", filepath.Join(h.tmp, "origin", "app.git"), "main")
			}
			if tc.global != "" {
				inc := filepath.Join(dir, "include.gitconfig")
				writeFile(t, inc, strings.ReplaceAll(tc.global, "EVIL", evil), 0o644)
				gc := os.Getenv("GIT_CONFIG_GLOBAL")
				b, err := os.ReadFile(gc)
				if err != nil {
					t.Fatal(err)
				}
				writeFile(t, gc, string(b)+"[include]\n\tpath = "+inc+"\n", 0o644)
			}
			h.start(Timing{})

			fired := func(stage string) {
				t.Helper()
				if b, err := os.ReadFile(marker); err == nil {
					t.Errorf("%s: the planted command ran on the host by %s:\n%s", tc.name, stage, b)
					_ = os.Remove(marker)
				}
			}
			id := "bjob_m1"
			h.offerJob(fixSpec(id), nil)
			h.waitDone(id, 60*time.Second)
			fired("the job (worktree setup and post-agent steps)")

			h.controlAndWait(id, "rq_prev", dexec.Control{Kind: dexec.ControlShipPreview}, 1)
			fired("ship_preview")
			h.controlAndWait(id, "rq_diff", dexec.Control{Kind: dexec.ControlDiff, Repo: "app"}, 1)
			fired("diff")
			ship := dexec.Control{Kind: dexec.ControlShip, Repos: []dexec.ShipRepo{{Repo: "app", Files: []string{"README.md"}, Message: "Fix", Title: "Fix", Body: "b"}}}
			r := h.controlAndWait(id, "rq_ship", ship, 1)
			fired("ship")
			var res workerproto.ShipPushResult
			if r.OK && json.Unmarshal(r.Result, &res) == nil && len(res.Repos) == 1 {
				t.Logf("ship: pushed=%v error=%q", res.Repos[0].Pushed, res.Repos[0].Error)
			}
		})
	}
}

func gitRun(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

func gitOut(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1")
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git %v: %v", args, err)
	}
	return string(out)
}
