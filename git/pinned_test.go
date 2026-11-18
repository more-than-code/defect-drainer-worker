package git

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// setLFSPath pins LFSPath for one test.
func setLFSPath(t *testing.T, p string) {
	t.Helper()
	lfsOnce.Do(func() {})
	old := lfsPath
	lfsPath = p
	t.Cleanup(func() { lfsPath = old })
}

func pinnedRun(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1",
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// linkedWorktree is a clone with one commit and a linked worktree wt.
func linkedWorktree(t *testing.T) (clone, wt string) {
	t.Helper()
	root := t.TempDir()
	clone, wt = filepath.Join(root, "clone"), filepath.Join(root, "wt")
	if err := os.MkdirAll(clone, 0o755); err != nil {
		t.Fatal(err)
	}
	pinnedRun(t, clone, "init", "-q", "-b", "main")
	if err := os.WriteFile(filepath.Join(clone, "a.txt"), []byte("a\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	pinnedRun(t, clone, "add", ".")
	pinnedRun(t, clone, "commit", "-q", "-m", "init")
	pinnedRun(t, clone, "worktree", "add", "-q", "--detach", wt, "HEAD")
	return clone, wt
}

func TestPinnedNamesTheAdminDirAndBlanksFilters(t *testing.T) {
	setLFSPath(t, "")
	clone, wt := linkedWorktree(t)
	pinnedRun(t, clone, "config", "filter.cfg.clean", "false")
	if err := os.MkdirAll(filepath.Join(wt, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wt, "sub", ".gitattributes"), []byte("*.x filter=nested -text\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	admin, ok := WorktreeAdminDir(clone, wt)
	if !ok {
		t.Fatal("admin dir not found")
	}
	cmd, err := PinnedWorktreeGit(clone, admin, wt, "diff", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	args := strings.Join(cmd.Args, "\n")
	for _, want := range []string{
		"--git-dir=" + admin, "--work-tree=" + wt, "core.hooksPath=/dev/null", "core.fsmonitor=", "commit.gpgSign=false", "submodule.recurse=false", "--ignore-submodules=all",
		"core.pager=cat", "diff.external=", "core.sshCommand=", "core.askPass=", "credential.helper=",
		"protocol.allow=never", "protocol.https.allow=always", "core.attributesFile=/dev/null",
		"filter.cfg.clean=", "filter.cfg.required=false", "filter.nested.process=", "--no-pager",
		"--no-ext-diff", "--no-textconv",
	} {
		if !strings.Contains(args, "\n"+want+"\n") && !strings.HasSuffix(args, "\n"+want) {
			t.Errorf("argv lacks %q:\n%s", want, args)
		}
	}
	if out, err := cmd.Output(); err != nil {
		t.Fatalf("pinned diff: %v %s", err, out)
	}
}

func TestPinnedEnv(t *testing.T) {
	t.Setenv("GH_TOKEN", "gh-secret")
	t.Setenv("GITHUB_TOKEN", "ghs-secret")
	t.Setenv("SSH_AUTH_SOCK", "/tmp/agent.sock")
	t.Setenv("GIT_CONFIG_PARAMETERS", "'core.fsmonitor'='x'")
	t.Setenv("GIT_DIR", "/elsewhere")
	t.Setenv("GIT_AUTHOR_NAME", "dd")
	env := "\n" + strings.Join(pinnedEnv(), "\n") + "\n"
	for _, gone := range []string{"GH_TOKEN=", "GITHUB_TOKEN=", "SSH_AUTH_SOCK=", "GIT_CONFIG_PARAMETERS=", "GIT_DIR=", "GIT_AUTHOR_NAME="} {
		if strings.Contains(env, "\n"+gone) {
			t.Errorf("%s reached pinned git", gone)
		}
	}
	for _, want := range []string{"GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null", "GIT_TERMINAL_PROMPT=0"} {
		if !strings.Contains(env, "\n"+want+"\n") {
			t.Errorf("%s missing", want)
		}
	}
}

func TestPinnedLFSMissing(t *testing.T) {
	setLFSPath(t, "")
	clone, wt := linkedWorktree(t)
	if err := os.WriteFile(filepath.Join(wt, ".gitattributes"), []byte("*.bin filter=lfs diff=lfs merge=lfs -text\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	admin, _ := WorktreeAdminDir(clone, wt)
	if _, err := PinnedWorktreeGit(clone, admin, wt, "status"); !errors.Is(err, ErrLFSMissing) {
		t.Fatalf("err = %v, want ErrLFSMissing", err)
	}
}

// With a real git-lfs, a tracked file is stored as an LFS pointer through
// the fixed filter.lfs commands (W3-1: LFS repos keep working).
func TestPinnedLFSWithRealGitLFS(t *testing.T) {
	real, err := exec.LookPath("git-lfs")
	if err != nil {
		t.Skip("git-lfs is not installed on this machine: the LFS path is covered by the Linux container gate")
	}
	abs, _ := filepath.Abs(real)
	setLFSPath(t, abs)
	clone, wt := linkedWorktree(t)
	if err := os.WriteFile(filepath.Join(wt, ".gitattributes"), []byte("*.bin filter=lfs diff=lfs merge=lfs -text\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wt, "big.bin"), []byte("binary payload\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	admin, _ := WorktreeAdminDir(clone, wt)
	cmd, err := PinnedWorktreeGit(clone, admin, wt, "add", "--", ".gitattributes", "big.bin")
	if err != nil {
		t.Fatal(err)
	}
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("pinned add with git-lfs: %v\n%s", err, out)
	}
	cmd, _ = PinnedWorktreeGit(clone, admin, wt, "show", ":big.bin")
	out, err := cmd.Output()
	if err != nil || !strings.Contains(string(out), "https://git-lfs.github.com/spec/v1") {
		t.Fatalf("index holds %q (%v), want an LFS pointer", out, err)
	}
}

// A worker binding (the primary is a real clone) never falls back to a .git
// dir planted in the worktree.
func TestBindingGitDirIgnoresPlantedGitDir(t *testing.T) {
	clone, _ := linkedWorktree(t)
	planted := filepath.Join(t.TempDir(), "wt2")
	if err := os.MkdirAll(planted, 0o755); err != nil {
		t.Fatal(err)
	}
	pinnedRun(t, planted, "init", "-q")
	if dir, ok := BindingGitDir(WorktreeBinding{Repo: "app", PrimaryAbs: clone, WorktreeAbs: planted}); ok {
		t.Fatalf("planted git dir accepted: %s", dir)
	}
	if PinnedWorktree(clone, planted) {
		t.Fatal("an unregistered dir passed the Rerun check")
	}
	// A plain checkout with no separate primary still works (local fixtures).
	if dir, ok := BindingGitDir(WorktreeBinding{Repo: "app", WorktreeAbs: planted}); !ok || dir != filepath.Join(planted, ".git") {
		t.Fatalf("plain checkout: %q %v", dir, ok)
	}
}

func TestPinnedWorktreeRerunCheck(t *testing.T) {
	setLFSPath(t, "")
	clone, wt := linkedWorktree(t)
	if !PinnedWorktree(clone, wt) {
		t.Fatal("a live worktree failed the check")
	}
	// The agent rewrites wt/.git at another repo: the clone's record still
	// names wt, so the check holds and git never reads the planted file.
	other := t.TempDir()
	pinnedRun(t, other, "init", "-q")
	if err := os.WriteFile(filepath.Join(wt, ".git"), []byte("gitdir: "+filepath.Join(other, ".git")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if !PinnedWorktree(clone, wt) {
		t.Fatal("check followed the planted .git file")
	}
	if err := os.RemoveAll(wt); err != nil {
		t.Fatal(err)
	}
	if PinnedWorktree(clone, wt) {
		t.Fatal("a removed worktree passed the check")
	}
}

// With RequireLinkedWorktrees (dd-worker), a binding whose clone has no .git
// never falls back to a .git dir in the worktree.
func TestRequireLinkedWorktrees(t *testing.T) {
	plain := t.TempDir()
	pinnedRun(t, plain, "init", "-q")
	b := WorktreeBinding{Repo: "app", PrimaryAbs: t.TempDir(), WorktreeAbs: plain}
	if _, ok := BindingGitDir(b); !ok {
		t.Fatal("local fixture fallback lost")
	}
	RequireLinkedWorktrees()
	t.Cleanup(func() { linkedOnly.Store(false) })
	if dir, ok := BindingGitDir(b); ok {
		t.Fatalf("fallback used in linked-only mode: %s", dir)
	}
}

func TestShellQuote(t *testing.T) {
	for in, want := range map[string]string{
		"/usr/bin/git-lfs":  "/usr/bin/git-lfs",
		"/opt/my tools/lfs": "'/opt/my tools/lfs'",
		"/opt/a;b/lfs":      "'/opt/a;b/lfs'",
		"/opt/it's/git-lfs": `'/opt/it'\''s/git-lfs'`,
	} {
		if got := shellQuote(in); got != want {
			t.Errorf("shellQuote(%q) = %q, want %q", in, got, want)
		}
	}
}
