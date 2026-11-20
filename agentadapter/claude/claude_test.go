package claude

import (
	"bufio"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	aa "github.com/more-than-code/defect-drainer-worker/agentadapter"
)

type replay struct {
	text     string
	tools    [][]byte
	ends     []aa.End
	maxTurns int
	ignored  int
}

func decodeFixture(t *testing.T, name string) replay {
	t.Helper()
	f, err := os.Open(filepath.Join("testdata", name+".jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var r replay
	d := New(Config{}).NewDecoder()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		for _, ev := range d.Decode(sc.Bytes()) {
			switch ev.Kind {
			case aa.Text:
				r.text += ev.Text
			case aa.Tool:
				r.tools = append(r.tools, ev.Raw)
			case aa.EndEvent:
				r.ends = append(r.ends, ev.End)
			case aa.MaxTurns:
				r.maxTurns++
			case aa.Ignore:
				r.ignored++
			default:
				t.Fatalf("%s: unexpected event %+v", name, ev)
			}
		}
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	if len(r.ends) != 1 {
		t.Fatalf("%s: %d end events, want 1", name, len(r.ends))
	}
	return r
}

func TestDecodeSuccess(t *testing.T) {
	r := decodeFixture(t, "c1-basic")
	e := r.ends[0]
	if e.IsError || e.AuthFailed || r.maxTurns != 0 {
		t.Fatalf("c1 end = %+v maxTurns=%d", e, r.maxTurns)
	}
	if e.SessionID != "504d6a57-e6de-43e7-ab1f-b75a2abc5655" || e.NumTurns != 4 || e.TotalCostUSD != 0.04495095 || e.StopReason != "success" {
		t.Fatalf("c1 end = %+v", e)
	}
	if !strings.Contains(r.text, "Fixed the `add()` function") {
		t.Fatalf("c1 text = %q", r.text)
	}
	if !New(Config{}).ReviewEvidence(r.tools, []string{"/tmp/ddfx/wt"}) {
		t.Fatal("c1 has a completed git diff of the worktree")
	}
	if New(Config{}).ReviewEvidence(r.tools, []string{"/tmp/other"}) {
		t.Fatal("a diff of another path is not evidence")
	}
}

// Criterion 3: the turn cap is error_max_turns with is_error and exit 1; it
// must read as max_turns_reached, not a failure.
func TestDecodeTurnCap(t *testing.T) {
	r := decodeFixture(t, "c2-turncap")
	if r.maxTurns != 1 {
		t.Fatalf("maxTurns = %d, want 1", r.maxTurns)
	}
	e := r.ends[0]
	if e.StopReason != "error_max_turns" || e.AuthFailed || e.SessionID != "023759b8-8496-44f8-9ef8-e6fb4e1e08e4" || e.TotalCostUSD != 0.0124575 {
		t.Fatalf("c2 end = %+v", e)
	}
	// c3 resumed c2's session: same id, its own cost only.
	r3 := decodeFixture(t, "c3-resume")
	if r3.ends[0].SessionID != e.SessionID || r3.ends[0].TotalCostUSD != 0.02500985 || r3.ends[0].IsError {
		t.Fatalf("c3 end = %+v", r3.ends[0])
	}
}

// A failed run can say subtype "success": the decoder keys on is_error.
func TestDecodeAuthFailures(t *testing.T) {
	for _, name := range []string{"c9-badkey", "logged-out"} {
		r := decodeFixture(t, name)
		e := r.ends[0]
		if !e.IsError || !e.AuthFailed || r.maxTurns != 0 || e.StopReason != "error" || e.Message == "" {
			t.Fatalf("%s end = %+v", name, e)
		}
	}
}

func TestDecodeDenials(t *testing.T) {
	r := decodeFixture(t, "c6-scoped2")
	tool, ok := New(Config{}).Denied(r.tools, r.ends[0])
	if !ok || tool != "Write" {
		t.Fatalf("Denied = %q,%v", tool, ok)
	}
	r1 := decodeFixture(t, "c1-basic")
	if _, ok := New(Config{}).Denied(r1.tools, r1.ends[0]); ok {
		t.Fatal("c1 has no denials")
	}
}

func TestDecodeEveryFixture(t *testing.T) {
	ents, _ := filepath.Glob("testdata/*.jsonl")
	if len(ents) < 10 {
		t.Fatalf("fixtures: %v", ents)
	}
	for _, p := range ents {
		name := strings.TrimSuffix(filepath.Base(p), ".jsonl")
		r := decodeFixture(t, name)
		exit, _ := os.ReadFile(filepath.Join("testdata", name+".exit"))
		if strings.TrimSpace(string(exit)) == "exit=1" && !r.ends[0].IsError {
			t.Errorf("%s exited 1 without is_error", name)
		}
	}
}

func TestDecodeNonJSONAndInvalid(t *testing.T) {
	d := New(Config{}).NewDecoder()
	if ev := d.Decode([]byte("hello")); len(ev) != 1 || ev[0].Kind != aa.Raw || ev[0].Text != "hello" {
		t.Fatalf("raw = %+v", ev)
	}
	if ev := d.Decode([]byte(`{"type":`)); len(ev) != 1 || ev[0].Kind != aa.Invalid {
		t.Fatalf("invalid = %+v", ev)
	}
	if ev := d.Decode([]byte("  ")); len(ev) != 0 {
		t.Fatalf("blank = %+v", ev)
	}
}

func task(role aa.Role, mode aa.CredentialMode) aa.Task {
	return aa.Task{
		Role: role, Handoff: "/data/batch-jobs/j1", Worktrees: []string{"/data/wt/a", "/data/wt/b"},
		Prompt: "do it", MaxTurns: 40, StateDir: "/data/agent-state/j1",
		Env:        []string{"PATH=/bin", "ANTHROPIC_API_KEY=inherited", "CLAUDE_CONFIG_DIR=/inherited", "CLAUDE_CODE_OAUTH_TOKEN=tok"},
		Credential: aa.Credential{Mode: mode, Key: "sk-test-key", ConfigDir: "/data/agent-state/j1/claude-config"},
	}
}

func TestImplementerArgvAndEnv(t *testing.T) {
	a := New(Config{Bin: "/usr/bin/claude"})
	tk := task(aa.Implementer, aa.APIKey)
	tk.ResumeID = "023759b8-8496-44f8-9ef8-e6fb4e1e08e4"
	c, err := a.Command(tk)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"-p", "do it", "--output-format", "stream-json", "--verbose", "--permission-mode", "dontAsk",
		"--settings", "/data/agent-state/j1/claude-settings.json", "--setting-sources", "user", "--strict-mcp-config",
		"--add-dir", "/data/wt/a", "--add-dir", "/data/wt/b", "--max-turns", "40", "--resume", "023759b8-8496-44f8-9ef8-e6fb4e1e08e4"}
	if !slices.Equal(c.Args, want) {
		t.Fatalf("argv = %q", c.Args)
	}
	if c.Dir != tk.Handoff {
		t.Fatalf("dir = %q", c.Dir)
	}
	// Criterion 8: no bypass, no --bare, no credential value in argv.
	for _, arg := range c.Args {
		if strings.Contains(arg, "bypassPermissions") || arg == "--bare" || strings.Contains(arg, "sk-test-key") {
			t.Fatalf("forbidden argv element %q", arg)
		}
	}
	wantEnv := []string{"PATH=/bin", "ANTHROPIC_API_KEY=sk-test-key", "CLAUDE_CONFIG_DIR=/data/agent-state/j1/claude-config",
		"CLAUDE_CODE_SUBPROCESS_ENV_SCRUB=1", "CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1", "CI=1", "SKILL_FORGE_AGENT_ROLE=worker",
		// §4.1.11: tool caches under the cwd, off $HOME.
		"npm_config_cache=/data/batch-jobs/j1/.cache/npm", "XDG_CACHE_HOME=/data/batch-jobs/j1/.cache"}
	if !slices.Equal(c.Env, wantEnv) {
		t.Fatalf("env = %q", c.Env)
	}
}

func TestSubscriptionEnv(t *testing.T) {
	a := New(Config{Bin: "/usr/bin/claude"})
	tk := task(aa.Implementer, aa.Subscription)
	tk.Credential.ConfigDir = ""
	c, err := a.Command(tk)
	if err != nil {
		t.Fatal(err)
	}
	if aa.GetEnv(c.Env, "ANTHROPIC_API_KEY") != "" || aa.GetEnv(c.Env, "CLAUDE_CONFIG_DIR") != "" {
		t.Fatalf("subscription env carries a key or inherited config dir: %q", c.Env)
	}
	if aa.GetEnv(c.Env, "CLAUDE_CODE_OAUTH_TOKEN") != "tok" {
		t.Fatalf("setup-token login must reach the claude child: %q", c.Env)
	}
	tk.Credential.ConfigDir = "/owner/claude"
	c, _ = a.Command(tk)
	if aa.GetEnv(c.Env, "CLAUDE_CONFIG_DIR") != "/owner/claude" {
		t.Fatalf("config dir = %q", c.Env)
	}
}

func TestContinueWithoutSessionIsRefused(t *testing.T) {
	tk := task(aa.Implementer, aa.APIKey)
	tk.Continue = true
	if _, err := New(Config{Bin: "/b"}).Command(tk); err == nil {
		t.Fatal("continue without a stored session id must be refused")
	}
	tk.ResumeID = "-rm"
	if _, err := New(Config{Bin: "/b"}).Command(tk); err == nil {
		t.Fatal("a non-uuid resume id must be refused")
	}
	if _, ok := New(Config{}).ResumeID("/h", ""); ok {
		t.Fatal("no stored id is not resumable")
	}
}

func TestAPIKeyModeWithoutKeyIsAuth(t *testing.T) {
	tk := task(aa.Implementer, aa.APIKey)
	tk.Credential.Key = ""
	_, err := New(Config{Bin: "/b"}).Command(tk)
	var ae *aa.AuthError
	if err == nil || !errors.As(err, &ae) {
		t.Fatalf("err = %v", err)
	}
}

func TestReviewerArgvAndSettings(t *testing.T) {
	a := New(Config{Bin: "/usr/bin/claude"})
	tk := task(aa.Reviewer, aa.APIKey)
	tk.ResumeID = "023759b8-8496-44f8-9ef8-e6fb4e1e08e4"
	c, err := a.Command(tk)
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(c.Args, " ")
	if strings.Contains(joined, "--resume") || strings.Contains(joined, "--continue") {
		t.Fatalf("reviewer resumes: %q", c.Args)
	}
	if !strings.Contains(joined, "--disallowedTools Edit,Write,NotebookEdit,Task") || !strings.Contains(joined, "claude-review-settings.json") {
		t.Fatalf("reviewer argv = %q", c.Args)
	}
	body, err := Settings(tk)
	if err != nil {
		t.Fatal(err)
	}
	var rs settings
	if err := json.Unmarshal(body, &rs); err != nil {
		t.Fatal(err)
	}
	for _, a := range rs.Permissions.Allow {
		if strings.HasPrefix(a, "Edit") || a == "Read" {
			t.Fatalf("reviewer settings allow %q: %s", a, body)
		}
	}
}

// Main Dev ruling (H1, H2, H4): fail closed. No bare tool allows, scoped
// Read/Edit, no unsandboxed escape, no missing sandbox, secrets denied to
// Bash and the file tools, own .claude/ dirs not editable.
func TestImplementerSettingsFailClosed(t *testing.T) {
	tk := task(aa.Implementer, aa.APIKey)
	tk.DenyRead = []string{"/home/o/.ssh", "/data/defect-drainer.db", "/data", "/srv/dd/.env"}
	body, err := Settings(tk)
	if err != nil {
		t.Fatal(err)
	}
	var s settings
	if err := json.Unmarshal(body, &s); err != nil {
		t.Fatal(err)
	}
	wantAllow := []string{"Bash", "Read(//data/batch-jobs/j1/**)", "Read(//data/wt/a/**)", "Read(//data/wt/b/**)",
		"Edit(//data/batch-jobs/j1/**)", "Edit(//data/wt/a/**)", "Edit(//data/wt/b/**)"}
	if !slices.Equal(s.Permissions.Allow, wantAllow) {
		t.Fatalf("allow = %q", s.Permissions.Allow)
	}
	for _, want := range []string{"WebFetch", "WebSearch",
		"Edit(//data/batch-jobs/j1/.claude/**)", "Edit(//data/wt/a/.claude/**)",
		"Read(//home/o/.ssh)", "Read(//home/o/.ssh/**)", "Read(//data/defect-drainer.db)", "Read(//srv/dd/.env)"} {
		if !slices.Contains(s.Permissions.Deny, want) {
			t.Errorf("deny lacks %q", want)
		}
	}
	// /data holds this job's dirs: it is denied to Bash only, the job's own
	// dirs are re-allowed there, and the file tools stay on the scoped allow.
	if slices.Contains(s.Permissions.Deny, "Read(//data/**)") {
		t.Fatal("a Read deny on an ancestor of the handoff would block the job's own dirs")
	}
	if !slices.Equal(s.Sandbox.Filesystem.DenyRead, []string{"//home/o/.ssh", "//data/defect-drainer.db", "//data", "//srv/dd/.env"}) {
		t.Fatalf("denyRead = %q", s.Sandbox.Filesystem.DenyRead)
	}
	if !slices.Equal(s.Sandbox.Filesystem.AllowRead, []string{"//data/batch-jobs/j1", "//data/wt/a", "//data/wt/b"}) {
		t.Fatalf("allowRead = %q", s.Sandbox.Filesystem.AllowRead)
	}
	if !s.Sandbox.Enabled || !s.Sandbox.AutoAllowBashIfSandboxed || s.Sandbox.AllowUnsandboxedCommands || !s.Sandbox.FailIfUnavailable {
		t.Fatalf("sandbox = %+v", s.Sandbox)
	}
	if !strings.Contains(string(body), `"allowUnsandboxedCommands": false`) {
		t.Fatalf("allowUnsandboxedCommands must be written explicitly: %s", body)
	}
	for _, a := range s.Permissions.Allow {
		if a == "Read" || a == "Glob" || a == "Grep" || a == "Edit" {
			t.Fatalf("bare allow %q", a)
		}
	}
	// No command may be excluded from the sandbox: the "Bash" allow is only
	// safe because every Bash command runs sandboxed.
	if strings.Contains(string(body), "excludedCommands") {
		t.Fatalf("settings exclude commands from the sandbox: %s", body)
	}
	tk.Worktrees = []string{"relative/wt"}
	if _, err := Settings(tk); err == nil {
		t.Fatal("a relative path must be refused")
	}
	tk = task(aa.Implementer, aa.APIKey)
	tk.DenyRead = []string{"relative"}
	if _, err := Settings(tk); err == nil {
		t.Fatal("a relative deny path must be refused")
	}
}

func TestPrepareWritesSettingsOutsideHandoff(t *testing.T) {
	dir := t.TempDir()
	tk := task(aa.Implementer, aa.APIKey)
	tk.StateDir = filepath.Join(dir, "state")
	tk.Credential.ConfigDir = filepath.Join(dir, "state", "claude-config")
	if err := New(Config{}).Prepare(tk); err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(SettingsPath(tk))
	if err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("settings stat = %v %v", st, err)
	}
	if st, err := os.Stat(tk.Credential.ConfigDir); err != nil || !st.IsDir() {
		t.Fatalf("config dir: %v", err)
	}
}

// Phase 2 (2026-11-15): a reviewer runs in handoff/review/<lens> and must read
// the job's BRIEF.md, NOTES.md, fix-notes/ and fix-evidence/ under a DATA
// deny, read-only. Before the fix every lens failed with "tool-denied: Read".
func TestReviewerReadsJobNotesReadOnly(t *testing.T) {
	tk := task(aa.Reviewer, aa.APIKey)
	tk.Handoff = "/data/batch-jobs/j1/review/spec"
	tk.ReadOnly = []string{"/data/batch-jobs/j1/BRIEF.md", "/data/batch-jobs/j1/fix-notes"}
	tk.DenyRead = []string{"/data"}
	body, err := Settings(tk)
	if err != nil {
		t.Fatal(err)
	}
	var s settings
	if err := json.Unmarshal(body, &s); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Read(//data/batch-jobs/j1/BRIEF.md)", "Read(//data/batch-jobs/j1/fix-notes/**)"} {
		if !slices.Contains(s.Permissions.Allow, want) {
			t.Errorf("allow lacks %q: %q", want, s.Permissions.Allow)
		}
	}
	for _, want := range []string{"//data/batch-jobs/j1/BRIEF.md", "//data/batch-jobs/j1/fix-notes"} {
		if !slices.Contains(s.Sandbox.Filesystem.AllowRead, want) {
			t.Errorf("allowRead lacks %q: %q", want, s.Sandbox.Filesystem.AllowRead)
		}
	}
	for _, a := range s.Permissions.Allow {
		if strings.HasPrefix(a, "Edit") {
			t.Fatalf("reviewer may edit: %q", a)
		}
	}
	// The implementer never gets an Edit allow on a read-only path either.
	tk.Role = aa.Implementer
	body, err = Settings(tk)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(body), "Edit(//data/batch-jobs/j1/BRIEF.md") || strings.Contains(string(body), "Edit(//data/batch-jobs/j1/fix-notes") {
		t.Fatalf("read-only path became editable: %s", body)
	}
	tk.ReadOnly = []string{"relative/BRIEF.md"}
	if _, err := Settings(tk); err == nil {
		t.Fatal("a relative read-only path must be refused")
	}
}

// RF-a item 2 (D2): on a worker, the job's clone .git sits under the denied
// {state}/repos. It is re-allowed for reading only, the repos dir stays in
// denyRead, and another clone is neither read-allowed nor re-allowed.
func TestWorkerCloneGitReadOnly(t *testing.T) {
	for _, role := range []aa.Role{aa.Implementer, aa.Reviewer} {
		tk := task(role, aa.APIKey)
		tk.DenyRead = []string{"/state/repos", "/state/jobs"}
		tk.ReadOnly = []string{"/state/repos/aaa/.git", "/state/jobs/j1/evidence"}
		body, err := Settings(tk)
		if err != nil {
			t.Fatal(err)
		}
		var s settings
		if err := json.Unmarshal(body, &s); err != nil {
			t.Fatal(err)
		}
		for _, want := range []string{"Read(//state/repos/aaa/.git/**)", "Read(//state/jobs/j1/evidence/**)"} {
			if !slices.Contains(s.Permissions.Allow, want) {
				t.Errorf("%v: allow lacks %q", role, want)
			}
		}
		if !slices.Contains(s.Sandbox.Filesystem.DenyRead, "//state/repos") || !slices.Contains(s.Sandbox.Filesystem.DenyRead, "//state/jobs") {
			t.Errorf("%v: denyRead = %q", role, s.Sandbox.Filesystem.DenyRead)
		}
		for _, want := range []string{"//state/repos/aaa/.git", "//state/jobs/j1/evidence"} {
			if !slices.Contains(s.Sandbox.Filesystem.AllowRead, want) {
				t.Errorf("%v: allowRead lacks %q", role, want)
			}
		}
		if strings.Contains(string(body), "Edit(//state") || strings.Contains(string(body), "/state/repos/bbb") {
			t.Fatalf("%v: clone editable or another clone allowed: %s", role, body)
		}
	}
}

// RF-a item 4 (D3): the allowed hosts reach the implementer's sandbox only,
// and a task without any writes exactly the file it wrote before.
func TestAllowedDomains(t *testing.T) {
	tk := task(aa.Implementer, aa.APIKey)
	before, err := Settings(tk)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(before), sandboxNetworkKey) {
		t.Fatalf("no domains, yet a network block: %s", before)
	}
	tk.AllowedDomains = []string{"registry.npmjs.org"}
	body, err := Settings(tk)
	if err != nil {
		t.Fatal(err)
	}
	var m struct {
		Sandbox map[string]json.RawMessage `json:"sandbox"`
	}
	if err := json.Unmarshal(body, &m); err != nil {
		t.Fatal(err)
	}
	var net map[string][]string
	if err := json.Unmarshal(m.Sandbox[sandboxNetworkKey], &net); err != nil || !slices.Equal(net[allowedDomainsKey], []string{"registry.npmjs.org"}) {
		t.Fatalf("network = %s (%v)", m.Sandbox[sandboxNetworkKey], err)
	}
	// Everything else is unchanged.
	var a, b settings
	_ = json.Unmarshal(before, &a)
	_ = json.Unmarshal(body, &b)
	if !reflect.DeepEqual(a, b) {
		t.Fatalf("domains changed other settings:\n%s\n%s", before, body)
	}
	tk.Role = aa.Reviewer
	if body, _ := Settings(tk); strings.Contains(string(body), sandboxNetworkKey) {
		t.Fatalf("reviewer got network: %s", body)
	}
}

// §4.1.11 (F11): DenyWrite reaches the sandbox for both roles; none writes
// the same file as before; an entry over the agent's own or read-only
// paths is refused.
func TestDenyWrite(t *testing.T) {
	for _, role := range []aa.Role{aa.Implementer, aa.Reviewer} {
		tk := task(role, aa.APIKey)
		before, err := Settings(tk)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(before), "denyWrite") {
			t.Fatalf("%v: no entries, yet denyWrite: %s", role, before)
		}
		tk.ReadOnly = []string{"/state/repos/aaa/.git"}
		tk.DenyWrite = []string{"/state/status.json", "/state/readiness", "/home/w"}
		body, err := Settings(tk)
		if err != nil {
			t.Fatal(err)
		}
		var s settings
		if err := json.Unmarshal(body, &s); err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(s.Sandbox.Filesystem.DenyWrite, []string{"//state/status.json", "//state/readiness", "//home/w"}) {
			t.Fatalf("%v: denyWrite = %q", role, s.Sandbox.Filesystem.DenyWrite)
		}
		for _, bad := range []string{
			"/data",                 // contains the handoff and the worktrees
			"/data/batch-jobs/j1",   // the handoff itself
			"/data/wt/a",            // a worktree
			"/state/repos",          // contains a read-only path
			"/state/repos/aaa/.git", // a read-only path
			"relative/dir",          // not absolute
		} {
			tk.DenyWrite = []string{"/state/status.json", bad}
			if _, err := Settings(tk); err == nil {
				t.Errorf("%v: deny-write %q accepted", role, bad)
			}
		}
		// A sibling of an own dir is fine.
		tk.DenyWrite = []string{"/data/wt/ab", "/data/batch-jobs/j10"}
		if _, err := Settings(tk); err != nil {
			t.Errorf("%v: sibling refused: %v", role, err)
		}
	}
}

// §4.1.11: both roles get the caches under their own cwd, over inherited
// values.
func TestCacheEnvUnderCwd(t *testing.T) {
	a := New(Config{Bin: "/usr/bin/claude"})
	for _, role := range []aa.Role{aa.Implementer, aa.Reviewer} {
		tk := task(role, aa.APIKey)
		tk.Handoff = "/data/batch-jobs/j1/review/spec"
		tk.Env = append(tk.Env, "npm_config_cache=/home/w/.npm", "NPM_CONFIG_CACHE=/home/w/.npm", "XDG_CACHE_HOME=/home/w/.cache")
		c, err := a.Command(tk)
		if err != nil {
			t.Fatal(err)
		}
		if got := aa.GetEnv(c.Env, "npm_config_cache"); got != "/data/batch-jobs/j1/review/spec/.cache/npm" {
			t.Errorf("%v: npm_config_cache = %q", role, got)
		}
		if got := aa.GetEnv(c.Env, "XDG_CACHE_HOME"); got != "/data/batch-jobs/j1/review/spec/.cache" {
			t.Errorf("%v: XDG_CACHE_HOME = %q", role, got)
		}
		if aa.GetEnv(c.Env, "NPM_CONFIG_CACHE") != "" || strings.Contains(strings.Join(c.Env, "\n"), "/home/w") {
			t.Errorf("%v: an inherited cache survived: %q", role, c.Env)
		}
	}
}
