package git

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// hostSecrets are set in the test env; none may reach a git or gh child.
var hostSecrets = map[string]string{
	"DEFECT_DRAINER_DATABASE_URL":   "postgres://dd:secret@db/dd",
	"DEFECT_DRAINER_CLAUDE_API_KEY": "sk-dd-claude",
	"DEFECT_CHANNEL_SESSION_KEY":    "legacy-session-key",
	"DEFECTS_ROOT":                  "/secret/defects",
	"ANTHROPIC_API_KEY":             "sk-ant-secret",
	"XAI_API_KEY":                   "xai-secret",
	"DD_TEST_ACCOUNT_PASSWORD":      "pw-test-account",
	"QA_PASSWORD":                   "pw-app-env-secret",
	"SOME_DOTENV_TOKEN":             "loaded-from-dotenv",
}

func setHostSecrets(t *testing.T) {
	t.Helper()
	for k, v := range hostSecrets {
		t.Setenv(k, v)
	}
	t.Setenv("GH_TOKEN", "gh-token-kept")
	t.Setenv("GIT_AUTHOR_NAME", "t")
	t.Setenv("GIT_AUTHOR_EMAIL", "t@t")
	t.Setenv("GIT_COMMITTER_NAME", "t")
	t.Setenv("GIT_COMMITTER_EMAIL", "t@t")
}

// checkHostEnv fails when environ carries a secret or misses a kept name.
func checkHostEnv(t *testing.T, label string, environ []string) {
	t.Helper()
	got := map[string]string{}
	for _, e := range environ {
		if k, v, ok := strings.Cut(e, "="); ok {
			got[k] = v
		}
	}
	for k, v := range hostSecrets {
		if _, ok := got[k]; ok {
			t.Errorf("%s: %s reached the child", label, k)
		}
		for name, val := range got {
			if strings.Contains(val, v) {
				t.Errorf("%s: value of %s reached the child as %s", label, k, name)
			}
		}
	}
	for _, k := range []string{"PATH", "HOME"} {
		if got[k] == "" {
			t.Errorf("%s: %s missing", label, k)
		}
	}
	if got["GH_TOKEN"] != "gh-token-kept" {
		t.Errorf("%s: GH_TOKEN not kept", label)
	}
}

func TestHostToolEnvAllowlist(t *testing.T) {
	setHostSecrets(t)
	t.Setenv("LC_ALL", "C")
	t.Setenv("SSH_AUTH_SOCK", "/tmp/agent.sock")
	t.Setenv("XDG_CONFIG_HOME", "/tmp/xdg")
	t.Setenv("XDG_RUNTIME_DIR", "/tmp/xdg-run")
	t.Setenv("GIT_DIR", "/elsewhere/.git")
	t.Setenv("GIT_WORK_TREE", "/elsewhere")
	env := HostToolEnv()
	checkHostEnv(t, "HostToolEnv", env)
	joined := "\n" + strings.Join(env, "\n") + "\n"
	for _, want := range []string{"\nLC_ALL=C\n", "\nSSH_AUTH_SOCK=/tmp/agent.sock\n", "\nGIT_AUTHOR_NAME=t\n", "\nXDG_CONFIG_HOME=/tmp/xdg\n"} {
		if !strings.Contains(joined, want) {
			t.Errorf("HostToolEnv dropped %q", strings.TrimSpace(want))
		}
	}
	for _, gone := range []string{"\nXDG_RUNTIME_DIR=", "\nGIT_DIR=", "\nGIT_WORK_TREE="} {
		if strings.Contains(joined, gone) {
			t.Errorf("HostToolEnv kept %q", strings.TrimSpace(gone))
		}
	}
}

func TestWithHooksOffExtendsEnvConfig(t *testing.T) {
	got := withHooksOff([]string{"PATH=/bin"})
	want := []string{"PATH=/bin", "GIT_CONFIG_COUNT=1", "GIT_CONFIG_KEY_0=core.hooksPath", "GIT_CONFIG_VALUE_0=/dev/null"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("fresh: %v", got)
	}
	got = withHooksOff([]string{"GIT_CONFIG_COUNT=1", "GIT_CONFIG_KEY_0=http.proxy", "GIT_CONFIG_VALUE_0=p"})
	want = []string{"GIT_CONFIG_KEY_0=http.proxy", "GIT_CONFIG_VALUE_0=p", "GIT_CONFIG_COUNT=2", "GIT_CONFIG_KEY_1=core.hooksPath", "GIT_CONFIG_VALUE_1=/dev/null"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("extend: %v", got)
	}
}

// installEnvDumpers puts a git wrapper (dumps its env, then runs the real
// git) and a fake gh (dumps its env, prints a PR URL) first on PATH and at
// GH_BIN. It returns the dump dir.
func installEnvDumpers(t *testing.T) string {
	t.Helper()
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git not on PATH")
	}
	root := t.TempDir()
	bin := filepath.Join(root, "bin")
	dumps := filepath.Join(root, "dumps")
	for _, d := range []string{bin, dumps} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	gitScript := "#!/bin/sh\nenv > \"" + dumps + "/git.$$\"\nexec \"" + realGit + "\" \"$@\"\n"
	ghScript := "#!/bin/sh\nenv > \"" + dumps + "/gh.$$\"\n" +
		"case \"$*\" in\n" +
		"  *pr\\ view*) echo '{\"state\":\"OPEN\",\"mergedAt\":\"\",\"number\":7,\"url\":\"https://github.com/example/demo/pull/7\"}' ;;\n" +
		"  *) echo https://github.com/example/demo/pull/7 ;;\n" +
		"esac\nexit 0\n"
	if err := os.WriteFile(filepath.Join(bin, "git"), []byte(gitScript), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bin, "gh"), []byte(ghScript), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("GH_BIN", filepath.Join(bin, "gh"))
	return dumps
}

// checkDumps checks every env dump with prefix (git. or gh.) and that there
// was at least one, then clears them for the next family.
func checkDumps(t *testing.T, dumps, prefix, label string) {
	t.Helper()
	matches, _ := filepath.Glob(filepath.Join(dumps, prefix+"*"))
	if len(matches) == 0 {
		t.Fatalf("%s: no %s child ran through the dumper", label, prefix)
	}
	for _, m := range matches {
		raw, err := os.ReadFile(m)
		if err != nil {
			t.Fatal(err)
		}
		checkHostEnv(t, label, strings.Split(strings.TrimSpace(string(raw)), "\n"))
		_ = os.Remove(m)
	}
}

// realGitRun runs git with the test's own env, bypassing the dumpers.
func realGitRun(t *testing.T, gitBin, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command(gitBin, append([]string{"-C", dir}, args...)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// shipFixture is a primary repo on main with one commit, and a job worktree
// on its own branch with one uncommitted file.
type shipFixture struct {
	gitBin, primary, worktree, branch string
}

func newShipFixture(t *testing.T) shipFixture {
	t.Helper()
	gitBin, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git not on PATH")
	}
	root := t.TempDir()
	f := shipFixture{gitBin: gitBin, primary: filepath.Join(root, "primary"),
		worktree: filepath.Join(root, "wt"), branch: "defect-drainer/BATCH-m1"}
	if err := os.MkdirAll(f.primary, 0o755); err != nil {
		t.Fatal(err)
	}
	realGitRun(t, gitBin, f.primary, "init", "-q", "-b", "main")
	writeFile(t, filepath.Join(f.primary, "README"), "base\n")
	realGitRun(t, gitBin, f.primary, "add", "README")
	realGitRun(t, gitBin, f.primary, "commit", "-q", "-m", "base")
	realGitRun(t, gitBin, f.primary, "worktree", "add", "-q", "-b", f.branch, f.worktree, "main")
	writeFile(t, filepath.Join(f.worktree, "fix.txt"), "fixed\n")
	return f
}

func (f shipFixture) binding() WorktreeBinding {
	return WorktreeBinding{Repo: "demo", PrimaryAbs: f.primary, WorktreeAbs: f.worktree,
		Branch: f.branch, BaseSource: "local", BaseRef: "main", BaseBranch: "main"}
}

func writeFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
}

func TestHostGitEnvDumpPerFamily(t *testing.T) {
	setHostSecrets(t)
	f := newShipFixture(t)
	dumps := installEnvDumpers(t)

	// gitC / gitCOut, and the direct status call in dirtyFiles: a local ship
	// with commit (status, add, commit, rev-parse, merge).
	out := CreatePRsForWorktrees([]WorktreeBinding{f.binding()}, "BATCH-m1", "t", ShipRequest{Commit: true, Message: "fix"})
	if len(out) != 1 || out[0].Status != "merged" {
		t.Fatalf("local ship %+v", out)
	}
	checkDumps(t, dumps, "git.", "gitC ship")

	// gitOk
	if ref, _, _ := ResolveBaseRef(f.primary, "origin", "main"); ref == "" {
		t.Fatal("ResolveBaseRef found nothing")
	}
	checkDumps(t, dumps, "git.", "gitOk")

	// direct HostGit calls: branch listing and best-effort fetch.
	if _, err := ListRepoBranches("local", f.primary); err != nil {
		t.Fatal(err)
	}
	FetchBaseRef(f.primary, "origin", "main")
	checkDumps(t, dumps, "git.", "direct git")

	// gh through GH_BIN (ghCmd) and through "gh" on PATH.
	RefreshPRStatuses([]PRResult{{Repo: "demo", Number: 7, Status: "created"}}, nil)
	if _, err := CreatePullRequest(f.worktree, "t", "b", "main"); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := RefreshPR(f.worktree); err != nil {
		t.Fatal(err)
	}
	// gh also carries hooks-off as env config, for the git it starts itself.
	matches, _ := filepath.Glob(filepath.Join(dumps, "gh.*"))
	for _, m := range matches {
		raw, _ := os.ReadFile(m)
		if !strings.Contains(string(raw), "GIT_CONFIG_KEY_0=core.hooksPath\n") || !strings.Contains(string(raw), "GIT_CONFIG_VALUE_0=/dev/null\n") {
			t.Errorf("gh child %s has no hooks-off env config", m)
		}
	}
	checkDumps(t, dumps, "gh.", "gh")
}

// hookScript writes the hook's name and env to marker when it runs.
func hookScript(marker, name string) string {
	return "#!/bin/sh\n{ echo " + name + "; env; } >> \"" + marker + "\"\n"
}

var plantedHooks = []string{"pre-commit", "prepare-commit-msg", "commit-msg", "post-commit", "post-merge", "post-checkout", "pre-push", "reference-transaction"}

func TestHostGitRunsNoRepoHooks(t *testing.T) {
	cases := []struct {
		name  string
		plant func(t *testing.T, f shipFixture, marker string)
	}{
		{"hooks dir", func(t *testing.T, f shipFixture, marker string) {
			for _, h := range plantedHooks {
				writeFile(t, filepath.Join(f.primary, ".git", "hooks", h), hookScript(marker, h))
			}
		}},
		{"in-tree core.hooksPath", func(t *testing.T, f shipFixture, marker string) {
			// The agent writes .husky/ into its worktree; the repo config
			// (shared by the worktree) points core.hooksPath at it. A local
			// merge then brings the same files into the primary tree.
			realGitRun(t, f.gitBin, f.primary, "config", "core.hooksPath", ".husky")
			for _, h := range plantedHooks {
				writeFile(t, filepath.Join(f.worktree, ".husky", h), hookScript(marker, h))
			}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			setHostSecrets(t)
			f := newShipFixture(t)
			marker := filepath.Join(t.TempDir(), "hook-fired")
			tc.plant(t, f, marker)

			out := CreatePRsForWorktrees([]WorktreeBinding{f.binding()}, "BATCH-m1", "t", ShipRequest{Commit: true, Message: "fix"})
			if len(out) != 1 || out[0].Status != "merged" {
				t.Fatalf("local ship %+v", out)
			}
			if raw, err := os.ReadFile(marker); err == nil {
				t.Fatalf("a repo hook ran during DD's commit/merge:\n%s", raw)
			}

			// Control: the same hooks do fire for a plain git commit, so the
			// test planted them where git looks.
			writeFile(t, filepath.Join(f.primary, "control.txt"), "c\n")
			realGitRun(t, f.gitBin, f.primary, "add", "control.txt")
			realGitRun(t, f.gitBin, f.primary, "commit", "-q", "-m", "control")
			if _, err := os.Stat(marker); err != nil {
				t.Fatal("control commit did not fire the planted hook; the test is not testing anything")
			}
		})
	}
}

// TestHostGitWorktreeAddAndPushRunNoHooks covers the hooks the local ship does
// not reach: post-checkout and reference-transaction on `worktree add`, and
// pre-push on an origin ship.
func TestHostGitWorktreeAddAndPushRunNoHooks(t *testing.T) {
	setHostSecrets(t)
	gitBin, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git not on PATH")
	}
	root := t.TempDir()
	primary, bare := filepath.Join(root, "primary"), filepath.Join(root, "origin.git")
	if err := os.MkdirAll(primary, 0o755); err != nil {
		t.Fatal(err)
	}
	realGitRun(t, gitBin, root, "init", "-q", "--bare", bare)
	realGitRun(t, gitBin, primary, "init", "-q", "-b", "main")
	writeFile(t, filepath.Join(primary, "README"), "base\n")
	realGitRun(t, gitBin, primary, "add", "README")
	realGitRun(t, gitBin, primary, "commit", "-q", "-m", "base")
	realGitRun(t, gitBin, primary, "remote", "add", "origin", bare)
	realGitRun(t, gitBin, primary, "push", "-q", "origin", "main")

	marker := filepath.Join(root, "hook-fired")
	for _, h := range plantedHooks {
		writeFile(t, filepath.Join(primary, ".git", "hooks", h), hookScript(marker, h))
	}
	installEnvDumpers(t) // fake gh; git still runs for real

	wts, err := CreateBatchWorktrees("BATCH-hooks", filepath.Join(root, "wts"), map[string]WorktreeRequest{
		"demo": {PrimaryAbs: primary, Source: "local", Branch: "main"},
	})
	if err != nil || len(wts) != 1 {
		t.Fatalf("worktrees %v %+v", err, wts)
	}
	writeFile(t, filepath.Join(wts[0].WorktreeAbs, "fix.txt"), "fixed\n")
	realGitRun(t, gitBin, wts[0].WorktreeAbs, "-c", "core.hooksPath=/dev/null", "add", "fix.txt")
	realGitRun(t, gitBin, wts[0].WorktreeAbs, "-c", "core.hooksPath=/dev/null", "commit", "-q", "-m", "fix")
	b := wts[0]
	b.BaseSource, b.BaseRef, b.BaseBranch = "origin", "origin/main", "main"
	CreatePRsForWorktrees([]WorktreeBinding{b}, "BATCH-hooks", "t", ShipRequest{})
	if out := realGitRun(t, gitBin, bare, "branch", "--list", b.Branch); out == "" {
		t.Fatalf("push did not reach origin; the pre-push path was not exercised")
	}
	if raw, err := os.ReadFile(marker); err == nil {
		t.Fatalf("a repo hook ran during DD's worktree add or push:\n%s", raw)
	}

	// Control: a plain push fires the planted pre-push hook.
	realGitRun(t, gitBin, b.WorktreeAbs, "push", "-q", "origin", "HEAD:refs/heads/control")
	if raw, _ := os.ReadFile(marker); !strings.Contains(string(raw), "pre-push") {
		t.Fatal("control push did not fire pre-push; the test is not testing anything")
	}
}

// TestNoInheritedEnvForGitOrGh fails when any non-test Go file in the
// module starts git or gh with os/exec (under any import name)
// instead of HostGit/hostCmd, or calls hostCmd("git", …), which would skip
// the hooks-off flag. Only git/hostenv.go may build those commands.
func TestNoInheritedEnvForGitOrGh(t *testing.T) {
	var files []string
	for _, top := range []string{".."} {
		err := filepath.WalkDir(top, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() && (d.Name() == "testdata" || d.Name() == "node_modules") {
				return filepath.SkipDir
			}
			if !d.IsDir() && strings.HasSuffix(p, ".go") && !strings.HasSuffix(p, "_test.go") {
				files = append(files, p)
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	if len(files) < 20 {
		t.Fatalf("scanned only %d files; the walk is not reaching the source", len(files))
	}
	for _, path := range files {
		if filepath.Base(path) == "hostenv.go" && filepath.Base(filepath.Dir(path)) == "git" {
			continue
		}
		fset := token.NewFileSet()
		file, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		execName := ""
		for _, imp := range file.Imports {
			if imp.Path.Value == `"os/exec"` {
				execName = "exec"
				if imp.Name != nil {
					execName = imp.Name.Name
				}
			}
		}
		ast.Inspect(file, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			if id, ok := call.Fun.(*ast.Ident); ok && id.Name == "hostCmd" && len(call.Args) > 0 {
				if lit, ok := call.Args[0].(*ast.BasicLit); ok && lit.Value == `"git"` {
					t.Errorf("%s: hostCmd(\"git\", …) skips the hooks-off flag; use HostGit", fset.Position(call.Pos()))
				}
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || execName == "" {
				return true
			}
			if pkg, ok := sel.X.(*ast.Ident); !ok || pkg.Name != execName {
				return true
			}
			idx := -1
			switch sel.Sel.Name {
			case "Command":
				idx = 0
			case "CommandContext":
				idx = 1
			}
			if idx >= 0 && len(call.Args) > idx && hostToolBinExpr(call.Args[idx]) {
				t.Errorf("%s: git/gh started with %s.%s; use git.HostGit (HostToolEnv, hooks off)", fset.Position(call.Pos()), execName, sel.Sel.Name)
			}
			return true
		})
	}
}

func hostToolBinExpr(e ast.Expr) bool {
	switch v := e.(type) {
	case *ast.BasicLit:
		return v.Value == `"git"` || v.Value == `"gh"`
	case *ast.CallExpr:
		if id, ok := v.Fun.(*ast.Ident); ok && id.Name == "GhBin" {
			return true
		}
		if s, ok := v.Fun.(*ast.SelectorExpr); ok && s.Sel.Name == "GhBin" {
			return true
		}
	}
	return false
}
