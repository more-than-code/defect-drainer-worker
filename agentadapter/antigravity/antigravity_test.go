package antigravity

import (
	"bufio"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	aa "github.com/more-than-code/defect-drainer-worker/agentadapter"
)

type replay struct {
	text  string
	tools [][]byte
	end   aa.End
	ends  int
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
				r.end = ev.End
				r.ends++
			case aa.Ignore:
			default:
				t.Fatalf("%s: unexpected event %+v", name, ev)
			}
		}
	}
	if r.ends != 1 {
		t.Fatalf("%s: %d end events", name, r.ends)
	}
	return r
}

func TestDecodeFixFixture(t *testing.T) {
	r := decodeFixture(t, "p3-global")
	e := r.end
	if e.IsError || e.SessionID != "7c316356-0665-4ee6-b015-8bd5a6261ab4" || e.NumTurns != 1 || e.Tokens == 0 || e.TotalCostUSD != 0 {
		t.Fatalf("end = %+v", e)
	}
	// No delta streamed: the prose comes from result.response.
	if !strings.Contains(r.text, "returns `a + b`") {
		t.Fatalf("text = %q", r.text)
	}
	a := New(Config{})
	if !a.ReviewEvidence(r.tools, []string{"/tmp/ddfx/wt"}) {
		t.Fatal("p3 ran git diff of the worktree to DONE")
	}
	if _, denied := a.Denied(r.tools, e); denied {
		t.Fatal("p3 has no denials")
	}
}

// A denied shell command still ends SUCCESS, exit 0: the denial is the signal.
func TestDecodeDeniedCommand(t *testing.T) {
	r := decodeFixture(t, "p1-fix")
	if r.end.IsError {
		t.Fatalf("end = %+v", r.end)
	}
	tool, ok := New(Config{}).Denied(r.tools, r.end)
	if !ok || tool != "command" {
		t.Fatalf("Denied = %q %v", tool, ok)
	}
	if New(Config{}).ReviewEvidence(r.tools, []string{"/tmp/ddfx/wt"}) {
		t.Fatal("an ERROR git diff step is not evidence")
	}
}

// A deny rule refuses read_file without a denied_actions entry.
func TestDecodeDenyRule(t *testing.T) {
	r := decodeFixture(t, "p6-deny")
	tool, ok := New(Config{}).Denied(r.tools, r.end)
	if !ok || tool != "read_file" {
		t.Fatalf("Denied = %q %v", tool, ok)
	}
	// Prose streamed as deltas: response is not appended a second time.
	if strings.Count(r.text, "Refused") != 1 {
		t.Fatalf("text = %q", r.text)
	}
}

func TestDecodeResumeAndSignedOut(t *testing.T) {
	r := decodeFixture(t, "p4-resume")
	if r.end.SessionID != "7c316356-0665-4ee6-b015-8bd5a6261ab4" || r.end.NumTurns != 2 {
		t.Fatalf("resume end = %+v", r.end)
	}
	out := decodeFixture(t, "logged-out")
	if !out.end.IsError || !out.end.AuthFailed || out.end.StopReason != "error" {
		t.Fatalf("logged-out end = %+v", out.end)
	}
}

func TestDecodeEveryFixture(t *testing.T) {
	ents, _ := filepath.Glob("testdata/*.jsonl")
	if len(ents) < 8 {
		t.Fatalf("fixtures: %v", ents)
	}
	for _, p := range ents {
		decodeFixture(t, strings.TrimSuffix(filepath.Base(p), ".jsonl"))
	}
}

func task(role aa.Role) aa.Task {
	return aa.Task{
		Role: role, Handoff: "/data/batch-jobs/j1", Worktrees: []string{"/data/wt/a"},
		Prompt: "fix", Timeout: 45 * time.Minute, StateDir: "/data/agent-state/j1",
		Env:           []string{"PATH=/bin", "HOME=/home/owner", "GEMINI_API_KEY=k"},
		AllowCommands: []string{"pnpm test", "flutter analyze lib", "pnpm lint", "x; rm -rf /", ""},
	}
}

func TestImplementerArgvAndEnv(t *testing.T) {
	a := New(Config{Bin: "/opt/agy", Home: "/agyhome", Model: "gemini-4-argon-low"})
	tk := task(aa.Implementer)
	tk.ResumeID = "7c316356-0665-4ee6-b015-8bd5a6261ab4"
	c, err := a.Command(tk)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"-p", "fix", "--output-format", "stream-json", "--sandbox", "--settings", "/data/agent-state/j1/agy-settings.json", "--mode", "accept-edits",
		"--add-dir", "/data/wt/a", "--model", "gemini-4-argon-low", "--print-timeout", "2700s",
		"--conversation", "7c316356-0665-4ee6-b015-8bd5a6261ab4"}
	if !slices.Equal(c.Args, want) {
		t.Fatalf("argv = %q", c.Args)
	}
	if !slices.Equal(c.Env, []string{"PATH=/bin", "HOME=/agyhome", "CI=1", "SKILL_FORGE_AGENT_ROLE=worker"}) {
		t.Fatalf("env = %q", c.Env)
	}
	for _, arg := range c.Args {
		if strings.Contains(arg, "dangerously") {
			t.Fatalf("forbidden flag %q", arg)
		}
	}
	tk.ResumeID, tk.Continue = "", true
	if _, err := a.Command(tk); err == nil {
		t.Fatal("continue without a conversation id must be refused")
	}
}

func TestReviewerArgv(t *testing.T) {
	a := New(Config{Bin: "/opt/agy", Home: "/agyhome"})
	tk := task(aa.Reviewer)
	tk.ResumeID = "7c316356-0665-4ee6-b015-8bd5a6261ab4"
	c, err := a.Command(tk)
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(c.Args, " ")
	if strings.Contains(joined, "accept-edits") || strings.Contains(joined, "--conversation") ||
		!strings.Contains(joined, "--settings /data/agent-state/j1/agy-review-settings.json") || !strings.Contains(joined, "--sandbox") {
		t.Fatalf("reviewer argv = %q", c.Args)
	}
}

func TestPermissions(t *testing.T) {
	allow, deny := Permissions(task(aa.Implementer), "/agyhome")
	if !slices.Equal(allow, []string{"command(git)", "command(flutter)", "command(pnpm)"}) {
		t.Fatalf("allow = %q", allow)
	}
	for _, want := range []string{"read_file(/agyhome/.gemini)", "write_file(/agyhome/.gemini)", "generate_image", "call_mcp_tool", "browser_get_dom"} {
		if !slices.Contains(deny, want) {
			t.Errorf("deny lacks %q", want)
		}
	}
	for _, r := range append(allow, deny...) {
		if strings.Contains(r, "*") {
			t.Errorf("wildcard rule %q", r)
		}
	}
	allow, deny = Permissions(task(aa.Reviewer), "/agyhome")
	if !slices.Equal(allow, []string{"command(git)"}) || !slices.Contains(deny, "write_file(/data/wt/a)") || !slices.Contains(deny, "write_file(/data/batch-jobs/j1)") {
		t.Fatalf("reviewer allow=%q deny=%q", allow, deny)
	}
}

// Rules go in a session --settings file in DD's state dir; the agy home
// (the owner's signed-in config) is never written.
func TestPrepareWritesSessionSettingsOnly(t *testing.T) {
	home := t.TempDir()
	state := filepath.Join(t.TempDir(), "state")
	a := New(Config{Home: home})
	tk := task(aa.Implementer)
	tk.StateDir = state
	if err := a.Prepare(tk); err != nil {
		t.Fatal(err)
	}
	var doc struct {
		AgentMode   *string `json:"agentMode"`
		Permissions struct {
			Allow []string `json:"allow"`
			Deny  []string `json:"deny"`
		} `json:"permissions"`
	}
	body, err := os.ReadFile(SettingsPath(tk))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(body, &doc); err != nil {
		t.Fatal(err)
	}
	if doc.AgentMode != nil || !slices.Contains(doc.Permissions.Deny, "read_file("+filepath.Join(home, ".gemini")+")") {
		t.Fatalf("implementer settings = %s", body)
	}
	if st, _ := os.Stat(SettingsPath(tk)); st.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %v", st.Mode())
	}
	rv := task(aa.Reviewer)
	rv.StateDir = state
	if err := a.Prepare(rv); err != nil {
		t.Fatal(err)
	}
	body, _ = os.ReadFile(SettingsPath(rv))
	if !strings.Contains(string(body), `"agentMode": ""`) {
		t.Fatalf("reviewer settings = %s", body)
	}
	ents, _ := os.ReadDir(home)
	if len(ents) != 0 {
		t.Fatalf("agy home was written: %v", ents)
	}
}

// On macOS the separate HOME has no Keychain: readiness refuses without
// running agy, and the adapter names the host as unsupported.
func TestDarwinIsNotReady(t *testing.T) {
	mark := filepath.Join(t.TempDir(), "ran")
	bin := filepath.Join(t.TempDir(), "agy")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\ntouch '"+mark+"'\necho gemini\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	mac := New(Config{Bin: bin, Home: t.TempDir(), GOOS: "darwin"})
	err := mac.Ready(t.Context(), task(aa.Implementer))
	var ae *aa.AuthError
	if !errors.As(err, &ae) || !strings.Contains(ae.Reason, "Linux") {
		t.Fatalf("darwin Ready = %v", err)
	}
	if _, serr := os.Stat(mark); serr == nil {
		t.Fatal("agy ran on darwin")
	}
	if aa.HostNote(mac) != "unsupported-on-darwin" {
		t.Fatalf("note = %q", aa.HostNote(mac))
	}
	linux := New(Config{Bin: bin, Home: t.TempDir(), GOOS: "linux"})
	ltk := task(aa.Implementer)
	ltk.Handoff = t.TempDir()
	if err := linux.Ready(t.Context(), ltk); err != nil {
		t.Fatalf("linux Ready = %v", err)
	}
	if aa.HostNote(linux) != "" {
		t.Fatal("linux has a host note")
	}
}

func TestFilterStderr(t *testing.T) {
	a := New(Config{})
	raw, _ := os.ReadFile("testdata/logged-out.stderr")
	for _, line := range strings.Split(string(raw), "\n") {
		if out := a.FilterStderr(line); strings.Contains(out, "http") {
			t.Fatalf("URL forwarded: %q", out)
		}
	}
	if out := a.FilterStderr("  https://accounts.example/o?x=1 "); out != "<url removed>" {
		t.Fatalf("url line = %q", out)
	}
	p1, _ := os.ReadFile("testdata/p1-fix.stderr")
	if out := a.FilterStderr(strings.TrimSpace(string(p1))); strings.Contains(out, "dangerously") {
		t.Fatalf("denial hint forwarded: %q", out)
	}
}
