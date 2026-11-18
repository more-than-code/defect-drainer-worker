package git

import (
	"os"
	"os/exec"
	"strconv"
	"strings"

	"github.com/more-than-code/defect-drainer-worker/agentadapter"
)

// hostToolNames are the variables git and gh may need: identity and paths,
// locale, ssh-agent and gpg signing, TLS roots, proxies, and gh's own auth.
// Everything else in serve's env (DB URL, API keys, an app's env: secret
// such as QA_PASSWORD, anything else .env loaded) is left out.
var hostToolNames = map[string]bool{
	"PATH": true, "HOME": true, "USER": true, "LOGNAME": true, "SHELL": true,
	"TMPDIR": true, "LANG": true, "TERM": true,
	"SSH_AUTH_SOCK": true, "SSH_ASKPASS": true, "GNUPGHOME": true, "GPG_TTY": true,
	"SSL_CERT_FILE": true, "SSL_CERT_DIR": true,
	"HTTP_PROXY": true, "HTTPS_PROXY": true, "NO_PROXY": true, "ALL_PROXY": true,
	"http_proxy": true, "https_proxy": true, "no_proxy": true, "all_proxy": true,
	"GITHUB_TOKEN": true, "GITHUB_ENTERPRISE_TOKEN": true,
	"XDG_CONFIG_HOME": true,
}

// hostToolPrefixes are kept whole. LC_, GIT_ and GH_ are reserved names an
// app's env: secret_ref may not use (for rows saved after that check).
var hostToolPrefixes = []string{"LC_", "GIT_", "GH_"}

// hostToolGitRepoNames are GIT_ names that would point every `git -C dir`
// at another repository, index or object store. DD always names the repo.
var hostToolGitRepoNames = map[string]bool{
	"GIT_DIR": true, "GIT_WORK_TREE": true, "GIT_INDEX_FILE": true,
	"GIT_OBJECT_DIRECTORY": true, "GIT_ALTERNATE_OBJECT_DIRECTORIES": true,
	"GIT_COMMON_DIR": true, "GIT_NAMESPACE": true,
}

func hostToolName(key string) bool {
	if hostToolGitRepoNames[key] {
		return false
	}
	if hostToolNames[key] {
		return true
	}
	for _, p := range hostToolPrefixes {
		if strings.HasPrefix(key, p) {
			return true
		}
	}
	return false
}

// HostToolEnv is the environment for DD's own git and gh children (status,
// commit, merge, push, fetch, worktree, pr create/view). It is an allowlist
// (hostToolNames, hostToolPrefixes) of os.Environ, then agentadapter.ChildEnv
// with no credential kept, so the DD names, DD_TEST_ACCOUNT_PASSWORD and every
// coding-agent credential stay out even if the allowlist ever grows to match.
func HostToolEnv() []string {
	environ := os.Environ()
	kept := make([]string, 0, len(environ))
	for _, e := range environ {
		if key, _, ok := strings.Cut(e, "="); ok && hostToolName(key) {
			kept = append(kept, e)
		}
	}
	return agentadapter.ChildEnv(kept, nil, nil)
}

// noHooks turns off repository hooks for every git command DD runs. A
// coding agent can write hooks into its worktree (an in-tree .husky/ named by
// core.hooksPath, which a local merge then brings into the primary tree), and
// DD must not run them on the host outside the agent's sandbox.
var noHooks = []string{"-c", "core.hooksPath=/dev/null"}

// HostGit is the one way DD starts its own git: hooks off, HostToolEnv.
// Callers may append to cmd.Env, or narrow it, but never replace it with an
// inherited environment.
func HostGit(args ...string) *exec.Cmd {
	return hostCmd("git", append(append([]string{}, noHooks...), args...)...)
}

// hostCmd starts bin (git or gh) with HostToolEnv and hooks off. Call it
// with "git" only through HostGit.
func hostCmd(bin string, args ...string) *exec.Cmd {
	cmd := exec.Command(bin, args...)
	cmd.Env = withHooksOff(HostToolEnv())
	return cmd
}

// withHooksOff sets core.hooksPath=/dev/null through git's environment
// config (GIT_CONFIG_COUNT/KEY_n/VALUE_n), so the git that gh starts runs no
// hook either. An operator's existing GIT_CONFIG_COUNT list is extended.
func withHooksOff(env []string) []string {
	return withGitConfig(env, "core.hooksPath", "/dev/null")
}

// withGitConfig appends key/value pairs to env's GIT_CONFIG_COUNT list
// (starting a new one when there is none). kv alternates key and value.
func withGitConfig(env []string, kv ...string) []string {
	n := 0
	out := make([]string, 0, len(env)+1+len(kv))
	for _, e := range env {
		if v, ok := strings.CutPrefix(e, "GIT_CONFIG_COUNT="); ok {
			if c, err := strconv.Atoi(v); err == nil && c > 0 {
				n = c
			}
			continue
		}
		out = append(out, e)
	}
	out = append(out, "GIT_CONFIG_COUNT="+strconv.Itoa(n+len(kv)/2))
	for i := 0; i+1 < len(kv); i += 2 {
		out = append(out,
			"GIT_CONFIG_KEY_"+strconv.Itoa(n)+"="+kv[i],
			"GIT_CONFIG_VALUE_"+strconv.Itoa(n)+"="+kv[i+1],
		)
		n++
	}
	return out
}

// AddGitConfigEnv adds per-command git config (kv alternates key and value)
// to a HostGit command through the same GIT_CONFIG_COUNT list as the hooks
// setting: never argv, never a config file. A remote worker passes its
// GitHub token header this way.
func AddGitConfigEnv(cmd *exec.Cmd, kv ...string) {
	cmd.Env = withGitConfig(cmd.Env, kv...)
}

// DropEnv removes the named variables from a HostGit command's env.
func DropEnv(cmd *exec.Cmd, names ...string) {
	drop := make(map[string]bool, len(names))
	for _, n := range names {
		drop[n] = true
	}
	out := cmd.Env[:0:0]
	for _, e := range cmd.Env {
		if k, _, _ := strings.Cut(e, "="); !drop[k] {
			out = append(out, e)
		}
	}
	cmd.Env = out
}
