package worker

import (
	"context"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/cgi"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	dexec "github.com/more-than-code/defect-drainer-worker/exec"
	"github.com/more-than-code/defect-drainer-worker/workerproto"
)

// hostGitCreds are host credentials a token git command must not get
// (§14). GIT_CONFIG_PARAMETERS is dropped too: git reads it after
// GIT_CONFIG_COUNT and it could re-add a credential helper.
var hostGitCreds = map[string]string{
	"GITHUB_TOKEN":            "host-github-token-x1",
	"GH_TOKEN":                "host-gh-token-x2",
	"GITHUB_ENTERPRISE_TOKEN": "host-ghe-token-x3",
	"GH_ENTERPRISE_TOKEN":     "host-ghe-token-x4",
	"SSH_AUTH_SOCK":           "/tmp/host-agent-x5.sock",
	"SSH_ASKPASS":             "/bin/host-askpass-x6",
	"GIT_ASKPASS":             "/bin/host-askpass-x7",
	"GIT_CONFIG_PARAMETERS":   "'credential.helper'='host-helper-x8'",
}

// gitWrapper puts a git first on PATH that appends its argv and env to log,
// then runs the real git.
func gitWrapper(t *testing.T) (log string) {
	t.Helper()
	real, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	log = filepath.Join(t.TempDir(), "git.log")
	writeFile(t, filepath.Join(dir, "git"), "#!/bin/sh\n"+
		"{ printf 'ARGV %s\\n' \"$*\"; env | sed 's/^/ENV /'; echo END; } >> '"+log+"'\n"+
		"exec '"+real+"' \"$@\"\n", 0o755)
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return log
}

type gitCall struct {
	argv string
	env  map[string]string
	cfg  map[string][]string // GIT_CONFIG_KEY_n → values, in order
}

func readGitCalls(t *testing.T, log string) []gitCall {
	t.Helper()
	b, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	var calls []gitCall
	var cur *gitCall
	for _, line := range strings.Split(string(b), "\n") {
		switch {
		case strings.HasPrefix(line, "ARGV "):
			calls = append(calls, gitCall{argv: strings.TrimPrefix(line, "ARGV "), env: map[string]string{}, cfg: map[string][]string{}})
			cur = &calls[len(calls)-1]
		case strings.HasPrefix(line, "ENV ") && cur != nil:
			k, v, _ := strings.Cut(strings.TrimPrefix(line, "ENV "), "=")
			cur.env[k] = v
		case line == "END" && cur != nil:
			for i := 0; ; i++ {
				k, ok := cur.env["GIT_CONFIG_KEY_"+strconv.Itoa(i)]
				if !ok {
					break
				}
				cur.cfg[k] = append(cur.cfg[k], cur.env["GIT_CONFIG_VALUE_"+strconv.Itoa(i)])
			}
			cur = nil
		}
	}
	return calls
}

// isNetworkGit: the git commands that reach the origin.
func isNetworkGit(argv string) bool {
	for _, w := range strings.Fields(argv) {
		switch w {
		case "clone", "fetch", "push", "ls-remote":
			return true
		}
	}
	return false
}

// §14 and acceptance 5, 6, 7, 8 (worker side): a fix job and a ship with
// host GitHub/SSH credentials in the worker's env. Every clone, fetch and
// push gets its token only through GIT_CONFIG_* env, after an empty
// credential.helper, with prompts off and no host credential; no argv,
// .git/config, state file, log, event or child env holds a token; gh
// never runs.
func TestGitTokenPathAndNoTokenAtRest(t *testing.T) {
	for k, v := range hostGitCreds {
		t.Setenv(k, v)
	}
	dumps := t.TempDir()
	agentEnv, verifyEnv := filepath.Join(dumps, "agent.env"), filepath.Join(dumps, "verify.env")
	agent := "env > '" + agentEnv + "'\n" + fixArtifacts(testDefect) +
		"echo fixed >> ../worktrees/app/README.md\n" +
		"cat '" + claudeFixture(t, "c1-basic") + "'\nexit 0\n"
	h := newHarness(t, harnessOpts{agent: agent})
	// After the harness: setupGit's own fixture git is not the worker's.
	gitLog := gitWrapper(t)
	ghLog := fakeGH(t)
	h.fs.mu.Lock()
	h.fs.withholdAcks = true // keep the spool on disk for the grep
	h.fs.mu.Unlock()
	id := "bjob_tok"
	spec := fixSpec(id)
	spec.App.VerifyCommands = []dexec.VerifyCommand{{Repo: "app", Command: "env > '" + verifyEnv + "'"}}
	h.offerJob(spec, nil)
	if d := h.waitDone(id, 60*time.Second); d.State == "failed" {
		t.Fatalf("job failed: %+v", d)
	}
	ship := dexec.Control{Kind: dexec.ControlShip, Repos: []dexec.ShipRepo{{Repo: "app", Files: []string{"README.md"}, Message: "Fix it", Title: "Fix it"}}}
	r := h.controlAndWait(id, "rq_tok_ship", ship, 1)
	var res workerproto.ShipPushResult
	if !r.OK || json.Unmarshal(r.Result, &res) != nil || len(res.Repos) != 1 || !res.Repos[0].Pushed {
		t.Fatalf("ship %+v %s", r, r.Result)
	}

	// The mints: read for the clone with the offer's job token, write for
	// the push with the control's.
	var tokens []string
	var reads, writes int
	for _, m := range h.fs.mints() {
		if m.status != http.StatusOK || m.jobID != id || m.repo != "app" {
			t.Fatalf("unexpected mint %+v", m)
		}
		tokens = append(tokens, m.token)
		switch m.access {
		case workerproto.GitAccessRead:
			reads++
		case workerproto.GitAccessWrite:
			writes++
			if m.bearer != "dd_wjt_ctl_rq_tok_ship" {
				t.Errorf("write minted with %q", m.bearer)
			}
		}
	}
	if reads != 1 || writes != 1 {
		t.Fatalf("mints: %d read, %d write", reads, writes)
	}

	// Every network git: the header and the empty helper in env, nothing
	// of the host's; no git argv holds a token.
	network := 0
	for _, c := range readGitCalls(t, gitLog) {
		for _, tok := range tokens {
			if strings.Contains(c.argv, tok) || strings.Contains(c.argv, basicAuth(tok)) {
				t.Fatalf("token in argv: git %s", c.argv)
			}
		}
		if !isNetworkGit(c.argv) {
			continue
		}
		network++
		helpers := c.cfg["credential.helper"]
		if len(helpers) != 1 || helpers[0] != "" {
			t.Errorf("git %s: credential.helper %q", c.argv, helpers)
		}
		hdr := c.cfg["http."+gitHubOrigin+".extraheader"]
		if len(hdr) != 2 || hdr[0] != "" || !strings.HasPrefix(hdr[1], "Authorization: Basic ") {
			t.Errorf("git %s: extraheader %q", c.argv, hdr)
		} else {
			matched := false
			for _, tok := range tokens {
				matched = matched || hdr[1] == "Authorization: Basic "+basicAuth(tok)
			}
			if !matched {
				t.Errorf("git %s: header is not a minted token", c.argv)
			}
		}
		if c.env["GIT_TERMINAL_PROMPT"] != "0" {
			t.Errorf("git %s: GIT_TERMINAL_PROMPT=%q", c.argv, c.env["GIT_TERMINAL_PROMPT"])
		}
		if c.cfg["core.hooksPath"] == nil {
			t.Errorf("git %s: hooks not off", c.argv)
		}
		for k := range hostGitCreds {
			if _, ok := c.env[k]; ok {
				t.Errorf("git %s: host %s reached it", c.argv, k)
			}
		}
	}
	if network < 2 {
		t.Fatalf("saw %d network git commands, want clone and push", network)
	}

	// The haystack: every file in the worker's state and config dirs (job
	// meta, spool, stored results, the clone's and worktree's .git), the
	// worker log, every frame and event, and the agent and verify envs.
	var hay []string
	add := func(what string, b []byte) { hay = append(hay, what+"\n"+string(b)) }
	for _, root := range []string{h.paths.StateDir, h.paths.ConfigDir} {
		_ = filepath.Walk(root, func(p string, fi os.FileInfo, err error) error {
			if err == nil && fi.Mode().IsRegular() {
				b, _ := os.ReadFile(p)
				add(p, b)
			}
			return nil
		})
	}
	add("worker log", []byte(h.log.String()))
	h.fs.mu.Lock()
	for _, f := range h.fs.recv {
		add("frame "+f.typ, f.raw)
	}
	h.fs.mu.Unlock()
	evs, _ := json.Marshal(h.fs.jobEvents(id))
	add("events", evs)
	for _, f := range []string{agentEnv, verifyEnv} {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("no env dump %s: %v", filepath.Base(f), err)
		}
		add(f, b)
	}
	if len(hay) < 10 {
		t.Fatalf("only %d places to search", len(hay))
	}
	for _, s := range hay {
		where := strings.SplitN(s, "\n", 2)[0]
		for _, tok := range tokens {
			if strings.Contains(s, tok) || strings.Contains(s, basicAuth(tok)) {
				t.Errorf("token found in %s", where)
			}
		}
		if strings.Contains(s, "ghs_") {
			t.Errorf("ghs_ found in %s", where)
		}
		if strings.HasSuffix(where, "config") && (strings.Contains(s, "extraheader") || strings.Contains(s, "Authorization")) {
			t.Errorf("git config %s carries a header", where)
		}
	}
	if b, err := os.ReadFile(ghLog); err == nil {
		t.Fatalf("gh ran: %s", b)
	}
}

// §14 and acceptance 11 (worker side): a server whose hello_ok lacks
// gittoken.v1 gets bad_spec, and nothing is fetched, minted or started.
func TestDeclineWithoutServerGitToken(t *testing.T) {
	h := newHarness(t, harnessOpts{noStart: true})
	h.fs.mu.Lock()
	h.fs.noGitTokenCap = true
	h.fs.mu.Unlock()
	h.start(Timing{})
	id := "bjob_nocap"
	h.offerJob(fixSpec(id), nil)
	eventually(t, 30*time.Second, "decline", func() bool { return h.fs.declineFor(id) != "" })
	if got := h.fs.declineFor(id); got != workerproto.DeclineBadSpec {
		t.Fatalf("decline %q", got)
	}
	if h.fs.accepted(id) || len(h.fs.mints()) != 0 || h.w.job(id) != nil {
		t.Fatal("the job went further than the decline")
	}
	if !strings.Contains(h.log.String(), workerproto.DetailServerLacksGitToken) {
		t.Fatalf("detail not logged: %s", h.log.String())
	}
}

// §14: a mint the server refuses (409 repo_not_installed) ends the job
// repo_auth: <repo>, with no clone and no agent.
func TestRepoAuthOnRefusedMint(t *testing.T) {
	h := newHarness(t, harnessOpts{})
	h.fs.mu.Lock()
	h.fs.gitTokenFail = http.StatusConflict
	h.fs.mu.Unlock()
	id := "bjob_noinst"
	h.offerJob(fixSpec(id), nil)
	d := h.waitDone(id, 60*time.Second)
	if d.State != "failed" || d.Line != workerproto.ReasonRepoAuth+": app" {
		t.Fatalf("done %+v", d)
	}
	if b, _ := os.ReadFile(h.argv); len(b) != 0 {
		t.Fatalf("the agent ran: %s", b)
	}
}

// A non-GitHub repo is declined bad_spec before anything is fetched.
func TestDeclineNonGitHubRepo(t *testing.T) {
	h := newHarness(t, harnessOpts{policy: strings.Replace(defaultPolicy, `repos = ["https://git.test/acme/*"]`, `repos = ["https://git.test/acme/*", "https://gitlab.test/acme/*"]`, 1)})
	id := "bjob_gitlab"
	spec := fixSpec(id)
	spec.Worktrees[0].RepoURL = "https://gitlab.test/acme/app"
	h.offerJob(spec, nil)
	eventually(t, 30*time.Second, "decline", func() bool { return h.fs.declineFor(id) != "" })
	if got := h.fs.declineFor(id); got != workerproto.DeclineBadSpec || len(h.fs.mints()) != 0 {
		t.Fatalf("decline %q, mints %d", got, len(h.fs.mints()))
	}
}

// The token cache: one mint per repo and access until 5 minutes before
// expires_at, and a malformed token (header injection) is refused.
func TestGitTokenCache(t *testing.T) {
	h := newHarness(t, harnessOpts{})
	id := "bjob_cache"
	h.offerJob(fixSpec(id), nil)
	h.waitDone(id, 60*time.Second)
	j := h.w.job(id)
	canon, _ := CanonicalRepoURL(testRepo)
	before := len(h.fs.mints())
	tok, err := j.gitToken(context.Background(), "app", workerproto.GitAccessRead, canon)
	if err != nil || len(h.fs.mints()) != before {
		t.Fatalf("cached read token not reused: %v, mints %d→%d", err, before, len(h.fs.mints()))
	}
	j.mu.Lock()
	j.gitTokens["app\x00"+workerproto.GitAccessRead] = cachedGitToken{value: tok, expires: time.Now().Add(4 * time.Minute)}
	j.mu.Unlock()
	if _, err := j.gitToken(context.Background(), "app", workerproto.GitAccessRead, canon); err != nil || len(h.fs.mints()) != before+1 {
		t.Fatalf("a token inside the 5-minute margin was reused: %v", err)
	}
	j.mu.Lock()
	j.gitTokens = nil
	j.mu.Unlock()
	if _, err := j.gitToken(context.Background(), "app", workerproto.GitAccessRead, "https://git.test/acme/other"); err == nil {
		t.Fatal("a token for another repo url was accepted")
	}
	for _, bad := range []string{"ghs_a\r\nX-Evil: 1", "ghs_a b", "short"} {
		if gitTokenRe.MatchString(bad) {
			t.Fatalf("token %q accepted", bad)
		}
	}
}

// The token header really authenticates: real git, real HTTPS (git
// http-backend under net/http/cgi), a server that wants exactly
// "Basic base64(x-access-token:<token>)". The right token clones and
// pushes; a wrong one fails as an auth failure, never prompting.
func TestTokenGitAgainstHTTPBackend(t *testing.T) {
	execPath, err := exec.Command("git", "--exec-path").Output()
	if err != nil {
		t.Skip("no git exec path")
	}
	backend := filepath.Join(strings.TrimSpace(string(execPath)), "git-http-backend")
	if _, err := os.Stat(backend); err != nil {
		t.Skip("no git-http-backend")
	}
	const good = "ghs_goodTokenValue123"
	tmp := t.TempDir()
	t.Setenv("GIT_CONFIG_GLOBAL", "/dev/null")
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GIT_AUTHOR_NAME", "dd test")
	t.Setenv("GIT_AUTHOR_EMAIL", "dd@test.invalid")
	t.Setenv("GIT_COMMITTER_NAME", "dd test")
	t.Setenv("GIT_COMMITTER_EMAIL", "dd@test.invalid")
	root := filepath.Join(tmp, "srv")
	src := filepath.Join(tmp, "src")
	for _, args := range [][]string{
		{"init", "-q", "--bare", "-b", "main", filepath.Join(root, "acme", "app.git")},
		{"init", "-q", "-b", "main", src},
		{"-C", src, "commit", "-q", "--allow-empty", "-m", "init"},
		{"-C", src, "push", "-q", filepath.Join(root, "acme", "app.git"), "main"},
	} {
		if out, err := exec.Command("git", args...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	cgiH := &cgi.Handler{Path: backend, Env: []string{"GIT_PROJECT_ROOT=" + root, "GIT_HTTP_EXPORT_ALL=1", "REMOTE_USER=x-access-token"}}
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Basic "+basicAuth(good) {
			w.Header().Set("WWW-Authenticate", `Basic realm="GitHub"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		cgiH.ServeHTTP(w, r)
	}))
	defer srv.Close()
	ca := filepath.Join(tmp, "ca.pem")
	writeFile(t, ca, string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw})), 0o644)
	t.Setenv("GIT_SSL_CAINFO", ca)
	prev := gitHubOrigin
	gitHubOrigin = srv.URL + "/"
	defer func() { gitHubOrigin = prev }()
	repo := srv.URL + "/acme/app.git"

	// A host credential helper that would answer with the right token:
	// the empty credential.helper must reset it, so a wrong token fails.
	helperCfg := filepath.Join(tmp, "helper.gitconfig")
	writeFile(t, helperCfg, "[credential]\n\thelper = \"!f() { echo username=x-access-token; echo password="+good+"; }; f\"\n", 0o644)
	t.Setenv("GIT_CONFIG_GLOBAL", helperCfg)
	if out, err := exec.Command("git", "clone", "-q", "--", repo, filepath.Join(tmp, "via-helper")).CombinedOutput(); err != nil {
		t.Fatalf("control: the host helper alone should clone: %v %s", err, out)
	}
	if out, err := tokenGit("ghs_wrongTokenValue99", "clone", "-q", "--", repo, filepath.Join(tmp, "bad")).CombinedOutput(); err == nil || !gitAuthFailure(out) {
		t.Fatalf("wrong token (host helper not reset?): err %v, auth failure %v: %s", err, gitAuthFailure(out), out)
	}
	t.Setenv("GIT_CONFIG_GLOBAL", "/dev/null")
	clone := filepath.Join(tmp, "good")
	if out, err := tokenGit(good, "clone", "-q", "--", repo, clone).CombinedOutput(); err != nil {
		t.Fatalf("clone with the token: %v %s", err, out)
	}
	if out, err := exec.Command("git", "-C", clone, "commit", "-q", "--allow-empty", "-m", "fix").CombinedOutput(); err != nil {
		t.Fatalf("commit: %v %s", err, out)
	}
	if out, err := tokenGit(good, "-C", clone, "push", "-q", "origin", "HEAD:refs/heads/dd/x").CombinedOutput(); err != nil {
		t.Fatalf("push with the token: %v %s", err, out)
	}
	// A host insteadOf rewrite to ssh fails instead of using a host key.
	gc := filepath.Join(tmp, "insteadof.gitconfig")
	writeFile(t, gc, "[url \"ssh://git@127.0.0.1/\"]\n\tinsteadOf = "+srv.URL+"/\n", 0o644)
	t.Setenv("GIT_CONFIG_GLOBAL", gc)
	if out, err := tokenGit(good, "clone", "-q", "--", repo, filepath.Join(tmp, "ssh")).CombinedOutput(); err == nil || !strings.Contains(string(out), "not allowed") {
		t.Fatalf("an insteadOf rewrite to ssh was followed: %v %s", err, out)
	}
	t.Setenv("GIT_CONFIG_GLOBAL", "/dev/null")
	cfg, _ := os.ReadFile(filepath.Join(clone, ".git", "config"))
	if strings.Contains(string(cfg), good) || strings.Contains(string(cfg), "extraheader") {
		t.Fatalf(".git/config carries the token:\n%s", cfg)
	}
	if out, err := exec.Command("git", "-C", filepath.Join(root, "acme", "app.git"), "rev-parse", "--verify", "refs/heads/dd/x").CombinedOutput(); err != nil {
		t.Fatalf("push did not land: %s", out)
	}
}

// Ship never finds the repository through the worktree's .git file, which
// the agent can rewrite: the job's branch is still committed and pushed
// from the clone's own record of the worktree.
func TestShipIgnoresRewrittenGitFile(t *testing.T) {
	agent := fixArtifacts(testDefect) + "echo fixed >> ../worktrees/app/README.md\n" +
		"cat '" + claudeFixture(t, "c1-basic") + "'\nexit 0\n"
	h := newHarness(t, harnessOpts{agent: agent})
	id := "bjob_gitfile"
	h.offerJob(fixSpec(id), nil)
	h.waitDone(id, 60*time.Second)
	evil := filepath.Join(h.tmp, "evil.git")
	if out, err := exec.Command("git", "init", "-q", "--bare", evil).CombinedOutput(); err != nil {
		t.Fatalf("%v %s", err, out)
	}
	if out, err := exec.Command("git", "--git-dir="+evil, "config", "http.proxy", "http://127.0.0.1:9").CombinedOutput(); err != nil {
		t.Fatalf("%v %s", err, out)
	}
	j := h.w.job(id)
	j.mu.Lock()
	wt := j.meta.Spec.Worktrees[0].WorktreeAbs
	j.mu.Unlock()
	writeFile(t, filepath.Join(wt, ".git"), "gitdir: "+evil+"\n", 0o644)

	ship := dexec.Control{Kind: dexec.ControlShip, Repos: []dexec.ShipRepo{{Repo: "app", Files: []string{"README.md"}, Message: "Fix it"}}}
	r := h.controlAndWait(id, "rq_gitfile", ship, 1)
	var res workerproto.ShipPushResult
	if !r.OK || json.Unmarshal(r.Result, &res) != nil || len(res.Repos) != 1 || !res.Repos[0].Pushed {
		t.Fatalf("ship %+v %s", r, r.Result)
	}
	out, err := exec.Command("git", "-C", filepath.Join(h.tmp, "origin", "app.git"), "rev-parse", "dd/"+id+"/app").Output()
	if err != nil || strings.TrimSpace(string(out)) != res.Repos[0].HeadSHA {
		t.Fatalf("origin at %s (%v), head_sha %s", out, err, res.Repos[0].HeadSHA)
	}
	if out, _ := exec.Command("git", "--git-dir="+evil, "for-each-ref").Output(); len(out) != 0 {
		t.Fatalf("the agent's git dir was written: %s", out)
	}
}

func TestGitAuthFailure(t *testing.T) {
	for out, want := range map[string]bool{
		"fatal: Authentication failed for 'https://github.com/o/r/'":                                   true,
		"remote: Repository not found.\nfatal: repository 'https://github.com/o/r/' not found":         true,
		"fatal: could not read Username for 'https://github.com': terminal prompts disabled":           true,
		"fatal: unable to access 'https://github.com/o/r/': The requested URL returned error: 403":     true,
		"remote: Write access to repository not granted.":                                              true,
		"error: cannot lock ref 'refs/remotes/origin/main': Permission denied":                         false,
		"fatal: unable to access '…': The requested URL returned error: 403 (API rate limit exceeded)": false,
		"fatal: couldn't find remote ref main":                                                         false,
		"fatal: unable to access 'https://github.com/o/r/': Could not resolve host: github.com":        false,
	} {
		if got := gitAuthFailure([]byte(out)); got != want {
			t.Errorf("gitAuthFailure(%q) = %v", out, got)
		}
	}
}
