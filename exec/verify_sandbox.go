package exec

import (
	"context"
	"fmt"
	"maps"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"

	"github.com/more-than-code/defect-drainer-worker/git"
	"github.com/more-than-code/defect-drainer-worker/internal/egress"
	"github.com/more-than-code/defect-drainer-worker/internal/sandbox"
)

// VerifySkippedSandboxUnavailable is VerifySkipped (and a run's
// SkippedReason) when the worker's verify sandbox cannot start. Verify never
// falls back to running unsandboxed.
const VerifySkippedSandboxUnavailable = "verify_sandbox_unavailable"

// VerifySandbox is how a worker runs verify commands (sandboxed verify,
// V6–V8). A Host without one skips them, unless it sets UnsandboxedVerify
// (the control plane's dev-only local runner, V10).
type VerifySandbox struct {
	Launcher sandbox.Launcher
	// Unavailable is why the sandbox cannot start (the self-test's
	// error); nil when it passed.
	Unavailable error
	// Hide are the worker's own dirs to hide besides Host.DenyRead: its
	// config dir, its whole state dir, the LoadCredential dir.
	Hide []string
	// Roots are where every bound path must lie: the job's dir (its
	// worktrees) and the clones dir (their git dirs).
	Roots []string
	// WorkDir is the worker-owned dir the run's tmp and cache dirs are
	// made in (the job's dir), never the agent-writable handoff ("": the
	// system temp dir).
	WorkDir string
	// OnRefuse logs a host the egress proxy refused.
	OnRefuse func(host string)
	// Dial, set only by tests, points the allowed names at a local fake
	// registry. Nil: the proxy's own dialer, which never dials this host.
	Dial func(ctx context.Context, network, addr string) (net.Conn, error)
}

// RunVerification runs every command the way this host must: in the
// sandbox when the host has one; unsandboxed only when the host opts in
// with UnsandboxedVerify (the dev-only local runner); else not at all.
func (h Host) RunVerification(ctx context.Context, commands []VerifyCommand, worktrees []git.WorktreeBinding, handoffAbs string, repoEntries []RepoEntry, extraEnv map[string]string, realByRepo map[string]string, onLog VerifyLog, strip []string) VerificationRun {
	if h.Verify == nil && h.UnsandboxedVerify {
		return RunVerification(ctx, commands, worktrees, handoffAbs, repoEntries, extraEnv, realByRepo, onLog, strip)
	}
	if len(commands) == 0 {
		return runVerification(ctx, nil, commands, worktrees, handoffAbs, repoEntries, realByRepo, onLog)
	}
	if onLog == nil {
		onLog = func(string, string) {}
	}
	box := h.Verify
	if box == nil {
		box = &VerifySandbox{Unavailable: fmt.Errorf("%w: no verify sandbox on this host", sandbox.ErrUnavailable)}
	}
	opened, err := box.open(h, worktrees, handoffAbs, extraEnv, strip, onLog)
	if err != nil {
		onLog("verify: sandbox unavailable — "+sandbox.Reason(err)+"; the checks were not run", "warn")
		run := VerificationRun{StartedAt: nowIso(), FinishedAt: nowIso(), Results: []VerifyResult{}, SkippedReason: VerifySkippedSandboxUnavailable}
		tryWriteVerifyJSON(handoffAbs, run)
		return run
	}
	defer opened.close()
	return runVerification(ctx, opened.command, commands, worktrees, handoffAbs, repoEntries, realByRepo, onLog)
}

// openBox is one verify run's sandbox: its egress proxy and layout.
type openBox struct {
	launcher sandbox.Launcher
	proxy    *egress.Proxy
	sockDir  string
	work     string
	base     sandbox.Run
}

// maxRefusalLines bounds the egress refusal lines one run logs.
const maxRefusalLines = 20

func (v *VerifySandbox) open(h Host, worktrees []git.WorktreeBinding, handoffAbs string, extraEnv map[string]string, strip []string, onLog VerifyLog) (*openBox, error) {
	if v.Unavailable != nil {
		return nil, v.Unavailable
	}
	// The run's tmp and cache dirs: fresh, in a dir the worker made, so a
	// symlink the agent left in its handoff can't redirect them.
	work, err := os.MkdirTemp(v.WorkDir, "verify-")
	if err != nil {
		return nil, fmt.Errorf("%w: %v", sandbox.ErrUnavailable, err)
	}
	tmp, cache := filepath.Join(work, "tmp"), filepath.Join(work, "cache")
	for _, d := range []string{tmp, cache} {
		if err := os.Mkdir(d, 0o700); err != nil {
			_ = os.RemoveAll(work)
			return nil, fmt.Errorf("%w: %v", sandbox.ErrUnavailable, err)
		}
	}
	// A short path: a Unix socket path is limited to about 100 bytes.
	sockDir, err := os.MkdirTemp("", "ddv-")
	if err != nil {
		_ = os.RemoveAll(work)
		return nil, fmt.Errorf("%w: %v", sandbox.ErrUnavailable, err)
	}
	sock := filepath.Join(sockDir, "egress.sock")
	// A refusal is logged by host name only (V1), in the job log and the
	// worker's: once per host, and at most maxRefusalLines hosts a run.
	var mu sync.Mutex
	seen := map[string]bool{}
	onRefuse := func(host string) {
		mu.Lock()
		first := !seen[host] && len(seen) < maxRefusalLines
		if first {
			seen[host] = true
		}
		mu.Unlock()
		if !first {
			return
		}
		onLog("verify: egress refused "+host+" (not in the allowed domains)", "warn")
		if v.OnRefuse != nil {
			v.OnRefuse(host)
		}
	}
	proxy, err := egress.Listen(sock, h.AllowedDomains, egress.Options{Dial: v.Dial, OnRefuse: onRefuse})
	if err != nil {
		_ = os.RemoveAll(sockDir)
		_ = os.RemoveAll(work)
		return nil, fmt.Errorf("%w: %v", sandbox.ErrUnavailable, err)
	}
	box := &openBox{launcher: v.Launcher, proxy: proxy, sockDir: sockDir, work: work}
	home, _ := os.UserHomeDir()
	writable := []string{tmp, cache}
	var readOnly []string
	for _, w := range worktrees {
		writable = append(writable, w.WorktreeAbs)
		// The worktree's .git entry and the clone's git dir are what
		// the ship path reads later: never writable from a check. The
		// entry must be the file git wrote, not a link the agent left.
		dotGit := filepath.Join(w.WorktreeAbs, ".git")
		if fi, err := os.Lstat(dotGit); err != nil || !fi.Mode().IsRegular() {
			box.close()
			return nil, fmt.Errorf("%w: layout: not a regular file: %s", sandbox.ErrUnavailable, dotGit)
		}
		readOnly = append(readOnly, dotGit)
		if dir, ok := git.BindingGitDir(w); ok {
			readOnly = append(readOnly, gitCommonDir(dir))
		}
	}
	hide := append(append(h.DenyRead(), v.Hide...), h.MaskedSockets...)
	box.base = sandbox.Run{
		Home:      home,
		Hide:      hide,
		Roots:     append(append([]string{}, v.Roots...), work),
		Writable:  writable,
		ReadOnly:  readOnly,
		Env:       verifySandboxEnv(home, tmp, cache, extraEnv, strip),
		ProxySock: sock,
	}
	// Refuse a bad layout now, before any command, as a skip.
	if _, err := v.Launcher.Args(box.base); err != nil {
		box.close()
		return nil, err
	}
	return box, nil
}

// close removes the run's dirs. RemoveAll never follows a link inside.
func (b *openBox) close() {
	_ = b.proxy.Close()
	_ = os.RemoveAll(b.sockDir)
	_ = os.RemoveAll(b.work)
}

func (b *openBox) command(ctx context.Context, command, dir string) (verifyProc, error) {
	r := b.base
	r.Command, r.Dir = command, dir
	p, err := b.launcher.Command(ctx, r)
	if err != nil {
		return verifyProc{}, err
	}
	return verifyProc{cmd: p.Cmd, start: p.Start, reached: p.Reached}, nil
}

// gitCommonDir is a linked worktree's admin dir's clone git dir
// (<clone>/.git/worktrees/<name> -> <clone>/.git), else dir itself.
func gitCommonDir(dir string) string {
	parent := filepath.Dir(dir)
	if filepath.Base(parent) == "worktrees" {
		return filepath.Dir(parent)
	}
	return dir
}

// verifySandboxEnv is V7: built, never inherited, so nothing of the
// worker's own environment (its settings, secrets.env, adapter keys) is in
// it. PATH is the host's (the toolchain's PATH when the job has one), HOME
// an empty tmpfs, the caches under the run's cache dir, the proxy variables
// set, and the job's extra env without the strip list, the test password or
// a DD_ name.
func verifySandboxEnv(home, tmp, cache string, extraEnv map[string]string, strip []string) []string {
	path := os.Getenv("PATH")
	if p := extraEnv["PATH"]; p != "" {
		path = p
	}
	if path == "" {
		path = "/usr/local/bin:/usr/bin:/bin"
	}
	lang := os.Getenv("LANG")
	if lang == "" {
		lang = "C.UTF-8"
	}
	env := []string{
		"PATH=" + path,
		"HOME=" + home,
		"TMPDIR=" + tmp,
		"LANG=" + lang,
		"CI=1",
		"HTTPS_PROXY=" + sandbox.ProxyURL, "https_proxy=" + sandbox.ProxyURL,
		"HTTP_PROXY=" + sandbox.ProxyURL, "http_proxy=" + sandbox.ProxyURL,
		"XDG_CACHE_HOME=" + cache,
		"npm_config_cache=" + filepath.Join(cache, "npm"),
		"GOCACHE=" + filepath.Join(cache, "go-build"),
		"GOMODCACHE=" + filepath.Join(cache, "go-mod"),
		"PIP_CACHE_DIR=" + filepath.Join(cache, "pip"),
		"PUB_CACHE=" + filepath.Join(cache, "pub"),
		"CARGO_HOME=" + filepath.Join(cache, "cargo"),
	}
	set := map[string]bool{}
	for _, kv := range env {
		k, _, _ := strings.Cut(kv, "=")
		set[k] = true
	}
	drop := map[string]bool{"DD_TEST_ACCOUNT_PASSWORD": true}
	for _, s := range strip {
		drop[s] = true
	}
	for _, k := range slices.Sorted(maps.Keys(extraEnv)) {
		v := extraEnv[k]
		if k == "" || set[k] || drop[k] || strings.HasPrefix(k, "DD_") {
			continue
		}
		env = append(env, k+"="+v)
	}
	return env
}
