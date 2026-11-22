package worker

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	dexec "github.com/more-than-code/defect-drainer-worker/exec"
	"github.com/more-than-code/defect-drainer-worker/workerproto"
)

// fakePostgres writes a directory with initdb, pg_ctl and postgres, the last
// answering --version like PostgreSQL 15.
func fakePostgres(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "pgbin")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(dir, "initdb"), "#!/bin/sh\nexit 0\n", 0o755)
	writeFile(t, filepath.Join(dir, "pg_ctl"), "#!/bin/sh\nexit 0\n", 0o755)
	writeFile(t, filepath.Join(dir, "postgres"), "#!/bin/sh\n[ \"$1\" = --version ] && echo 'postgres (PostgreSQL) 15.10 (Debian 15.10-0+deb12u1)'\nexit 0\n", 0o755)
	return dir
}

// S2: agent_postgres, its refusals and the wire field.
func TestPolicyPostgresKey(t *testing.T) {
	dir := t.TempDir()
	pg := fakePostgres(t)
	path := filepath.Join(dir, "policy.toml")
	writeFile(t, path, policyBase+"agent_postgres = \""+pg+"\"\n", 0o644)
	p := LoadPolicy(path, false, rootUID)
	if !p.Valid || p.AgentPostgres != pg || p.Wire().Postgres != pg {
		t.Fatalf("policy %+v", p)
	}
	writeFile(t, path, policyBase, 0o644)
	if p := LoadPolicy(path, false, rootUID); !p.Valid || p.AgentPostgres != "" || p.Wire().Postgres != "" {
		t.Fatalf("without agent_postgres %+v", p)
	}
	// Missing: valid, readiness says missing.
	writeFile(t, path, policyBase+"agent_postgres = \"/usr/lib/no-such/bin\"\n", 0o644)
	if p := LoadPolicy(path, false, rootUID); !p.Valid {
		t.Fatalf("missing dir invalidated the policy: %s", p.Reason)
	}
	groupW := fakePostgres(t)
	if err := os.Chmod(filepath.Join(groupW, "pg_ctl"), 0o775); err != nil {
		t.Fatal(err)
	}
	openDir := fakePostgres(t)
	if err := os.Chmod(openDir, 0o777); err != nil {
		t.Fatal(err)
	}
	for _, v := range []string{"pgbin", "/", pg + "/", groupW, openDir} {
		writeFile(t, path, policyBase+"agent_postgres = \""+v+"\"\n", 0o644)
		if p := LoadPolicy(path, false, rootUID); p.Valid || p.Reason != "bad_agent_postgres" {
			t.Errorf("%q: valid=%v reason %q", v, p.Valid, p.Reason)
		}
	}
	// Not root's: refused.
	writeFile(t, path, policyBase+"agent_postgres = \""+pg+"\"\n", 0o644)
	if p := LoadPolicy(path, false, func(fi os.FileInfo) (uint32, bool) {
		if fi.IsDir() && fi.Name() == "pgbin" {
			return 1000, true
		}
		return 0, true
	}); p.Valid || p.Reason != "bad_agent_postgres" {
		t.Errorf("a dir not root's: valid=%v reason %q", p.Valid, p.Reason)
	}
	if invalid := (Policy{AgentPostgres: pg}).Wire(); invalid.Postgres != "" {
		t.Fatalf("an invalid policy shows postgres %q", invalid.Postgres)
	}
}

// S8: the postgres tool only with agent_postgres, job_stack.v1 always.
func TestPostgresReadiness(t *testing.T) {
	pg := fakePostgres(t)
	h := newHarness(t, harnessOpts{policy: defaultPolicy + "agent_postgres = \"" + pg + "\"\n", noStart: true})
	h.start(Timing{})
	hello := h.fs.frames(workerproto.TypeHello)
	var f workerproto.Hello
	if err := json.Unmarshal(hello[len(hello)-1].raw, &f); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(f.Capabilities, workerproto.CapJobStackV1) || f.Policy.Postgres != pg {
		t.Fatalf("hello caps %v policy %+v", f.Capabilities, f.Policy)
	}
	if err := f.Validate(); err != nil {
		t.Fatalf("hello invalid: %v", err)
	}
	i := slices.IndexFunc(f.Tools, func(t workerproto.ToolStatus) bool { return t.Name == workerproto.ToolPostgres })
	if i < 0 {
		t.Fatalf("no postgres tool: %+v", f.Tools)
	}
	st := f.Tools[i]
	switch runtime.GOOS {
	case "linux":
		if !st.Ready || st.Version != "15.10" {
			t.Fatalf("postgres %+v", st)
		}
		ctx := context.Background()
		if st := checkPostgres(ctx, filepath.Join(pg, "none"), rootUID); st.Ready || st.Reason != workerproto.ReasonMissing {
			t.Fatalf("missing dir %+v", st)
		}
		noCtl := fakePostgres(t)
		_ = os.Remove(filepath.Join(noCtl, "pg_ctl"))
		if st := checkPostgres(ctx, noCtl, rootUID); st.Ready || st.Reason != workerproto.ReasonMissing {
			t.Fatalf("no pg_ctl %+v", st)
		}
		if st := checkPostgres(ctx, pg, func(os.FileInfo) (uint32, bool) { return 1000, true }); st.Ready || st.Reason != workerproto.ReasonUnsafe {
			t.Fatalf("unsafe %+v", st)
		}
		broken := fakePostgres(t)
		writeFile(t, filepath.Join(broken, "postgres"), "#!/bin/sh\nexit 3\n", 0o755)
		if st := checkPostgres(ctx, broken, rootUID); st.Ready || st.Reason != workerproto.ReasonMissing {
			t.Fatalf("broken %+v", st)
		}
	default:
		if st.Ready || st.Reason != workerproto.ReasonUnsupportedOS {
			t.Fatalf("postgres on %s: %+v", runtime.GOOS, st)
		}
	}
	// Without agent_postgres there is no postgres entry (an older server
	// refuses the name).
	h2 := newHarness(t, harnessOpts{noStart: true})
	h2.start(Timing{})
	hello = h2.fs.frames(workerproto.TypeHello)
	if err := json.Unmarshal(hello[len(hello)-1].raw, &f); err != nil {
		t.Fatal(err)
	}
	if slices.ContainsFunc(f.Tools, func(t workerproto.ToolStatus) bool { return t.Name == workerproto.ToolPostgres }) {
		t.Fatalf("postgres tool without agent_postgres: %+v", f.Tools)
	}
}

// stackSpec is fixSpec with a browser, a preview and a two-service stack
// on its one worktree.
func stackSpec(jobID string) dexec.JobSpec {
	s := fixSpec(jobID)
	s.Project.Browser = true
	s.Project.PreviewCmd, s.Project.PreviewURL = "pnpm dev --port 5173", "http://127.0.0.1:5173"
	s.Project.JobStack = []dexec.JobStackService{
		{Name: "api", Repo: "app", Install: "pnpm install --ignore-scripts", Migrate: "pnpm migrate", Seed: []string{"pnpm seed:users"},
			Start: "pnpm start", Port: 3000, Health: "/health", NeedsPostgres: true,
			Env: map[string]string{"DATABASE_URL": "{postgres.url}", "SHARED_SECRET": "{random}", "SELF": "{services.api.url}"}},
		{Name: "worker-2", Repo: "app", Start: "pnpm worker", Port: 3001, Health: "/"},
	}
	s.Project.PreviewEnv = map[string]string{"BACKEND_URL": "{services.api.url}", "BFF_SHARED_SECRET": "{random}"}
	s.Project.PreviewTimeoutSec = 120
	return s
}

// S3, S6: the bad_spec checks of the job stack.
func TestValidateServices(t *testing.T) {
	if err := validateSpec(stackSpec("bjob_ps_ok")); err != nil {
		t.Fatalf("valid stack refused: %v", err)
	}
	ok := []func(*dexec.JobSpec){
		func(s *dexec.JobSpec) { s.Project.JobStack, s.Project.PreviewEnv = nil, nil },
		func(s *dexec.JobSpec) { s.Project.PreviewTimeoutSec = 0 },
		func(s *dexec.JobSpec) { s.Project.PreviewTimeoutSec = 30 },
		func(s *dexec.JobSpec) { s.Project.PreviewTimeoutSec = 300 },
		func(s *dexec.JobSpec) { s.Project.JobStack[0].Env["JSON"] = `{"a":{"b":1}} { spaced } {}` },
		func(s *dexec.JobSpec) { s.Project.JobStack[0].Seed = []string{"a", "b", "c", "d"} },
		func(s *dexec.JobSpec) { s.Project.JobStack[0].Health = "/api/health?deep=1" },
		func(s *dexec.JobSpec) { s.Project.PreviewCmd, s.Project.PreviewURL = "", "" },
	}
	for i, m := range ok {
		s := stackSpec("bjob_ps_ok")
		m(&s)
		if err := validateSpec(s); err != nil {
			t.Errorf("ok %d refused: %v", i, err)
		}
	}
	many := func(n int) map[string]string {
		m := map[string]string{}
		for i := 0; i < n; i++ {
			m["V"+strings.Repeat("X", i)] = "x"
		}
		return m
	}
	bad := []func(*dexec.JobSpec){
		func(s *dexec.JobSpec) { s.Project.PreviewTimeoutSec = 29 },
		func(s *dexec.JobSpec) { s.Project.PreviewTimeoutSec = 301 },
		func(s *dexec.JobSpec) {
			for i := 0; i < 3; i++ {
				c := s.Project.JobStack[1]
				c.Name, c.Port = "x"+string(rune('a'+i)), 4000+i
				s.Project.JobStack = append(s.Project.JobStack, c)
			}
		},
		func(s *dexec.JobSpec) { s.Project.JobStack[1].Name = "api" },
		func(s *dexec.JobSpec) { s.Project.JobStack[1].Name = "Api" },
		func(s *dexec.JobSpec) { s.Project.JobStack[1].Name = "9api" },
		func(s *dexec.JobSpec) { s.Project.JobStack[1].Name = strings.Repeat("a", 33) },
		func(s *dexec.JobSpec) { s.Project.JobStack[1].Repo = "other" },
		func(s *dexec.JobSpec) { s.Project.JobStack[1].Start = "" },
		func(s *dexec.JobSpec) { s.Project.JobStack[0].Migrate = "a\nb" },
		func(s *dexec.JobSpec) { s.Project.JobStack[0].Install = strings.Repeat("x", 501) },
		func(s *dexec.JobSpec) { s.Project.JobStack[0].Seed = []string{"a", "b", "c", "d", "e"} },
		func(s *dexec.JobSpec) { s.Project.JobStack[0].Seed = []string{" "} },
		func(s *dexec.JobSpec) { s.Project.JobStack[1].Port = 1023 },
		func(s *dexec.JobSpec) { s.Project.JobStack[1].Port = 65536 },
		func(s *dexec.JobSpec) { s.Project.JobStack[1].Port = 3000 },
		func(s *dexec.JobSpec) { s.Project.JobStack[1].Port = 5173 },
		func(s *dexec.JobSpec) { s.Project.JobStack[1].Health = "health" },
		func(s *dexec.JobSpec) { s.Project.JobStack[1].Health = "/a b" },
		func(s *dexec.JobSpec) { s.Project.JobStack[0].Env = many(33) },
		func(s *dexec.JobSpec) { s.Project.JobStack[0].Env["1BAD"] = "x" },
		func(s *dexec.JobSpec) { s.Project.JobStack[0].Env["X"] = "a\nb" },
		func(s *dexec.JobSpec) { s.Project.JobStack[0].Env["X"] = strings.Repeat("x", 1025) },
		func(s *dexec.JobSpec) { s.Project.JobStack[0].Env["X"] = "{secret:API_KEY}" },
		func(s *dexec.JobSpec) { s.Project.JobStack[0].Env["X"] = "{services.nope.url}" },
		func(s *dexec.JobSpec) { s.Project.JobStack[0].Env["X"] = "{services.api.port}" },
		func(s *dexec.JobSpec) { s.Project.JobStack[0].Env["X"] = "{env.HOME}" },
		func(s *dexec.JobSpec) { s.Project.PreviewEnv["X"] = "{radnom}" },
		func(s *dexec.JobSpec) { s.Project.PreviewEnv["X"] = "{Random}" },
		func(s *dexec.JobSpec) {
			s.Project.JobStack[0].NeedsPostgres = false
		},
		func(s *dexec.JobSpec) { s.Project.JobStack = nil },
	}
	for i, m := range bad {
		s := stackSpec("bjob_ps_bad")
		m(&s)
		if err := validateSpec(s); err == nil {
			t.Errorf("bad %d accepted", i)
		}
	}
}

// setPostgresTool makes the harness's worker see the postgres tool ready or
// missing.
func setPostgresTool(h *harness, ready bool) {
	st := workerproto.ToolStatus{Name: workerproto.ToolPostgres, Version: "15.10", Ready: true}
	if !ready {
		st = workerproto.ToolStatus{Name: workerproto.ToolPostgres, Reason: workerproto.ReasonMissing}
	}
	h.w.mu.Lock()
	h.w.tools = slices.DeleteFunc(h.w.tools, func(t workerproto.ToolStatus) bool { return t.Name == workerproto.ToolPostgres })
	h.w.tools = append(h.w.tools, st)
	h.w.mu.Unlock()
}

// D2, S8: services only on an effective browser job; Postgres needs
// agent_postgres and a ready tool, or the job is declined.
func TestApplyServices(t *testing.T) {
	browser := fakeBrowser(t, t.TempDir())
	pg := fakePostgres(t)
	withBrowser := defaultPolicy + "agent_browser = \"" + browser + "\"\n"

	// No browser: services cleared with a note, no decline.
	h := newHarness(t, harnessOpts{policy: defaultPolicy + "agent_postgres = \"" + pg + "\"\n"})
	d := h.w.applyPolicy(stackSpec("bjob_ps_nob"), "", "", "")
	if d.reason != "" || d.spec.Project.StackOn() || d.spec.Project.PreviewEnv != nil || d.spec.Project.PreviewTimeoutSec != 0 || d.postgres != "" ||
		!slices.Contains(d.notes, "job stack off: browser jobs only") {
		t.Fatalf("no browser: %s %+v notes %v", d.reason, d.spec.Project, d.notes)
	}

	cases := []struct {
		name, policy, detail string
		ready                bool
	}{
		{"no agent_postgres", withBrowser, workerproto.DetailNoPostgres, true},
		{"not ready", withBrowser + "agent_postgres = \"" + pg + "\"\n", "postgres not ready: missing", false},
		{"ready", withBrowser + "agent_postgres = \"" + pg + "\"\n", "", true},
	}
	for i, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := newHarness(t, harnessOpts{policy: c.policy})
			root, _ := scanRoot(t)
			readyBrowser(h, root, true)
			setPostgresTool(h, c.ready)
			d := h.w.applyPolicy(stackSpec("bjob_ps_pg"+string(rune('a'+i))), "", "", "")
			if c.detail != "" {
				if d.reason != workerproto.DeclinePolicyDenied || d.detail != c.detail {
					t.Fatalf("decision %q %q, want policy_denied %q", d.reason, d.detail, c.detail)
				}
				return
			}
			if d.reason != "" || !d.spec.Project.StackOn() || d.postgres != pg {
				t.Fatalf("decision %q %q postgres %q", d.reason, d.detail, d.postgres)
			}
		})
	}

	// A stack without Postgres needs no agent_postgres.
	h = newHarness(t, harnessOpts{policy: withBrowser})
	root, _ := scanRoot(t)
	readyBrowser(h, root, true)
	s := stackSpec("bjob_ps_nopg")
	s.Project.JobStack = s.Project.JobStack[1:]
	s.Project.PreviewEnv = nil
	if d := h.w.applyPolicy(s, "", "", ""); d.reason != "" || !d.spec.Project.StackOn() || d.postgres != "" {
		t.Fatalf("stack without Postgres: %q %q", d.reason, d.detail)
	}
}

// S4, S9, AC3 through the real worker loop: a stack job gets dd-stack (with
// the policy's Postgres and the job's worktree), dd-shot's stack variant and
// the Stack brief; the {random} value the agent copies into its notes and
// output reaches no event, blob or agent.log.
func TestStackJobEndToEnd(t *testing.T) {
	browser := fakeBrowser(t, t.TempDir())
	pg := fakePostgres(t)
	leak := "r=$(sed -n \"s/.*export SHARED_SECRET='\\([0-9a-f]*\\)'.*/\\1/p\" dd-stack)\n" +
		"echo \"leaked $r\" >&2\n" +
		fixArtifacts(testDefect) +
		"printf 'fixed it, secret %s' \"$r\" > fix-notes/" + testDefect + ".md\n" +
		"printf 'plan %s' \"$r\" > NOTES.md\n" +
		"cat '" + claudeFixture(t, "c1-basic") + "'\nexit 0\n"
	h := newHarness(t, harnessOpts{agent: leak,
		policy: defaultPolicy + "agent_browser = \"" + browser + "\"\nagent_postgres = \"" + pg + "\"\n"})
	root, _ := scanRoot(t)
	readyBrowser(h, root, true)
	setPostgresTool(h, true)
	spec := stackSpec("bjob_ps_e2e")
	h.offerJob(spec, nil)
	h.waitDone(spec.JobID, 60*time.Second)

	handoff := h.paths.Handoff(spec.JobID)
	stack, err := os.ReadFile(filepath.Join(handoff, dexec.DDStackName))
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`export SHARED_SECRET='([0-9a-f]{64})'`).FindSubmatch(stack)
	if m == nil {
		t.Fatalf("dd-stack has no {random} value:\n%s", stack)
	}
	random := string(m[1])
	for _, want := range []string{"dd_pg_bin='" + pg + "'", "dd_timeout=120", "export BFF_SHARED_SECRET='" + random + "'",
		`export DATABASE_URL=''"$dd_pg_url"''`, "s_port=3001", "dd_step seed-1 'pnpm seed:users'"} {
		if !strings.Contains(string(stack), want) {
			t.Fatalf("dd-stack lacks %q:\n%s", want, stack)
		}
	}
	shot, _ := os.ReadFile(filepath.Join(handoff, dexec.DDShotName))
	if !strings.Contains(string(shot), "dd_wait=120 # dd-stack") {
		t.Fatal("dd-shot is not the stack variant")
	}
	brief, _ := os.ReadFile(filepath.Join(handoff, "BRIEF.md"))
	for _, want := range []string{"## Stack", "- `api` (repo `app`): install, migrate, 1 seed(s), start, then serves http://127.0.0.1:3000, health `/health`;",
		"sh dd-stack run -- curl -s 'http://127.0.0.1:3000/health'", "waits up to 120 s"} {
		if !strings.Contains(string(brief), want) {
			t.Fatalf("BRIEF.md lacks %q:\n%s", want, brief)
		}
	}
	if strings.Contains(string(brief), random) {
		t.Fatal("BRIEF.md shows the {random} value")
	}
	h.fs.mu.Lock()
	j := h.fs.jobs[spec.JobID]
	raw, _ := json.Marshal(j.events)
	var blobs []byte
	for _, b := range j.blobs {
		blobs = append(blobs, b...)
	}
	h.fs.mu.Unlock()
	if strings.Contains(string(raw), random) || strings.Contains(string(blobs), random) {
		t.Fatal("an event or blob shows the {random} value")
	}
	if !strings.Contains(string(raw), "leaked [redacted]") || !strings.Contains(string(raw)+string(blobs), "secret [redacted]") {
		t.Fatalf("no redacted copy reached the server:\n%s", raw)
	}
	if log, _ := os.ReadFile(filepath.Join(handoff, "agent.log")); strings.Contains(string(log), random) {
		t.Fatal("agent.log shows the {random} value")
	}
}

// AC6: a browser job without services gets no dd-stack and no Stack brief.
func TestNoStackJobUnchanged(t *testing.T) {
	browser := fakeBrowser(t, t.TempDir())
	h := newHarness(t, harnessOpts{agent: fixArtifacts(testDefect) + "cat '" + claudeFixture(t, "c1-basic") + "'\nexit 0\n",
		policy: defaultPolicy + "agent_browser = \"" + browser + "\"\n"})
	root, _ := scanRoot(t)
	readyBrowser(h, root, true)
	spec := fixSpec("bjob_ps_none")
	spec.Project.Browser = true
	spec.Project.PreviewCmd, spec.Project.PreviewURL = "pnpm dev --port 5173", "http://127.0.0.1:5173"
	spec.Project.PreviewTimeoutSec = 200
	h.offerJob(spec, nil)
	h.waitDone(spec.JobID, 60*time.Second)
	handoff := h.paths.Handoff(spec.JobID)
	if _, err := os.Stat(filepath.Join(handoff, dexec.DDStackName)); err == nil {
		t.Fatal("dd-stack written without services")
	}
	shot, _ := os.ReadFile(filepath.Join(handoff, dexec.DDShotName))
	brief, _ := os.ReadFile(filepath.Join(handoff, "BRIEF.md"))
	if strings.Contains(string(shot), "dd-stack") || strings.Contains(string(brief), "## Stack") || !strings.Contains(string(brief), "waits up\n  to 90 s") {
		t.Fatalf("no-services job changed:\n%s", brief)
	}
}

const testSignInPassword = "seed-Pass!word1"

// signInSpec is stackSpec with an HTTP Stack sign-in.
func signInSpec(jobID string) dexec.JobSpec {
	s := stackSpec(jobID)
	s.Project.JobStackSignIn = &dexec.StackSignIn{URL: "/login", Email: "seed@example.com", Password: testSignInPassword, Method: dexec.SignInHTTP,
		HTTP:        &dexec.SignInCall{Method: "POST", Path: "/api/login", BodyTemplate: `{"email":"{signin.email}","password":"{signin.password}","api":"{services.api.url}"}`},
		SuccessPath: "/account"}
	s.Project.PreviewEnv["LOGIN_AS"] = "{signin.email}"
	return s
}

// S10: the bad_spec checks of the Stack sign-in.
func TestValidateSignIn(t *testing.T) {
	if err := validateSpec(signInSpec("bjob_si_ok")); err != nil {
		t.Fatalf("valid sign-in refused: %v", err)
	}
	form := func(s *dexec.JobSpec) {
		s.Project.JobStackSignIn.Method, s.Project.JobStackSignIn.HTTP = dexec.SignInForm, nil
		s.Project.JobStackSignIn.Form = &dexec.SignInFields{EmailSelector: "#email", PasswordSelector: "input[type=password]", SubmitSelector: "form button"}
	}
	ok := []func(*dexec.JobSpec){
		form,
		func(s *dexec.JobSpec) { s.Project.JobStackSignIn.SuccessPath = "" },
		func(s *dexec.JobSpec) { s.Project.JobStackSignIn.HTTP.BodyTemplate = "" },
		func(s *dexec.JobSpec) { s.Project.JobStackSignIn.HTTP.Method = "PUT" },
		func(s *dexec.JobSpec) { s.Project.JobStack[0].Env["PW"] = "{signin.password}" },
		// signin_random.v1: exactly "{random}" is generated; other braces
		// are a literal under the 8–256 rule.
		func(s *dexec.JobSpec) { s.Project.JobStackSignIn.Password = "{random}" },
		func(s *dexec.JobSpec) { s.Project.JobStackSignIn.Password = "{random}x" },
		func(s *dexec.JobSpec) { s.Project.JobStackSignIn.Password = "x{random}" },
		func(s *dexec.JobSpec) { s.Project.JobStackSignIn.Password = "{signin.password}" },
	}
	for i, m := range ok {
		s := signInSpec("bjob_si_ok")
		m(&s)
		if err := validateSpec(s); err != nil {
			t.Errorf("ok %d refused: %v", i, err)
		}
	}
	bad := []func(*dexec.JobSpec){
		func(s *dexec.JobSpec) { s.Project.JobStack, s.Project.PreviewEnv = nil, nil },
		func(s *dexec.JobSpec) { s.Project.PreviewCmd, s.Project.PreviewURL = "", "" },
		func(s *dexec.JobSpec) { s.Project.JobStackSignIn.URL = "login" },
		func(s *dexec.JobSpec) { s.Project.JobStackSignIn.SuccessPath = "account" },
		func(s *dexec.JobSpec) { s.Project.JobStackSignIn.Email = "" },
		func(s *dexec.JobSpec) { s.Project.JobStackSignIn.Email = "a\nb@example.com" },
		func(s *dexec.JobSpec) { s.Project.JobStackSignIn.Password = "short" },
		func(s *dexec.JobSpec) { s.Project.JobStackSignIn.Password = "{rand}" },
		func(s *dexec.JobSpec) { s.Project.JobStackSignIn.Password = strings.Repeat("p", 257) },
		func(s *dexec.JobSpec) { s.Project.JobStackSignIn.Password = "pass\nword-long" },
		func(s *dexec.JobSpec) { s.Project.JobStackSignIn.Method = "oauth" },
		func(s *dexec.JobSpec) { s.Project.JobStackSignIn.HTTP = nil },
		func(s *dexec.JobSpec) { s.Project.JobStackSignIn.HTTP.Method = "GET" },
		func(s *dexec.JobSpec) { s.Project.JobStackSignIn.HTTP.Path = "api/login" },
		func(s *dexec.JobSpec) { s.Project.JobStackSignIn.HTTP.BodyTemplate = strings.Repeat("x", 2049) },
		func(s *dexec.JobSpec) { s.Project.JobStackSignIn.HTTP.BodyTemplate = `{"k":"{postgres.url}"}` },
		func(s *dexec.JobSpec) { s.Project.JobStackSignIn.HTTP.BodyTemplate = `{"k":"{secret:KEY}"}` },
		func(s *dexec.JobSpec) {
			form(s)
			s.Project.JobStackSignIn.HTTP = &dexec.SignInCall{Method: "POST", Path: "/x"}
		},
		func(s *dexec.JobSpec) {
			form(s)
			s.Project.JobStackSignIn.Form.SubmitSelector = ""
		},
		func(s *dexec.JobSpec) {
			form(s)
			s.Project.JobStackSignIn.Form.EmailSelector = strings.Repeat("a", 201)
		},
		func(s *dexec.JobSpec) {
			s.Project.JobStackSignIn = nil
			s.Project.PreviewEnv["LOGIN_AS"] = "{signin.email}"
		},
	}
	for i, m := range bad {
		s := signInSpec("bjob_si_bad")
		m(&s)
		if err := validateSpec(s); err == nil {
			t.Errorf("bad %d accepted", i)
		}
	}
}

// S10 through the real worker loop: the sign-in reaches dd-stack (env),
// dd-shot (the dd-worker line) and its sign-in file (the body rendered);
// the brief names the email, never the password. An agent that writes the
// password into the code and its notes: no event or blob shows it, the
// diff check flags it, nothing resolves, the diff view hides it and ship
// refuses.
func TestSignInJobEndToEnd(t *testing.T) {
	browser := fakeBrowser(t, t.TempDir())
	pg := fakePostgres(t)
	agent := "echo 'password " + testSignInPassword + "' >> ../worktrees/app/README.md\n" +
		"echo 'tried " + testSignInPassword + "' >&2\n" + fixArtifacts(testDefect) +
		"printf 'signed in with " + testSignInPassword + "' > fix-notes/" + testDefect + ".md\n" +
		"cat '" + claudeFixture(t, "c1-basic") + "'\nexit 0\n"
	h := newHarness(t, harnessOpts{agent: agent,
		policy: defaultPolicy + "agent_browser = \"" + browser + "\"\nagent_postgres = \"" + pg + "\"\n"})
	root, _ := scanRoot(t)
	readyBrowser(h, root, true)
	setPostgresTool(h, true)
	spec := signInSpec("bjob_si_e2e")
	h.offerJob(spec, nil)
	h.waitDone(spec.JobID, 60*time.Second)
	handoff := h.paths.Handoff(spec.JobID)

	stack, _ := os.ReadFile(filepath.Join(handoff, dexec.DDStackName))
	for _, want := range []string{"export DD_SIGNIN_URL='http://127.0.0.1:5173/login'", "export DD_SIGNIN_EMAIL='seed@example.com'",
		"export DD_SIGNIN_PASSWORD='" + testSignInPassword + "'", "export LOGIN_AS='seed@example.com'"} {
		if !strings.Contains(string(stack), want) {
			t.Fatalf("dd-stack lacks %q", want)
		}
	}
	shot, _ := os.ReadFile(filepath.Join(handoff, dexec.DDShotName))
	if !strings.Contains(string(shot), "dd_worker='"+shotBin()+"' # dd-signin") {
		t.Fatal("dd-shot lacks the dd-worker line")
	}
	fi, err := os.Stat(filepath.Join(handoff, dexec.ShotSignInName))
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("sign-in file: %v %v", err, fi)
	}
	si, err := dexec.ReadShotSignIn(filepath.Join(handoff, dexec.ShotSignInName))
	if err != nil || si.URL != "http://127.0.0.1:5173/login" || si.HTTPURL != "http://127.0.0.1:5173/api/login" || si.SuccessURL != "http://127.0.0.1:5173/account" ||
		si.HTTPBody != `{"email":"seed@example.com","password":"`+testSignInPassword+`","api":"http://127.0.0.1:3000"}` {
		t.Fatalf("sign-in file %+v %v", si, err)
	}
	brief, _ := os.ReadFile(filepath.Join(handoff, "BRIEF.md"))
	if !strings.Contains(string(brief), "test user is `seed@example.com`") || strings.Contains(string(brief), testSignInPassword) {
		t.Fatalf("BRIEF.md:\n%s", brief)
	}

	h.fs.mu.Lock()
	j := h.fs.jobs[spec.JobID]
	raw, _ := json.Marshal(j.events)
	var blobs []byte
	for _, b := range j.blobs {
		blobs = append(blobs, b...)
	}
	h.fs.mu.Unlock()
	if strings.Contains(string(raw)+string(blobs), testSignInPassword) {
		t.Fatal("an event or blob shows the password")
	}
	if !strings.Contains(string(raw), "tried [redacted]") || !strings.Contains(string(raw), `"stackSecret":true`) ||
		!strings.Contains(string(raw), "the diff holds a job stack secret") {
		t.Fatalf("events:\n%s", raw)
	}
	for _, e := range h.fs.jobEvents(spec.JobID) {
		if e.Harvest != nil && e.Harvest.AllowResolve {
			t.Fatal("a run whose diff holds the password may resolve")
		}
	}
	if log, _ := os.ReadFile(filepath.Join(handoff, "agent.log")); strings.Contains(string(log), testSignInPassword) {
		t.Fatal("agent.log shows the password")
	}

	r := h.controlAndWait(spec.JobID, "rq_si_diff", dexec.Control{Kind: dexec.ControlDiff, Repo: "app"}, 1)
	var diff workerproto.DiffResult
	if !r.OK || json.Unmarshal(r.Result, &diff) != nil || strings.Contains(diff.Diff, testSignInPassword) || !strings.Contains(diff.Diff, "password [redacted]") {
		t.Fatalf("diff %+v %q", r, diff.Diff)
	}
	ship := dexec.Control{Kind: dexec.ControlShip, Repos: []dexec.ShipRepo{{Repo: "app", Files: []string{"README.md"}, Message: "Fix", Title: "Fix"}}}
	if r = h.controlAndWait(spec.JobID, "rq_si_ship", ship, 1); r.OK || r.Code != workerproto.ReasonPolicyDenied || !strings.Contains(r.Detail, "Stack sign-in password") {
		t.Fatalf("ship %+v", r)
	}
}

// signin_random.v1 through the real worker loop: a password of exactly
// "{random}" is the run's {random} value in dd-stack's env and dd-shot's
// sign-in. An agent that copies it into the code and its notes: no event,
// blob or agent.log shows it, the diff check flags it, and the diff view
// hides it and ship refuses, also after a worker restart.
func TestGeneratedSignInPasswordEndToEnd(t *testing.T) {
	browser := fakeBrowser(t, t.TempDir())
	pg := fakePostgres(t)
	agent := "pw=$(sed -n \"s/^[[:space:]]*export DD_SIGNIN_PASSWORD='\\(.*\\)'$/\\1/p\" " + dexec.DDStackName + " | head -n 1)\n" +
		"echo \"password $pw\" >> ../worktrees/app/README.md\n" +
		"echo \"tried $pw\" >&2\n" + fixArtifacts(testDefect) +
		"printf 'signed in with %s' \"$pw\" > fix-notes/" + testDefect + ".md\n" +
		"cat '" + claudeFixture(t, "c1-basic") + "'\nexit 0\n"
	h := newHarness(t, harnessOpts{agent: agent,
		policy: defaultPolicy + "agent_browser = \"" + browser + "\"\nagent_postgres = \"" + pg + "\"\n"})
	root, _ := scanRoot(t)
	readyBrowser(h, root, true)
	setPostgresTool(h, true)
	spec := signInSpec("bjob_si_gen")
	spec.Project.JobStackSignIn.Password = dexec.PlaceholderRandom
	spec.Project.JobStack[0].Env["SEED_USER_PASSWORD"] = "{signin.password}"
	h.offerJob(spec, nil)
	h.waitDone(spec.JobID, 60*time.Second)
	handoff := h.paths.Handoff(spec.JobID)

	recorded, err := readStackSecrets(h.paths.JobStackSecrets(spec.JobID))
	if err != nil || len(recorded) != 1 || len(recorded[0]) != 64 {
		t.Fatalf("recorded %q %v", recorded, err)
	}
	pw := recorded[0]
	if fi, err := os.Stat(h.paths.JobStackSecrets(spec.JobID)); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("stack-secrets.json: %v %v", err, fi)
	}
	stack, _ := os.ReadFile(filepath.Join(handoff, dexec.DDStackName))
	for _, want := range []string{"export DD_SIGNIN_PASSWORD='" + pw + "'", "export SEED_USER_PASSWORD='" + pw + "'"} {
		if !strings.Contains(string(stack), want) {
			t.Fatalf("dd-stack lacks %q", want)
		}
	}
	si, err := dexec.ReadShotSignIn(filepath.Join(handoff, dexec.ShotSignInName))
	if err != nil || si.Password != pw || !strings.Contains(si.HTTPBody, `"password":"`+pw+`"`) {
		t.Fatalf("sign-in file %+v %v", si, err)
	}
	if readme, _ := os.ReadFile(filepath.Join(h.paths.WorktreeRoot(spec.JobID), "app", "README.md")); !strings.Contains(string(readme), "password "+pw) {
		t.Fatal("the agent did not copy the value: the test proves nothing")
	}

	h.fs.mu.Lock()
	j := h.fs.jobs[spec.JobID]
	raw, _ := json.Marshal(j.events)
	var blobs []byte
	for _, b := range j.blobs {
		blobs = append(blobs, b...)
	}
	h.fs.mu.Unlock()
	if strings.Contains(string(raw)+string(blobs), pw) {
		t.Fatal("an event or blob shows the generated password")
	}
	if !strings.Contains(string(raw), "tried [redacted]") || !strings.Contains(string(raw), `"stackSecret":true`) {
		t.Fatalf("events:\n%s", raw)
	}
	if log, _ := os.ReadFile(filepath.Join(handoff, "agent.log")); strings.Contains(string(log), pw) {
		t.Fatal("agent.log shows the generated password")
	}

	h.stop()
	h.start(Timing{})
	r := h.controlAndWait(spec.JobID, "rq_sig_diff", dexec.Control{Kind: dexec.ControlDiff, Repo: "app"}, 1)
	var diff workerproto.DiffResult
	if !r.OK || json.Unmarshal(r.Result, &diff) != nil || strings.Contains(diff.Diff, pw) || !strings.Contains(diff.Diff, "password [redacted]") {
		t.Fatalf("diff %+v %q", r, diff.Diff)
	}
	ship := dexec.Control{Kind: dexec.ControlShip, Repos: []dexec.ShipRepo{{Repo: "app", Files: []string{"README.md"}, Message: "Fix", Title: "Fix"}}}
	if r = h.controlAndWait(spec.JobID, "rq_sig_ship", ship, 1); r.OK || r.Code != workerproto.ReasonPolicyDenied || !strings.Contains(r.Detail, "Stack sign-in password") {
		t.Fatalf("ship %+v", r)
	}
}
