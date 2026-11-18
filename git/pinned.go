package git

import (
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
)

// ErrLFSMissing is returned for a repo that uses Git LFS on a host without
// git-lfs. A worker job ends with reason lfs_missing (workerproto).
var ErrLFSMissing = errors.New("the repo uses Git LFS and git-lfs is not installed on this host")

// pinnedConfig is forced on every pinned git call (docs/worker-w3.md §3.5),
// on top of HostGit's core.hooksPath=/dev/null.
// Each one is a setting an agent-written config could use to make git start
// a command, reach a remote or read a config DD did not write.
var pinnedConfig = []string{
	"core.fsmonitor=",
	"core.pager=cat",
	"core.editor=:",
	"core.alternateRefsCommand=",
	"commit.gpgSign=false",
	"tag.gpgSign=false",
	"submodule.recurse=false",
	"core.sshCommand=",
	"core.askPass=",
	"core.attributesFile=/dev/null",
	"diff.external=",
	"credential.helper=",
	"protocol.allow=never",
	"protocol.https.allow=always",
}

var (
	lfsOnce sync.Once
	lfsPath string
)

// LFSPath is git-lfs resolved to an absolute path once, at first use: "" when
// the host has none. The pinned filter.lfs commands name this path, never a
// bare "git-lfs" a later PATH could redirect.
func LFSPath() string {
	lfsOnce.Do(func() {
		if p, err := exec.LookPath("git-lfs"); err == nil {
			if abs, err := filepath.Abs(p); err == nil {
				lfsPath = abs
			}
		}
	})
	return lfsPath
}

// PinnedWorktreeGit is the one way DD runs git in a worktree, or in the clone
// behind it, when an agent has had write access to that worktree.
//
//   - The repo is named, never discovered: --git-dir is adminDir (the
//     clone's <common>/worktrees/<n>, see WorktreeAdminDir) or, when adminDir
//     is "", the clone's own git dir; --work-tree is wt when wt is set. The
//     agent-writable <wt>/.git is never read.
//   - No system or global config, no inherited GIT_* name (GIT_CONFIG_PARAMETERS,
//     GIT_DIR, the commit identity: a caller that commits sets its own), and
//     no GitHub token or ssh agent.
//   - pinnedConfig forced; --no-pager; diffs get --no-ext-diff --no-textconv.
//   - Every filter driver named in the repo's config or in a .gitattributes
//     under wt is blanked, except filter.lfs, which is forced to LFSPath().
//     A repo that names lfs on a host without git-lfs is ErrLFSMissing.
func PinnedWorktreeGit(clone, adminDir, wt string, args ...string) (*exec.Cmd, error) {
	gitDir := adminDir
	if gitDir == "" {
		common, ok := commonDir(clone)
		if !ok {
			return nil, errors.New("not a git repository: " + clone)
		}
		gitDir = common
	}
	base := []string{"--git-dir=" + gitDir}
	if wt != "" {
		base = append(base, "--work-tree="+wt)
	}
	for _, kv := range pinnedConfig {
		base = append(base, "-c", kv)
	}
	filters, err := filterArgs(gitDir, wt)
	// (filterArgs fails closed: an unreadable repo config is an error.)
	if err != nil {
		return nil, err
	}
	base = append(base, filters...)
	base = append(base, "--no-pager")
	// A submodule dir in the worktree is the agent's to fill with a repo of
	// its own: git must not start a child there, which would read that
	// repo's config. The flag beats .gitmodules' submodule.<n>.ignore.
	if len(args) > 0 {
		switch args[0] {
		case "diff":
			args = append([]string{"diff", "--no-ext-diff", "--no-textconv", "--ignore-submodules=all"}, args[1:]...)
		case "status":
			args = append([]string{"status", "--ignore-submodules=all"}, args[1:]...)
		}
	}
	cmd := HostGit(append(base, args...)...)
	cmd.Env = pinnedEnv() // narrows HostGit's env; hooks stay off through argv
	cmd.Dir = wt
	if wt == "" {
		cmd.Dir = gitDir
	}
	return cmd, nil
}

// pinnedEnv is HostToolEnv without any GIT_* name, without
// GitHub tokens or the ssh agent, and with system and global config off.
func pinnedEnv() []string {
	base := HostToolEnv()
	out := make([]string, 0, len(base)+4)
	for _, e := range base {
		k, _, _ := strings.Cut(e, "=")
		switch {
		case strings.HasPrefix(k, "GIT_"),
			k == "GITHUB_TOKEN", k == "GITHUB_ENTERPRISE_TOKEN",
			strings.HasPrefix(k, "GH_"), strings.HasPrefix(k, "SSH_"):
			continue
		}
		out = append(out, e)
	}
	return append(out,
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_CONFIG_SYSTEM=/dev/null",
		"GIT_TERMINAL_PROMPT=0",
	)
}

// filterArgs blanks every filter driver the repo config defines and every
// one a .gitattributes under wt (or the repo's info/attributes) names, and
// pins filter.lfs.
func filterArgs(gitDir, wt string) ([]string, error) {
	names := map[string]bool{}
	cfgNames, err := configFilterNames(gitDir)
	if err != nil {
		return nil, err
	}
	attrNames, err := attributeFilterNames(gitDir, wt)
	if err != nil {
		return nil, err
	}
	for _, n := range append(cfgNames, attrNames...) {
		names[n] = true
	}
	var out []string
	for n := range names {
		if n == "lfs" || !FilterNameSafe(n) {
			continue
		}
		out = append(out,
			"-c", "filter."+n+".clean=",
			"-c", "filter."+n+".smudge=",
			"-c", "filter."+n+".process=",
			"-c", "filter."+n+".required=false",
		)
	}
	if names["lfs"] {
		lfs := LFSPath()
		if lfs == "" || strings.Contains(lfs, "%") { // git expands % in filter commands
			return nil, ErrLFSMissing
		}
		q := shellQuote(lfs)
		out = append(out,
			"-c", "filter.lfs.process="+q+" filter-process",
			"-c", "filter.lfs.clean="+q+" clean -- %f",
			"-c", "filter.lfs.smudge="+q+" smudge -- %f",
			"-c", "filter.lfs.required=true",
		)
	}
	return out, nil
}

// configFilterNames lists filter.<name>.* names in the repo's own config
// (with its includes), read with system and global config off. It fails
// closed: any error but "no such key" (exit 1) is returned.
func configFilterNames(gitDir string) ([]string, error) {
	cmd := HostGit("--git-dir="+gitDir,
		"config", "--includes", "--name-only", "--get-regexp", `^filter\.`)
	cmd.Env = pinnedEnv()
	cmd.Dir = gitDir
	out, err := cmd.Output()
	var ee *exec.ExitError
	if errors.As(err, &ee) && ee.ExitCode() == 1 {
		return nil, nil
	} else if err != nil {
		return nil, errors.New("the repo config cannot be read for its filter drivers")
	}
	var names []string
	for _, key := range strings.Split(string(out), "\n") {
		rest, ok := strings.CutPrefix(strings.TrimSpace(key), "filter.")
		if !ok {
			continue
		}
		if dot := strings.LastIndex(rest, "."); dot > 0 {
			names = append(names, rest[:dot])
		}
	}
	return names, nil
}

// Bounds on what an agent-written .gitattributes may make DD read and pass.
const (
	maxAttributesBytes = 100 << 20 // git itself ignores larger files
	maxFilterNames     = 256
)

// attributeFilterNames lists filter=<name> in every .gitattributes under wt
// (not following symlinks, skipping .git) and in <common>/info/attributes.
// A file over maxAttributesBytes is skipped, as git skips it; more than
// maxFilterNames distinct names is an error (fail closed).
func attributeFilterNames(gitDir, wt string) ([]string, error) {
	var files []string
	if common, ok := commonDirOfGitDir(gitDir); ok {
		files = append(files, filepath.Join(common, "info", "attributes"))
	}
	if wt != "" {
		_ = filepath.WalkDir(wt, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return nil
			}
			if d.Name() == ".git" && p != wt {
				if d.IsDir() {
					return filepath.SkipDir
				}
				return nil
			}
			if d.Type().IsRegular() && d.Name() == ".gitattributes" {
				files = append(files, p)
			}
			return nil
		})
	}
	seen := map[string]bool{}
	var names []string
	for _, f := range files {
		if fi, err := os.Lstat(f); err != nil || !fi.Mode().IsRegular() || fi.Size() > maxAttributesBytes {
			continue
		}
		b, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		for _, field := range strings.Fields(string(b)) {
			if n, ok := strings.CutPrefix(field, "filter="); ok && n != "" && !seen[n] {
				seen[n] = true
				names = append(names, n)
				if len(names) > maxFilterNames {
					return nil, errors.New("too many filter drivers named in .gitattributes")
				}
			}
		}
	}
	return names, nil
}

// FilterNameSafe rejects names that cannot sit in one -c argument. Dots are
// kept: filter.a.b.clean is driver a.b.
func FilterNameSafe(name string) bool {
	if name == "" || strings.HasPrefix(name, "-") {
		return false
	}
	return !strings.ContainsAny(name, " \t\r\n\x00=")
}

// WorktreeAdminDir is the clone's admin dir for the linked worktree wt: the
// <common>/worktrees/<n> whose gitdir file names wt/.git. Only the clone's
// own records are read, never wt/.git, which the agent can rewrite.
func WorktreeAdminDir(clone, wt string) (string, bool) {
	if clone == "" || wt == "" {
		return "", false
	}
	common, ok := commonDir(clone)
	if !ok {
		return "", false
	}
	want := map[string]bool{filepath.Join(wt, ".git"): true}
	if real, err := filepath.EvalSymlinks(wt); err == nil {
		want[filepath.Join(real, ".git")] = true
	}
	root := filepath.Join(common, "worktrees")
	entries, err := os.ReadDir(root)
	if err != nil {
		return "", false
	}
	for _, e := range entries {
		dir := filepath.Join(root, e.Name())
		b, err := os.ReadFile(filepath.Join(dir, "gitdir"))
		if err == nil && e.IsDir() && want[filepath.Clean(strings.TrimSpace(string(b)))] {
			if _, err := os.Stat(wt); err != nil {
				return "", false
			}
			return dir, true
		}
	}
	return "", false
}

// PinnedWorktree reports whether wt is a live linked worktree of clone, as
// the clone records it: the Rerun/Resume check that never reads wt/.git.
func PinnedWorktree(clone, wt string) bool {
	admin, ok := WorktreeAdminDir(clone, wt)
	if !ok {
		return false
	}
	cmd, err := PinnedWorktreeGit(clone, admin, wt, "rev-parse", "--is-inside-work-tree")
	if err != nil {
		return false
	}
	out, err := cmd.Output()
	return err == nil && strings.TrimSpace(string(out)) == "true"
}

// commonDir is the clone's git common dir. A clone DD made has a .git dir;
// a local-mode primary that is itself a linked worktree is the owner's
// checkout (no agent writes it), so asking git is trusted there.
func commonDir(clone string) (string, bool) {
	dotgit := filepath.Join(clone, ".git")
	if fi, err := os.Lstat(dotgit); err == nil && fi.IsDir() {
		return dotgit, true
	}
	out, err := gitCOut(clone, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil || out == "" {
		return "", false
	}
	return out, true
}

// commonDirOfGitDir is <common> for an admin dir <common>/worktrees/<n>, or
// gitDir itself.
func commonDirOfGitDir(gitDir string) (string, bool) {
	b, err := os.ReadFile(filepath.Join(gitDir, "commondir"))
	if err != nil {
		return gitDir, true
	}
	c := strings.TrimSpace(string(b))
	if !filepath.IsAbs(c) {
		c = filepath.Join(gitDir, c)
	}
	return filepath.Clean(c), true
}

// shellQuote leaves a plain path as is and single-quotes anything else.
func shellQuote(s string) string {
	plain := s != ""
	for _, r := range s {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("_./+-", r)) {
			plain = false
			break
		}
	}
	if plain {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// BindingGitDir is the git dir to pin for b: the clone's admin dir for a
// linked worktree. Only when the binding has no separate primary repository
// (PrimaryAbs empty, equal to WorktreeAbs, or holding no .git at all) is it
// the worktree's own .git dir. A worker binding's primary is always its
// clone, so a .git dir planted in the worktree is never used there.
func BindingGitDir(b WorktreeBinding) (string, bool) {
	if b.WorktreeAbs == "" {
		return "", false
	}
	if linkedOnly.Load() {
		return WorktreeAdminDir(b.PrimaryAbs, b.WorktreeAbs)
	}
	plain := b.PrimaryAbs == "" || filepath.Clean(b.PrimaryAbs) == filepath.Clean(b.WorktreeAbs)
	if !plain {
		_, perr := os.Lstat(filepath.Join(b.PrimaryAbs, ".git"))
		plain = errors.Is(perr, fs.ErrNotExist)
	}
	if plain {
		p := filepath.Join(b.WorktreeAbs, ".git")
		if fi, err := os.Lstat(p); err == nil && fi.IsDir() {
			return p, true
		}
		return "", false
	}
	return WorktreeAdminDir(b.PrimaryAbs, b.WorktreeAbs)
}

// linkedOnly turns BindingGitDir's plain-checkout fallback off for the
// whole process (RequireLinkedWorktrees).
var linkedOnly atomic.Bool

// RequireLinkedWorktrees makes BindingGitDir accept only a worktree the
// clone records, never the worktree's own .git dir, whatever the binding
// says. dd-worker calls it at start: every worker worktree is linked to a
// clone, so a missing clone record means "not a worktree", not "a plain
// checkout" an agent could have planted.
func RequireLinkedWorktrees() { linkedOnly.Store(true) }

// PinnedBindingGit is PinnedWorktreeGit on b's worktree (BindingGitDir).
func PinnedBindingGit(b WorktreeBinding, args ...string) (*exec.Cmd, error) {
	dir, ok := BindingGitDir(b)
	if !ok {
		return nil, errors.New("not a registered worktree: " + b.Repo)
	}
	return PinnedWorktreeGit(b.PrimaryAbs, dir, b.WorktreeAbs, args...)
}
