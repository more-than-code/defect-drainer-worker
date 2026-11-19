// Package claude is the Claude Code adapter. Every flag and field here comes
// from the Phase 0 runs on Claude Code 2.1.292 (docs/claude-code-runner.md,
// "Phase 0 findings: Claude Code, API key"); the recorded streams are in
// testdata/.
package claude

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	aa "github.com/more-than-code/defect-drainer-worker/agentadapter"
)

// MinVersion is the CLI the Phase 0 fixtures were recorded with.
const MinVersion = "2.1.292"

// Config is what the caller resolved.
type Config struct {
	// Bin is the resolved claude binary ("" when not installed).
	Bin string
}

// Adapter is Claude Code.
type Adapter struct{ cfg Config }

// New returns the Claude Code adapter.
func New(cfg Config) *Adapter { return &Adapter{cfg: cfg} }

func (a *Adapter) Name() aa.Name           { return aa.Claude }
func (a *Adapter) Bin() string             { return a.cfg.Bin }
func (a *Adapter) MinVersion() string      { return MinVersion }
func (a *Adapter) VersionArgs() []string   { return []string{"--version"} }
func (a *Adapter) LogSource() string       { return "Claude" }
func (a *Adapter) SupportsSimulator() bool { return false }
func (a *Adapter) ResumesByID() bool       { return true }
func (a *Adapter) FilterStderr(line string) string {
	return line
}

// CredentialEnv are Claude Code's own credential and config variables.
func (a *Adapter) CredentialEnv() []string {
	return []string{"ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN", "CLAUDE_CODE_OAUTH_TOKEN", "CLAUDE_CONFIG_DIR"}
}

// sandboxNetworkKey and allowedDomainsKey name the sandbox's network
// allow list in the settings file: {"sandbox": {"network": {"allowedDomains":
// [...]}}}. Unconfirmed against the CLI until probe P1 (remote-job-fixes
// spec §8); change them here and nowhere else.
const (
	sandboxNetworkKey = "network"
	allowedDomainsKey = "allowedDomains"
)

// settings is the per-job --settings file.
type settings struct {
	Permissions struct {
		Allow []string `json:"allow"`
		Deny  []string `json:"deny"`
	} `json:"permissions"`
	Sandbox struct {
		Enabled                  bool `json:"enabled"`
		AutoAllowBashIfSandboxed bool `json:"autoAllowBashIfSandboxed"`
		AllowUnsandboxedCommands bool `json:"allowUnsandboxedCommands"`
		FailIfUnavailable        bool `json:"failIfUnavailable"`
		Filesystem               struct {
			DenyRead  []string `json:"denyRead"`
			AllowRead []string `json:"allowRead"`
		} `json:"filesystem"`
	} `json:"sandbox"`
}

// SettingsPath is where Prepare writes the task's settings file.
func SettingsPath(t aa.Task) string {
	name := "claude-settings.json"
	if t.Role == aa.Reviewer {
		name = "claude-review-settings.json"
	}
	return filepath.Join(t.StateDir, name)
}

// abs is a permission-rule path: a double leading slash is absolute (a
// single slash is relative to the settings file).
func abs(p string) string { return "/" + filepath.Clean(p) }

// Settings is the job settings for t. Fail closed (Main Dev rulings on the R1
// security review and the Phase 0b probe, 2026-11-15):
//   - reads and edits are scoped to the handoff and the worktrees (no bare
//     Read, Glob, Grep or Edit);
//   - "Bash" is allowed, because CLAUDE_CODE_SUBPROCESS_ENV_SCRUB forces the
//     permission mode to default, where autoAllowBashIfSandboxed approves
//     nothing (Phase 0b p5: git diff refused without it). It approves only
//     sandboxed Bash: the sandbox can be neither escaped
//     (allowUnsandboxedCommands false, no excludedCommands) nor missing
//     (failIfUnavailable true), and denyRead bounds what it reads;
//   - every DenyRead path is denied to Bash (sandbox denyRead) and to the
//     file tools (a Read deny), except a path that contains an allowed dir
//     (DD's batch-jobs dir holds this job's handoff): that one is denied to
//     Bash with this job's dirs re-allowed (allowRead), and the file tools
//     are held to the scoped Read allow;
//   - the agent cannot write .claude/ settings into its own dirs;
//   - t.ReadOnly paths are readable like the agent's own dirs (Read allow and
//     the allowRead re-allow) but never editable. A reviewer's cwd is its lens
//     dir, so this is how it reads the job's BRIEF.md and fix-notes (Phase 2).
//     On a worker it is also the job's clone .git and report evidence;
//   - the implementer's sandboxed shell may reach t.AllowedDomains, and
//     nothing else; with none, the file has no network block at all.
//
// The reviewer gets the same file without any Edit allow.
func Settings(t aa.Task) ([]byte, error) {
	dirs := append([]string{t.Handoff}, t.Worktrees...)
	// readable is everything the agent may read: its own dirs plus the
	// read-only paths (never given an Edit allow).
	readable := append(append([]string(nil), dirs...), t.ReadOnly...)
	for _, p := range append(append([]string(nil), readable...), t.DenyRead...) {
		if !filepath.IsAbs(p) {
			return nil, fmt.Errorf("claude: %q is not an absolute path", p)
		}
	}
	var s settings
	s.Permissions.Allow = []string{"Bash"}
	for _, p := range dirs {
		s.Permissions.Allow = append(s.Permissions.Allow, "Read("+abs(p)+"/**)")
	}
	// A read-only path may be a file or a dir; allow both forms.
	for _, p := range t.ReadOnly {
		s.Permissions.Allow = append(s.Permissions.Allow, "Read("+abs(p)+")", "Read("+abs(p)+"/**)")
	}
	if t.Role == aa.Implementer {
		for _, p := range dirs {
			s.Permissions.Allow = append(s.Permissions.Allow, "Edit("+abs(p)+"/**)")
		}
	}
	s.Permissions.Deny = []string{"WebFetch", "WebSearch"}
	for _, p := range dirs {
		s.Permissions.Deny = append(s.Permissions.Deny, "Edit("+abs(filepath.Join(p, ".claude"))+"/**)")
	}
	s.Sandbox.Filesystem.DenyRead = []string{}
	s.Sandbox.Filesystem.AllowRead = []string{}
	reallow := false
	for _, p := range t.DenyRead {
		s.Sandbox.Filesystem.DenyRead = append(s.Sandbox.Filesystem.DenyRead, abs(p))
		if containsAny(p, readable) {
			reallow = true
			continue
		}
		s.Permissions.Deny = append(s.Permissions.Deny, "Read("+abs(p)+")", "Read("+abs(p)+"/**)")
	}
	if reallow {
		for _, p := range readable {
			s.Sandbox.Filesystem.AllowRead = append(s.Sandbox.Filesystem.AllowRead, abs(p))
		}
	}
	s.Sandbox.Enabled = true
	s.Sandbox.AutoAllowBashIfSandboxed = true
	s.Sandbox.AllowUnsandboxedCommands = false
	s.Sandbox.FailIfUnavailable = true
	if t.Role != aa.Implementer || len(t.AllowedDomains) == 0 {
		return json.MarshalIndent(s, "", "  ")
	}
	return withAllowedDomains(s, t.AllowedDomains)
}

// withAllowedDomains marshals s with the sandbox network allow list added
// under the keys probe P1 confirms.
func withAllowedDomains(s settings, domains []string) ([]byte, error) {
	b, err := json.Marshal(s)
	if err != nil {
		return nil, err
	}
	var m map[string]map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, err
	}
	m["sandbox"][sandboxNetworkKey] = map[string][]string{allowedDomainsKey: append([]string(nil), domains...)}
	return json.MarshalIndent(m, "", "  ")
}

// allowedTools is the --allowedTools value: the settings file's allow list,
// so the CLI's permission mode has the same rules on its command line. A
// rule containing a comma would split, so it is refused.
func allowedTools(t aa.Task) (string, error) {
	body, err := Settings(t)
	if err != nil {
		return "", err
	}
	var s settings
	if err := json.Unmarshal(body, &s); err != nil {
		return "", err
	}
	for _, r := range s.Permissions.Allow {
		if strings.Contains(r, ",") {
			return "", fmt.Errorf("claude: allow rule %q contains a comma", r)
		}
	}
	return strings.Join(s.Permissions.Allow, ","), nil
}

// containsAny reports whether dir is one of dirs or an ancestor of one.
func containsAny(dir string, dirs []string) bool {
	d := filepath.Clean(dir)
	for _, p := range dirs {
		p = filepath.Clean(p)
		if p == d || strings.HasPrefix(p, d+string(filepath.Separator)) {
			return true
		}
	}
	return false
}

// Prepare writes the settings file (outside the handoff, so the agent cannot
// widen its own rules) and, in API-key mode, the per-job config dir. It runs
// before every spawn, so the file is always DD's.
func (a *Adapter) Prepare(t aa.Task) error {
	if t.StateDir == "" || !filepath.IsAbs(t.StateDir) {
		return fmt.Errorf("claude: job state dir %q is not an absolute path", t.StateDir)
	}
	if err := os.MkdirAll(t.StateDir, 0o700); err != nil {
		return err
	}
	if t.Credential.Mode == aa.APIKey && t.Credential.ConfigDir != "" {
		if err := os.MkdirAll(t.Credential.ConfigDir, 0o700); err != nil {
			return err
		}
	}
	body, err := Settings(t)
	if err != nil {
		return err
	}
	return os.WriteFile(SettingsPath(t), body, 0o600)
}

// Command builds `claude -p`. stdin stays nil (/dev/null).
func (a *Adapter) Command(t aa.Task) (aa.Command, error) {
	if a.cfg.Bin == "" {
		return aa.Command{}, fmt.Errorf("coding agent binary not found")
	}
	args := []string{
		"-p", t.Prompt,
		"--output-format", "stream-json",
		"--verbose",
		"--permission-mode", "dontAsk",
		"--settings", SettingsPath(t),
		// Only DD-written settings: no project or local sources, which the
		// agent could plant in its own cwd. In API-key mode "user" is DD's
		// empty per-job config dir; in subscription mode it is the owner's.
		"--setting-sources", "user",
		"--strict-mcp-config",
	}
	for _, w := range t.Worktrees {
		args = append(args, "--add-dir", w)
	}
	tools, err := allowedTools(t)
	if err != nil {
		return aa.Command{}, err
	}
	args = append(args, "--allowedTools", tools)
	if t.MaxTurns > 0 {
		args = append(args, "--max-turns", strconv.Itoa(t.MaxTurns))
	}
	if t.Role == aa.Reviewer {
		args = append(args, "--disallowedTools", "Edit,Write,NotebookEdit,Task")
	} else if t.ResumeID != "" {
		if !SessionID(t.ResumeID) {
			return aa.Command{}, fmt.Errorf("claude: invalid session id %q", t.ResumeID)
		}
		args = append(args, "--resume", t.ResumeID)
	} else if t.Continue {
		// --continue picks "the most recent" session, which is not proof of
		// this job's. A continue without the stored id is refused.
		return aa.Command{}, fmt.Errorf("claude: no session id to resume")
	}
	env, err := childEnv(t)
	if err != nil {
		return aa.Command{}, err
	}
	return aa.Command{Bin: a.cfg.Bin, Args: args, Dir: t.Handoff, Env: env}, nil
}

// childEnv adds Claude's variables to the caller's base env. API-key mode:
// the key under its native name, scrubbed from the agent's own tool
// subprocesses, with a dedicated config dir so the owner's settings, hooks,
// plugins and MCP config cannot leak in. Subscription mode: the owner's
// logged-in config dir, and no API key that would override the login.
func childEnv(t aa.Task) ([]string, error) {
	env := dropNames(t.Env, "ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN", "CLAUDE_CONFIG_DIR")
	switch t.Credential.Mode {
	case aa.APIKey:
		if t.Credential.Key == "" {
			return nil, &aa.AuthError{Reason: "no API key configured for claude"}
		}
		if t.Credential.ConfigDir == "" {
			return nil, fmt.Errorf("claude: API-key mode needs a dedicated config dir")
		}
		env = dropNames(env, "CLAUDE_CODE_OAUTH_TOKEN")
		env = append(env, "ANTHROPIC_API_KEY="+t.Credential.Key)
		env = append(env, "CLAUDE_CONFIG_DIR="+t.Credential.ConfigDir)
	default:
		if t.Credential.ConfigDir != "" {
			env = append(env, "CLAUDE_CONFIG_DIR="+t.Credential.ConfigDir)
		}
	}
	// Required: without it the agent's own Bash can print the key. It also
	// forces the permission mode to default, which in -p still denies
	// anything not allowed.
	env = append(env,
		"CLAUDE_CODE_SUBPROCESS_ENV_SCRUB=1",
		"CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1",
		"CI=1",
		"SKILL_FORGE_AGENT_ROLE=worker",
	)
	return env, nil
}

func dropNames(environ []string, names ...string) []string {
	drop := map[string]bool{}
	for _, n := range names {
		drop[n] = true
	}
	out := make([]string, 0, len(environ))
	for _, e := range environ {
		if k, _, ok := strings.Cut(e, "="); ok && drop[k] {
			continue
		}
		out = append(out, e)
	}
	return out
}

var sessionRe = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// SessionID reports a UUID-shaped session id, the only shape passed to
// --resume (a leading "-" would read as a flag).
func SessionID(s string) bool { return sessionRe.MatchString(s) }

// ResumeID is the stored session id from the last result event. No stored
// id means the job cannot be resumed; there is no directory scan.
func (a *Adapter) ResumeID(_, stored string) (string, bool) {
	if SessionID(stored) {
		return stored, true
	}
	return "", false
}

// Ready checks the credential under the task's exact child env. API-key
// mode needs only the key (a bad key fails the run with agent_auth).
// Subscription mode asks the CLI whether it is logged in.
func (a *Adapter) Ready(ctx context.Context, t aa.Task) error {
	env, err := childEnv(t)
	if err != nil {
		return err
	}
	if t.Credential.Mode == aa.APIKey {
		return nil
	}
	if a.cfg.Bin == "" {
		return fmt.Errorf("coding agent binary not found")
	}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, a.cfg.Bin, "auth", "status", "--json")
	cmd.Env = env
	if t.Handoff != "" {
		cmd.Dir = t.Handoff
	}
	out, _ := cmd.Output()
	var st struct {
		LoggedIn bool `json:"loggedIn"`
	}
	if json.Unmarshal(bytes.TrimSpace(out), &st) != nil {
		return &aa.AuthError{Reason: "claude auth status gave no answer"}
	}
	if !st.LoggedIn {
		return &aa.AuthError{Reason: "claude is not logged in for this config dir"}
	}
	return nil
}

// NewDecoder returns a stream-json decoder.
func (a *Adapter) NewDecoder() aa.Decoder { return &Decoder{} }

// Decoder decodes `claude -p --output-format stream-json --verbose`.
type Decoder struct{}

type contentBlock struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// Decode maps one line. assistant text blocks become text; tool_use and
// tool_result blocks become tool events; the result event becomes End. The
// decoder keys on is_error, never on subtype: a failed run can still say
// subtype "success".
func (d *Decoder) Decode(line []byte) []aa.Event {
	t := bytes.TrimSpace(line)
	if len(t) == 0 {
		return nil
	}
	if t[0] != '{' {
		return []aa.Event{{Kind: aa.Raw, Text: string(t)}}
	}
	// message is an object on assistant/user events and a string on a
	// system permission_denied event, so it is decoded on its own.
	var head struct {
		Type    string          `json:"type"`
		Subtype string          `json:"subtype"`
		Message json.RawMessage `json:"message"`
	}
	if err := json.Unmarshal(t, &head); err != nil {
		return []aa.Event{{Kind: aa.Invalid, Raw: t}}
	}
	switch head.Type {
	case "assistant", "user":
		var msg struct {
			Content json.RawMessage `json:"content"`
		}
		if json.Unmarshal(head.Message, &msg) != nil {
			return []aa.Event{{Kind: aa.Ignore}}
		}
		return decodeContent(msg.Content)
	case "result":
		return decodeResult(t)
	case "system":
		if head.Subtype == "permission_denied" {
			// A live denial; the result's permission_denials repeats it.
			return []aa.Event{{Kind: aa.Tool, Raw: append([]byte(nil), t...)}}
		}
		return []aa.Event{{Kind: aa.Ignore}}
	default:
		// system (init, thinking_tokens), stream_event, and whatever a later
		// version adds.
		return []aa.Event{{Kind: aa.Ignore}}
	}
}

func decodeContent(raw json.RawMessage) []aa.Event {
	var blocks []json.RawMessage
	if json.Unmarshal(raw, &blocks) != nil {
		// A user message with string content is the prompt echo.
		return []aa.Event{{Kind: aa.Ignore}}
	}
	var out []aa.Event
	for _, b := range blocks {
		var cb contentBlock
		if json.Unmarshal(b, &cb) != nil {
			continue
		}
		switch cb.Type {
		case "text":
			// Each assistant text block is a whole message; end it with a
			// newline so the log shows one block per line group.
			if cb.Text != "" {
				out = append(out, aa.Event{Kind: aa.Text, Text: cb.Text + "\n"})
			}
		case "tool_use", "tool_result":
			out = append(out, aa.Event{Kind: aa.Tool, Raw: append([]byte(nil), b...)})
		default:
			out = append(out, aa.Event{Kind: aa.Ignore})
		}
	}
	return out
}

type result struct {
	Subtype        string  `json:"subtype"`
	IsError        bool    `json:"is_error"`
	TerminalReason string  `json:"terminal_reason"`
	NumTurns       int     `json:"num_turns"`
	TotalCostUSD   float64 `json:"total_cost_usd"`
	SessionID      string  `json:"session_id"`
	Result         *string `json:"result"`
	APIErrorStatus *int    `json:"api_error_status"`
	Denials        []struct {
		ToolName string `json:"tool_name"`
	} `json:"permission_denials"`
}

func decodeResult(raw []byte) []aa.Event {
	var r result
	if err := json.Unmarshal(raw, &r); err != nil {
		return []aa.Event{{Kind: aa.Invalid, Raw: raw}}
	}
	end := aa.End{
		StopReason:   r.Subtype,
		SessionID:    r.SessionID,
		NumTurns:     r.NumTurns,
		TotalCostUSD: r.TotalCostUSD,
		IsError:      r.IsError,
	}
	for _, d := range r.Denials {
		if d.ToolName != "" {
			end.Denials = append(end.Denials, d.ToolName)
		}
	}
	maxTurns := r.IsError && (r.Subtype == "error_max_turns" || r.TerminalReason == "max_turns")
	if r.IsError && !maxTurns {
		if end.StopReason == "" || end.StopReason == "success" {
			end.StopReason = "error"
		}
		if r.Result != nil {
			end.Message = *r.Result
		}
		end.AuthFailed = authFailure(r)
	}
	out := []aa.Event{{Kind: aa.EndEvent, End: end}}
	if maxTurns {
		out = append(out, aa.Event{Kind: aa.MaxTurns})
	}
	return out
}

// authFailure is a bad or missing API key (HTTP 401/403, "Invalid API key")
// or an expired subscription login ("Failed to authenticate").
func authFailure(r result) bool {
	if r.APIErrorStatus != nil && (*r.APIErrorStatus == 401 || *r.APIErrorStatus == 403) {
		return true
	}
	if r.Result == nil {
		return false
	}
	msg := *r.Result
	for _, s := range []string{"Invalid API key", "Failed to authenticate", "Not logged in", "Please run /login", "authentication_failed"} {
		if strings.Contains(msg, s) {
			return true
		}
	}
	return false
}

// Denied blocks a reviewer lens on any permission denial: the result's
// permission_denials, or a live system permission_denied event (a run killed
// before its result still shows it).
func (a *Adapter) Denied(events [][]byte, end aa.End) (string, bool) {
	if len(end.Denials) > 0 {
		return end.Denials[0], true
	}
	for _, raw := range events {
		var ev struct {
			Type     string `json:"type"`
			Subtype  string `json:"subtype"`
			ToolName string `json:"tool_name"`
		}
		if json.Unmarshal(raw, &ev) == nil && ev.Type == "system" && ev.Subtype == "permission_denied" {
			if ev.ToolName == "" {
				return "unknown", true
			}
			return ev.ToolName, true
		}
	}
	return "", false
}

// ReviewEvidence wants a Bash tool_use whose command is a git diff of a
// worktree, answered by a tool_result that is not an error.
func (a *Adapter) ReviewEvidence(events [][]byte, worktrees []string) bool {
	want := aa.PathSet(worktrees)
	if len(want) == 0 {
		return false
	}
	diffCall := map[string]bool{}
	for _, raw := range events {
		var b struct {
			Type      string `json:"type"`
			ID        string `json:"id"`
			Name      string `json:"name"`
			ToolUseID string `json:"tool_use_id"`
			IsError   bool   `json:"is_error"`
			Input     struct {
				Command string `json:"command"`
			} `json:"input"`
		}
		if json.Unmarshal(raw, &b) != nil {
			continue
		}
		switch b.Type {
		case "tool_use":
			if b.Name == "Bash" && b.ID != "" && aa.IsDiffOf(b.Input.Command, want) {
				diffCall[b.ID] = true
			}
		case "tool_result":
			if diffCall[b.ToolUseID] && !b.IsError {
				return true
			}
		}
	}
	return false
}
