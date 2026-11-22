package worker

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	osexec "os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	dexec "github.com/more-than-code/defect-drainer-worker/exec"
	"github.com/more-than-code/defect-drainer-worker/git"
	"github.com/more-than-code/defect-drainer-worker/internal/sandbox"
	"github.com/more-than-code/defect-drainer-worker/workerproto"
)

// Sandboxed verify acceptance 1–5, with the real bwrap and socat.
// packaging/test/sandbox_test.sh runs it in a bookworm container with no
// network (DD_VERIFY_E2E=1). The worker
// layout is a real one: /etc/dd-worker, a state dir with the credential,
// status.json, clones, other jobs and agent state, and a $HOME with
// dotfiles. Each check is a verify command run through Host.RunVerification
// exactly as a job runs it; the fake registry is a local server the egress
// proxy's test dialer points the allowed names at.
func TestVerifySandboxBwrap(t *testing.T) {
	if os.Getenv("DD_VERIFY_E2E") != "1" {
		t.Skip("DD_VERIFY_E2E=1 (packaging/test/sandbox_test.sh)")
	}
	e := newVerifyE2E(t)

	// The control: the same checks unsandboxed do get through, so a pass
	// below means the sandbox stopped them.
	t.Run("0-control-unsandboxed", func(t *testing.T) {
		run := dexec.RunVerification(context.Background(), []dexec.VerifyCommand{{Repo: "app", Command: e.isolationScript()}}, e.wts, filepath.Join(e.root, "control"), nil, map[string]string{"WORKER_PID": strconv.Itoa(e.victim.Process.Pid)}, e.reals, nil, nil)
		out := run.Results[0].OutputTail
		for _, want := range []string{"BREACH read " + e.paths.Secrets(), "BREACH socket /run/dd-e2e.sock", "BREACH abstract socket", "BREACH write clone .git", "BREACH signal worker"} {
			if !strings.Contains(out, want) {
				t.Errorf("unsandboxed control lacks %q:\n%s", want, out)
			}
		}
		for _, f := range e.plants {
			_ = os.Remove(f)
		}
		_ = os.Remove(filepath.Join(e.cloneGit, "hooks", "planted"))
		_ = os.Remove(filepath.Join(e.wt, "worktree-write"))
		_ = os.Remove("/usr/planted")
		_ = os.Remove("/tmp-write")
		// kill -0 got through, so -KILL may have too: a new stand-in,
		// kept for the whole test.
		e.newVictim(e.top)
		// The control appended to the worktree's .git file: put it back.
		writeFile(t, filepath.Join(e.wt, ".git"), e.protected[filepath.Join(e.wt, ".git")], 0o644)
	})

	t.Run("1-isolation", func(t *testing.T) {
		script := e.isolationScript()
		res := e.run(t, script, nil)
		if !res.Ok || strings.Contains(res.OutputTail, "BREACH") || !strings.Contains(res.OutputTail, "done") {
			t.Fatalf("%+v", res)
		}
		if err := e.victim.Process.Signal(syscall.Signal(0)); err != nil {
			t.Errorf("the host process the check tried to kill is gone: %v", err)
		}
		for _, f := range e.plants {
			if _, err := os.Lstat(f); err == nil {
				t.Errorf("%s reached the real disk", f)
			}
		}
		for f, want := range e.protected {
			if b, err := os.ReadFile(f); err != nil || string(b) != want {
				t.Errorf("%s changed: %q %v", f, b, err)
			}
		}
		if _, err := os.Stat(filepath.Join(e.wt, "worktree-write")); err != nil {
			t.Errorf("the worktree write did not land: %v", err)
		}
	})

	t.Run("2-egress", func(t *testing.T) {
		script := strings.Join([]string{
			`curl -sf http://registry.example.test/ping | grep -q pong || echo "FAIL plain allowed"`,
			`curl -sfp http://registry.example.test/ping | grep -q pong || echo "FAIL connect allowed"`,
			`if curl -sf http://other.example.test/ping; then echo "BREACH other host"; fi`,
			`if curl -sfp http://other.example.test/ping; then echo "BREACH other host connect"; fi`,
			`if curl -sf http://192.0.2.1/ping; then echo "BREACH raw ip"; fi`,
			`if curl -sf --noproxy '*' --max-time 5 http://127.0.0.1:` + e.registryPort + `/ping; then echo "BREACH direct"; fi`,
			`echo done`,
		}, "\n")
		res := e.run(t, script, []string{"registry.example.test"})
		if !res.Ok || strings.Contains(res.OutputTail, "BREACH") || strings.Contains(res.OutputTail, "FAIL") || !strings.Contains(res.OutputTail, "done") {
			t.Fatalf("%+v", res)
		}
		var refused []string
		for _, l := range strings.Split(e.logs(), "\n") {
			if strings.Contains(l, "egress refused") {
				refused = append(refused, l)
			}
		}
		logs := strings.Join(refused, "\n")
		if !strings.Contains(logs, "verify: egress refused other.example.test") || !strings.Contains(logs, "verify: egress refused 192.0.2.1") {
			t.Fatalf("refusals not logged:\n%s", logs)
		}
		if strings.Contains(logs, "/ping") {
			t.Fatalf("a refusal logged more than the host:\n%s", logs)
		}
	})

	t.Run("3-install-like-fetch", func(t *testing.T) {
		script := `curl -sf -o "$TMPDIR/pkg.tgz" http://registry.example.test/pkg.tgz && mkdir -p node_modules/pkg && tar -xzf "$TMPDIR/pkg.tgz" -C node_modules/pkg && test -f node_modules/pkg/package/index.js`
		if res := e.run(t, script, []string{"registry.example.test"}); !res.Ok {
			t.Fatalf("allowed fetch: %+v", res)
		}
		res := e.run(t, `curl -sSf -o "$TMPDIR/pkg.tgz" http://registry.example.test/pkg.tgz`, nil)
		if res.Ok || res.ExitCode == nil || !strings.Contains(res.OutputTail, "403") {
			t.Fatalf("not allowed: want a clean failure with the 403, got %+v", res)
		}
	})

	t.Run("4-timeout-kills-tree", func(t *testing.T) {
		// A renamed sleep, so the host scan finds exactly these.
		marker := "dd-e2e-grandchild-" + strconv.Itoa(os.Getpid())
		script := `ln -sf "$(command -v sleep)" "$TMPDIR/` + marker + `"; ("$TMPDIR/` + marker + `" 300; touch survivor) & "$TMPDIR/` + marker + `" 301`
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		started := time.Now()
		run := e.runCtx(ctx, script, nil)
		if took := time.Since(started); took > 10*time.Second {
			t.Fatalf("RunVerification took %v after the cancel", took)
		}
		if len(run.Results) != 1 || run.Results[0].Ok {
			t.Fatalf("%+v", run)
		}
		time.Sleep(time.Second)
		if pids := processesNamed(t, marker); len(pids) > 0 {
			t.Fatalf("processes survived the sandbox: %v", pids)
		}
		if _, err := os.Stat(filepath.Join(e.wt, "survivor")); err == nil {
			t.Fatal("a grandchild ran on after the timeout")
		}
	})

	// H1: the agent replaced the worktree's .git with a link to a secret.
	// The run is refused as a layout the sandbox won't build; the secret
	// is never mounted, read or printed.
	t.Run("6-dotgit-symlink", func(t *testing.T) {
		dotGit := filepath.Join(e.wt, ".git")
		orig := e.protected[dotGit]
		if err := os.Remove(dotGit); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(e.paths.Env(), dotGit); err != nil {
			t.Fatal(err)
		}
		defer func() {
			_ = os.Remove(dotGit)
			writeFile(t, dotGit, orig, 0o644)
		}()
		run := e.runCtx(context.Background(), "cat .git; cat .git/../env", nil)
		if run.Ran || run.SkippedReason != dexec.VerifySkippedSandboxUnavailable || len(run.Results) != 0 {
			t.Fatalf("%+v", run)
		}
		if strings.Contains(e.logs(), "dd_wrk_") {
			t.Fatal("the credential reached the log")
		}
		if !strings.Contains(e.logs(), "layout: not a regular file") {
			t.Fatalf("no layout refusal logged:\n%s", e.logs())
		}
	})

	// H2: links the agent left in its handoff (where the tmp and cache
	// dirs used to be) are neither followed nor removed through.
	t.Run("7-handoff-symlink", func(t *testing.T) {
		victim := filepath.Join(e.root, "victim")
		writeFile(t, filepath.Join(victim, "keep"), "keep\n", 0o644)
		for _, l := range []struct{ at, to string }{{".verify", victim}, {".cache", "/"}, {"tmp", victim}} {
			if err := os.Symlink(l.to, filepath.Join(e.handoff, l.at)); err != nil {
				t.Fatal(err)
			}
		}
		res := e.run(t, `touch "$TMPDIR/x" "$XDG_CACHE_HOME/y" && echo "tmp=$TMPDIR"`, nil)
		if !res.Ok || !strings.Contains(res.OutputTail, "tmp="+e.paths.JobDir("j1")+"/verify-") {
			t.Fatalf("%+v", res)
		}
		entries, _ := os.ReadDir(victim)
		if len(entries) != 1 || entries[0].Name() != "keep" {
			t.Fatalf("the victim dir changed: %v", entries)
		}
		left, _ := filepath.Glob(filepath.Join(e.paths.JobDir("j1"), "verify-*"))
		if len(left) != 0 {
			t.Fatalf("the run's dirs were not removed: %v", left)
		}
	})

	// M3: a sandbox that fails to start (here the bridge: a socat that
	// exits) is no result of the check: the run is skipped, not failed.
	t.Run("8-setup-failure", func(t *testing.T) {
		host := e.host([]string{"registry.example.test"})
		host.Verify.Launcher.Socat = "/bin/false"
		run := host.RunVerification(context.Background(), []dexec.VerifyCommand{{Repo: "app", Command: "true"}}, e.wts, e.handoff, nil, nil, e.reals, nil, nil)
		if run.Ran || run.SkippedReason != dexec.VerifySkippedSandboxUnavailable || len(run.Results) != 0 {
			t.Fatalf("%+v", run)
		}
	})

	t.Run("5-no-bwrap", func(t *testing.T) {
		t.Setenv("PATH", t.TempDir())
		_, err := findVerifySandbox(context.Background())
		if err == nil || verifyReason(err) != "bwrap: not found" {
			t.Fatalf("find: %v", err)
		}
		v := verifyState{checked: true, err: err}
		if slices.Contains(capabilities(v), workerproto.CapVerifySandboxV1) {
			t.Fatal("verify_sandbox.v1 advertised without bwrap")
		}
		if vs := verifySandboxWire(v); vs.Ready || vs.Reason != "bwrap: not found" {
			t.Fatalf("hello verify_sandbox %+v", vs)
		}
		host := e.host(nil)
		host.Verify = newVerifyBox(e.paths, "j1", sandbox.Launcher{}, err, t.Logf)
		marker := filepath.Join(e.root, "ran-unsandboxed")
		run := host.RunVerification(context.Background(), []dexec.VerifyCommand{{Repo: "app", Command: "touch " + marker}}, e.wts, e.handoff, nil, nil, e.reals, nil, nil)
		if run.Ran || run.SkippedReason != dexec.VerifySkippedSandboxUnavailable {
			t.Fatalf("%+v", run)
		}
		if _, err := os.Stat(marker); err == nil {
			t.Fatal("a check ran without the sandbox")
		}
	})
}

// isolationScript prints a BREACH line for each thing acceptance 1 says a
// check cannot do, and "done" at the end.
func (e *verifyE2E) isolationScript() string {
	return strings.Join([]string{
		`for f in ` + strings.Join(e.secretFiles, " ") + `; do if cat "$f" >/dev/null 2>&1; then echo "BREACH read $f"; fi; done`,
		`for d in ` + strings.Join(e.secretDirs, " ") + `; do if [ -n "$(ls -A "$d" 2>/dev/null)" ]; then echo "BREACH list $d"; fi; done`,
		// $HOME is an empty tmpfs; it shows only the mount points on
		// the way to this job's own worktree (the test tree is in it).
		`if ls -A "$HOME" | grep -v '^dd-verify-e2e-' | grep -q .; then echo "BREACH home not empty: $(ls -A "$HOME" | tr '\n' ' ')"; fi`,
		`for f in ` + strings.Join(e.plants, " ") + `; do touch "$f" 2>/dev/null; done`,
		`if echo x >> .git 2>/dev/null; then echo "BREACH write .git file"; fi`,
		`if touch ` + e.cloneGit + `/hooks/planted 2>/dev/null; then echo "BREACH write clone .git"; fi`,
		`if touch /usr/planted 2>/dev/null; then echo "BREACH write /usr"; fi`,
		`for s in ` + strings.Join(e.sockets, " ") + `; do if socat -u OPEN:/dev/null UNIX-CONNECT:"$s" 2>/dev/null; then echo "BREACH socket $s"; fi; done`,
		`if socat -u OPEN:/dev/null ABSTRACT-CONNECT:` + e.abstract + ` 2>/dev/null; then echo "BREACH abstract socket"; fi`,
		`if kill -0 "$WORKER_PID" 2>/dev/null || kill -KILL "$WORKER_PID" 2>/dev/null; then echo "BREACH signal worker"; fi`,
		`touch ./worktree-write "$TMPDIR/tmp-write" || echo "BREACH cannot write its own dirs"`,
		`echo done`,
	}, "\n")
}

type verifyE2E struct {
	root, wt, handoff, cloneGit string
	paths                       Paths
	wts                         []git.WorktreeBinding
	reals                       map[string]string
	launcher                    sandbox.Launcher
	dial                        func(ctx context.Context, network, addr string) (net.Conn, error)
	registryPort                string
	secretFiles, secretDirs     []string
	plants, sockets             []string
	abstract                    string
	victim                      *osexec.Cmd // a host process, standing in for the worker
	top                         *testing.T
	protected                   map[string]string

	mu    sync.Mutex
	lines []string
}

func newVerifyE2E(t *testing.T) *verifyE2E {
	t.Helper()
	l, err := findVerifySandbox(context.Background())
	if err != nil {
		t.Fatalf("self-test: %v", err)
	}
	git.RequireLinkedWorktrees()
	// Not under /tmp: a host's state is not there, and /tmp is hidden too.
	home, _ := os.UserHomeDir()
	root, err := os.MkdirTemp(home, "dd-verify-e2e-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	root, _ = filepath.EvalSymlinks(root)
	e := &verifyE2E{root: root, launcher: l, protected: map[string]string{}, top: t}
	e.paths = Paths{ConfigDir: filepath.Join(root, "etc", "dd-worker"), StateDir: filepath.Join(root, "state")}
	p := e.paths
	put := func(path, body string) {
		writeFile(t, path, body, 0o600)
		e.protected[path] = body
	}
	put(p.Policy(), "enabled = true\n")
	put(p.Secrets(), "OTHER_SECRET=hunter2\n")
	put(p.Env(), "DD_WORKER_CREDENTIAL=dd_wrk_x\n")
	put(p.StatusFile(), "{}\n")
	put(filepath.Join(p.StateDir, "agent-state", "j1", "settings.json"), "{}\n")
	put(filepath.Join(p.JobDir("j2"), "handoff", "notes.md"), "other job\n")
	put(filepath.Join(home, ".ssh", "id_ed25519"), "PRIVATE\n")
	put(filepath.Join(home, ".gitconfig"), "[user]\n")
	put(filepath.Join(home, ".npmrc"), "//registry/:_authToken=x\n")
	other := filepath.Join(p.ReposDir(), "other")
	gitInit(t, other)
	put(filepath.Join(other, "secret.txt"), "other clone\n")
	for f := range e.protected {
		e.secretFiles = append(e.secretFiles, f)
	}
	slices.Sort(e.secretFiles)
	e.secretDirs = []string{p.ConfigDir, p.StateDir + "/agent-state", p.JobDir("j2"), other, filepath.Join(home, ".ssh")}
	e.plants = []string{p.StatusFile() + ".planted", filepath.Join(p.ConfigDir, "planted"), filepath.Join(p.JobDir("j1"), "handoff", "planted"), filepath.Join(home, "planted"), "/tmp/dd-e2e-planted"}

	// This job's clone and its linked worktree.
	clone := filepath.Join(p.ReposDir(), "app")
	gitInit(t, clone)
	e.cloneGit = filepath.Join(clone, ".git")
	e.wt = filepath.Join(p.WorktreeRoot("j1"), "app")
	if out, err := osexec.Command("git", "-C", clone, "worktree", "add", "-q", "-b", "dd/j1/app", e.wt).CombinedOutput(); err != nil {
		t.Fatalf("worktree add: %v %s", err, out)
	}
	e.protected[filepath.Join(e.wt, ".git")] = readString(t, filepath.Join(e.wt, ".git"))
	e.handoff = p.Handoff("j1")
	if err := os.MkdirAll(e.handoff, 0o755); err != nil {
		t.Fatal(err)
	}
	e.wts = []git.WorktreeBinding{{Repo: "app", PrimaryAbs: clone, WorktreeAbs: e.wt, Branch: "dd/j1/app"}}
	e.reals = dexec.SnapshotWorktreeReals(e.wts)

	// Host sockets: in the masked dirs, in the state dir, and abstract.
	for _, s := range []string{"/run/dd-e2e.sock", "/tmp/dd-e2e.sock", filepath.Join(p.StateDir, "w.sock")} {
		e.sockets = append(e.sockets, s)
		listenUnix(t, s)
	}
	e.abstract = "dd-e2e-" + strconv.Itoa(os.Getpid())
	listenUnix(t, "@"+e.abstract)

	// A host process the checks try to signal (the test process is PID 1
	// in the container, as bwrap is inside the sandbox).
	e.newVictim(t)

	// The fake registry, reached only through the proxy's test dialer.
	tgz := packageTgz(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/ping":
			fmt.Fprint(w, "pong")
		case "/pkg.tgz":
			_, _ = w.Write(tgz)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	_, e.registryPort, _ = net.SplitHostPort(srv.Listener.Addr().String())
	addr := srv.Listener.Addr().String()
	e.dial = func(ctx context.Context, network, _ string) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, network, addr)
	}
	return e
}

func (e *verifyE2E) newVictim(t *testing.T) {
	t.Helper()
	v := osexec.Command("sleep", "600")
	if err := v.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = v.Process.Kill(); _ = v.Wait() })
	e.victim = v
}

func (e *verifyE2E) host(domains []string) dexec.Host {
	h := hostAdapter{name: "claude"}
	host := h.host(e.paths, "j1", "", e.wts, domains)
	box := newVerifyBox(e.paths, "j1", e.launcher, nil, func(string, ...any) {})
	box.Dial = e.dial
	host.Verify = box
	return host
}

func (e *verifyE2E) runCtx(ctx context.Context, script string, domains []string) dexec.VerificationRun {
	extra := map[string]string{"WORKER_PID": strconv.Itoa(e.victim.Process.Pid)}
	return e.host(domains).RunVerification(ctx, []dexec.VerifyCommand{{Repo: "app", Command: script}}, e.wts, e.handoff, nil, extra, e.reals, func(line, _ string) {
		e.mu.Lock()
		e.lines = append(e.lines, line)
		e.mu.Unlock()
	}, nil)
}

func (e *verifyE2E) run(t *testing.T, script string, domains []string) dexec.VerifyResult {
	t.Helper()
	run := e.runCtx(context.Background(), script, domains)
	if len(run.Results) != 1 {
		t.Fatalf("%+v", run)
	}
	return run.Results[0]
}

func (e *verifyE2E) logs() string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return strings.Join(e.lines, "\n")
}

func gitInit(t *testing.T, dir string) {
	t.Helper()
	for _, args := range [][]string{
		{"init", "-q", "-b", "main", dir},
		{"-C", dir, "-c", "user.name=t", "-c", "user.email=t@example.test", "commit", "-q", "--allow-empty", "-m", "init"},
	} {
		if out, err := osexec.Command("git", args...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
}

func readString(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func listenUnix(t *testing.T, path string) {
	t.Helper()
	if !strings.HasPrefix(path, "@") {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		_ = os.Remove(path)
	}
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatalf("listen %s: %v", path, err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			_ = c.Close()
		}
	}()
}

func packageTgz(t *testing.T) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	body := []byte("module.exports = 1\n")
	if err := tw.WriteHeader(&tar.Header{Name: "package/index.js", Mode: 0o644, Size: int64(len(body))}); err != nil {
		t.Fatal(err)
	}
	_, _ = tw.Write(body)
	_ = tw.Close()
	_ = gz.Close()
	return buf.Bytes()
}

// processesNamed are the host pids whose argv[0] ends in /name.
func processesNamed(t *testing.T, name string) []string {
	t.Helper()
	entries, err := os.ReadDir("/proc")
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, ent := range entries {
		if _, err := strconv.Atoi(ent.Name()); err != nil {
			continue
		}
		b, err := os.ReadFile(filepath.Join("/proc", ent.Name(), "cmdline"))
		argv0, _, _ := strings.Cut(string(b), "\x00")
		if err == nil && strings.HasSuffix(argv0, "/"+name) {
			out = append(out, ent.Name())
		}
	}
	return out
}
