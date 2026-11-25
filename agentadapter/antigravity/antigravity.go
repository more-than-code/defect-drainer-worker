// Package antigravity is the Antigravity CLI (`agy`) adapter. Flags, the
// stream format and the permission rules come from the spike on agy 1.4.2
// (docs/claude-code-runner.md, "Spike findings: Antigravity CLI,
// subscription"); the recorded streams are in testdata/.
package antigravity

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	aa "github.com/more-than-code/defect-drainer-worker/agentadapter"
)

// MinVersion is the first agy release DD verified with session --settings
// (2026-11-14). The stream fixtures in testdata were recorded on 1.4.2; the
// event format did not change in the 1.5.9 probe.
const MinVersion = "1.5.9"

// Config is what the caller resolved.
type Config struct {
	// Bin is the resolved agy binary ("" when not installed).
	Bin string
	// Home is the DD-only HOME the owner signed in under. agy has no
	// config-dir variable: its config, conversations and OAuth token live in
	// $HOME/.gemini/antigravity-cli/, so the child's HOME is the only lever.
	Home string
	// Model is passed as --model when set.
	Model string
	// GOOS is the host OS; "" is runtime.GOOS. Tests set it.
	GOOS string
}

// Adapter is the Antigravity CLI.
type Adapter struct{ cfg Config }

// New returns the Antigravity adapter.
func New(cfg Config) *Adapter { return &Adapter{cfg: cfg} }

func (a *Adapter) Name() aa.Name           { return aa.Antigravity }
func (a *Adapter) Bin() string             { return a.cfg.Bin }
func (a *Adapter) MinVersion() string      { return MinVersion }
func (a *Adapter) VersionArgs() []string   { return []string{"--version"} }
func (a *Adapter) LogSource() string       { return "Antigravity" }
func (a *Adapter) SupportsSimulator() bool { return false }
func (a *Adapter) ResumesByID() bool       { return true }
func (a *Adapter) CredentialEnv() []string { return []string{"GEMINI_API_KEY", "GOOGLE_API_KEY"} }

func (a *Adapter) goos() string {
	if a.cfg.GOOS != "" {
		return a.cfg.GOOS
	}
	return runtime.GOOS
}

// HostUnsupported names why this host cannot run agy jobs. On macOS agy
// 1.5.x keeps its sign-in in the login Keychain, which a separate HOME
// cannot reach: the CLI then shows the owner a Keychain dialog. The adapter
// targets Linux workers (the maintainers, 2026-11-14); on the Mac use claude.
func (a *Adapter) HostUnsupported() string {
	if a.goos() == "darwin" {
		return "unsupported-on-darwin"
	}
	return ""
}

// SettingsPath is the session settings file for t, in DD's per-job state dir
// (beside the handoff, never in it). agy 1.5.x reads it with --settings and
// never saves it, so DD writes nothing into the agy home.
func SettingsPath(t aa.Task) string {
	name := "agy-settings.json"
	if t.Role == aa.Reviewer {
		name = "agy-review-settings.json"
	}
	return filepath.Join(t.StateDir, name)
}

// deniedTools are agy tools a DD job never needs: the browser, image
// generation, MCP, web access, subagents and messaging. The init event of
// agy 1.4.2 lists them. Whether read_file/write_file rules also cover
// view_file, sed_file, write_to_file and friends is unproven; the shell is
// confined by --sandbox. Re-probe on a Linux worker (W3).
var deniedTools = []string{
	"browser_click_element", "browser_drag_pixel_to_pixel", "browser_get_dom", "browser_get_network_request",
	"browser_input", "browser_list_network_requests", "browser_mouse_down", "browser_mouse_up",
	"browser_move_mouse", "browser_press_key", "browser_refresh_page", "browser_resize_window",
	"browser_scroll", "browser_scroll_dom", "browser_select_option", "browser_subagent",
	"capture_browser_console_logs", "capture_browser_screenshot", "click_browser_pixel",
	"execute_browser_javascript", "list_browser_pages", "open_browser_url", "read_browser_page",
	"generate_image", "call_mcp_tool", "list_resources", "read_resource",
	"read_url_content", "search_web", "search_marketplace", "list_plugin_accounts",
	"define_subagent", "invoke_subagent", "manage_subagents", "send_message", "manage_inbox",
	"schedule", "run_workflow", "notebook_execution",
}

// Permissions is the permissions block DD writes for t.
//   - allow: command(git) plus the first word of each verify command;
//     wildcards (command(*)) are refused by the CLI's own guidance.
//   - deny: the agy home's .gemini dir for the file tools (the OAuth token
//     is readable without it: probe 5), every unneeded tool, and for the
//     reviewer writes to the handoff and the worktrees.
func Permissions(t aa.Task, home string) (allow, deny []string) {
	seen := map[string]bool{"git": true}
	allow = []string{"command(git)"}
	if t.Role == aa.Implementer {
		var extra []string
		for _, c := range t.AllowCommands {
			f := strings.Fields(c)
			if len(f) == 0 || seen[f[0]] || !safeCommand.MatchString(f[0]) {
				continue
			}
			seen[f[0]] = true
			extra = append(extra, "command("+f[0]+")")
		}
		sort.Strings(extra)
		allow = append(allow, extra...)
	}
	gem := filepath.Join(home, ".gemini")
	deny = []string{"read_file(" + gem + ")", "write_file(" + gem + ")"}
	if t.Role == aa.Reviewer {
		for _, p := range append([]string{t.Handoff}, t.Worktrees...) {
			deny = append(deny, "write_file("+p+")")
		}
	}
	deny = append(deny, deniedTools...)
	return allow, deny
}

// safeCommand is a binary name or path with no rule syntax in it.
var safeCommand = regexp.MustCompile(`^[A-Za-z0-9_./+-]+$`)

// Settings is the --settings document for t. agy 1.5.x starts headless
// runs with no saved mode in Auto, which approves edits and sandboxed
// commands, so the reviewer pins Default (agentMode ""), where a headless
// edit is auto-denied; the implementer passes --mode accept-edits.
func Settings(t aa.Task, home string) ([]byte, error) {
	allow, deny := Permissions(t, home)
	doc := map[string]any{"permissions": map[string][]string{"allow": allow, "deny": deny}}
	if t.Role == aa.Reviewer {
		doc["agentMode"] = ""
	}
	return json.MarshalIndent(doc, "", "  ")
}

// Prepare writes the session settings file before every spawn, so the file
// is always DD's.
func (a *Adapter) Prepare(t aa.Task) error {
	if t.Role == aa.Chat {
		return aa.ErrChatUnsupported
	}
	if a.cfg.Home == "" || !filepath.IsAbs(a.cfg.Home) {
		return fmt.Errorf("antigravity: agy home %q is not an absolute path", a.cfg.Home)
	}
	if t.StateDir == "" || !filepath.IsAbs(t.StateDir) {
		return fmt.Errorf("antigravity: job state dir %q is not an absolute path", t.StateDir)
	}
	if err := os.MkdirAll(t.StateDir, 0o700); err != nil {
		return err
	}
	body, err := Settings(t, a.cfg.Home)
	if err != nil {
		return err
	}
	return os.WriteFile(SettingsPath(t), body, 0o600)
}

// Command builds `agy -p`. Always --sandbox and --settings; never
// --dangerously-skip-permissions. The implementer runs --mode accept-edits;
// the reviewer runs Default mode (pinned in its settings file), where
// headless edits are auto-denied.
func (a *Adapter) Command(t aa.Task) (aa.Command, error) {
	if a.cfg.Bin == "" {
		return aa.Command{}, fmt.Errorf("coding agent binary not found")
	}
	if a.cfg.Home == "" {
		return aa.Command{}, fmt.Errorf("antigravity: no agy home configured")
	}
	if t.Role == aa.Chat {
		return aa.Command{}, aa.ErrChatUnsupported
	}
	args := []string{"-p", t.Prompt, "--output-format", "stream-json", "--sandbox", "--settings", SettingsPath(t)}
	if t.Role == aa.Implementer {
		args = append(args, "--mode", "accept-edits")
	}
	for _, w := range t.Worktrees {
		args = append(args, "--add-dir", w)
	}
	if a.cfg.Model != "" {
		args = append(args, "--model", a.cfg.Model)
	}
	if t.Timeout > 0 {
		args = append(args, "--print-timeout", strconv.Itoa(int(t.Timeout/time.Second))+"s")
	}
	if t.Role == aa.Implementer {
		if t.ResumeID != "" {
			if !ConversationID(t.ResumeID) {
				return aa.Command{}, fmt.Errorf("antigravity: invalid conversation id %q", t.ResumeID)
			}
			args = append(args, "--conversation", t.ResumeID)
		} else if t.Continue {
			return aa.Command{}, fmt.Errorf("antigravity: no conversation id to resume")
		}
	}
	return aa.Command{Bin: a.cfg.Bin, Args: args, Dir: t.Handoff, Env: a.childEnv(t)}, nil
}

// childEnv: HOME is the DD-only agy home, so the owner's personal ~/.gemini
// (and any API-key mode it is set to) is never read. A Gemini key in serve's
// env would switch agy to API-key billing, so it is dropped.
func (a *Adapter) childEnv(t aa.Task) []string {
	env := make([]string, 0, len(t.Env)+4)
	for _, e := range t.Env {
		k, _, _ := strings.Cut(e, "=")
		if k == "HOME" || k == "GEMINI_API_KEY" || k == "GOOGLE_API_KEY" {
			continue
		}
		env = append(env, e)
	}
	return append(env, "HOME="+a.cfg.Home, "CI=1", "SKILL_FORGE_AGENT_ROLE=worker")
}

var convRe = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// ConversationID reports a UUID-shaped conversation id.
func ConversationID(s string) bool { return convRe.MatchString(s) }

// ResumeID is the conversation id stored from the last result.
func (a *Adapter) ResumeID(_, stored string) (string, bool) {
	if ConversationID(stored) {
		return stored, true
	}
	return "", false
}

// Ready runs `agy models` under the job env. Signed out it prints "Please
// sign in to view available models".
func (a *Adapter) Ready(ctx context.Context, t aa.Task) error {
	if a.cfg.Bin == "" {
		return fmt.Errorf("coding agent binary not found")
	}
	if a.cfg.Home == "" {
		return &aa.AuthError{Reason: "no agy home configured"}
	}
	// Never run agy under the separate HOME on macOS: it would show the
	// owner a Keychain dialog in the middle of a job.
	if a.HostUnsupported() != "" {
		return &aa.AuthError{Reason: "the antigravity adapter targets Linux workers; on macOS agy keeps its sign-in in the Keychain, which the DD-only HOME cannot reach. Use claude on the Mac"}
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, a.cfg.Bin, "models")
	cmd.Env = a.childEnv(t)
	if t.Handoff != "" {
		cmd.Dir = t.Handoff
	}
	out, err := cmd.CombinedOutput()
	if err != nil || bytes.Contains(bytes.ToLower(out), []byte("sign in")) {
		return &aa.AuthError{Reason: "agy is not signed in for this agy home"}
	}
	return nil
}

var urlRe = regexp.MustCompile(`https?://\S+`)

// FilterStderr never forwards agy's stderr raw: signed out it prints a
// sign-in URL and an interactive prompt, and a headless denial suggests
// --dangerously-skip-permissions, which DD never uses.
func (a *Adapter) FilterStderr(line string) string {
	l := strings.TrimSpace(line)
	switch {
	case l == "":
		return ""
	case strings.Contains(l, "dangerously-skip-permissions"):
		return "agent: a tool needed a permission headless mode cannot grant (see the denied actions)"
	case strings.Contains(l, "authorization code") || strings.Contains(l, "Waiting for authentication") ||
		strings.Contains(l, "Please visit the URL"):
		return ""
	}
	return urlRe.ReplaceAllString(l, "<url removed>")
}

// NewDecoder returns a fresh decoder (it remembers whether prose streamed).
func (a *Adapter) NewDecoder() aa.Decoder { return &Decoder{} }

// Decoder decodes `agy --output-format stream-json`: init, step_update and
// result events.
type Decoder struct {
	sawText bool
}

type stepUpdate struct {
	StepIndex int    `json:"step_index"`
	State     string `json:"state"`
	StepType  string `json:"step_type"`
	ToolName  string `json:"tool_name"`
	TextDelta string `json:"text_delta"`
}

type agyResult struct {
	ConversationID string `json:"conversation_id"`
	Status         string `json:"status"`
	Response       string `json:"response"`
	Error          string `json:"error"`
	NumTurns       int    `json:"num_turns"`
	FinishedReason string `json:"finished_reason"`
	Usage          struct {
		TotalTokens int `json:"total_tokens"`
	} `json:"usage"`
	DeniedActions []struct {
		Action string `json:"action"`
	} `json:"denied_actions"`
}

// Decode maps one line. Tool steps become tool events, agent_response
// text_delta becomes prose, and result becomes End. agy reports no cost: End
// carries the token total instead.
func (d *Decoder) Decode(line []byte) []aa.Event {
	t := bytes.TrimSpace(line)
	if len(t) == 0 {
		return nil
	}
	if t[0] != '{' {
		return []aa.Event{{Kind: aa.Raw, Text: string(t)}}
	}
	var head struct {
		Event      string          `json:"event"`
		StepUpdate json.RawMessage `json:"step_update"`
		Result     json.RawMessage `json:"result"`
	}
	if err := json.Unmarshal(t, &head); err != nil {
		return []aa.Event{{Kind: aa.Invalid, Raw: t}}
	}
	switch head.Event {
	case "step_update":
		var su stepUpdate
		if json.Unmarshal(head.StepUpdate, &su) != nil {
			return []aa.Event{{Kind: aa.Invalid, Raw: t}}
		}
		switch {
		case su.StepType == "tool":
			return []aa.Event{{Kind: aa.Tool, Raw: append([]byte(nil), head.StepUpdate...)}}
		case su.StepType == "agent_response" && su.TextDelta != "":
			d.sawText = true
			return []aa.Event{{Kind: aa.Text, Text: su.TextDelta}}
		}
		return []aa.Event{{Kind: aa.Ignore}}
	case "result":
		var r agyResult
		if json.Unmarshal(head.Result, &r) != nil {
			return []aa.Event{{Kind: aa.Invalid, Raw: t}}
		}
		var out []aa.Event
		// The assistant's text is only in result.response when no delta
		// streamed it.
		if !d.sawText && r.Response != "" {
			out = append(out, aa.Event{Kind: aa.Text, Text: r.Response})
		}
		end := aa.End{
			StopReason: strings.ToLower(r.Status),
			SessionID:  r.ConversationID,
			NumTurns:   r.NumTurns,
			Tokens:     r.Usage.TotalTokens,
			IsError:    r.Status != "SUCCESS",
			Message:    r.Error,
		}
		if r.FinishedReason != "" && end.IsError {
			end.StopReason = r.FinishedReason
		}
		for _, da := range r.DeniedActions {
			if da.Action != "" {
				end.Denials = append(end.Denials, da.Action)
			}
		}
		end.AuthFailed = end.IsError && strings.Contains(strings.ToLower(r.Error), "authentication")
		return append(out, aa.Event{Kind: aa.EndEvent, End: end})
	default:
		// init, and whatever a later version adds.
		return []aa.Event{{Kind: aa.Ignore}}
	}
}

type toolStep struct {
	StepIndex int    `json:"step_index"`
	State     string `json:"state"`
	ToolName  string `json:"tool_name"`
	ToolInfo  struct {
		Parameters struct {
			CommandLine string `json:"CommandLine"`
		} `json:"parameters"`
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
	} `json:"tool_info"`
}

// Denied blocks a reviewer lens on result.denied_actions, or on a tool step
// that failed its permission check (a deny rule refuses without listing a
// denied action).
func (a *Adapter) Denied(events [][]byte, end aa.End) (string, bool) {
	if len(end.Denials) > 0 {
		return end.Denials[0], true
	}
	for _, raw := range events {
		var s toolStep
		if json.Unmarshal(raw, &s) != nil || s.State != "ERROR" || s.ToolInfo.Error == nil {
			continue
		}
		if strings.Contains(s.ToolInfo.Error.Message, "permission check failed") {
			return s.ToolName, true
		}
	}
	return "", false
}

// ReviewEvidence wants a run_command step whose command line is a git diff
// of a worktree, finished DONE (not ERROR).
func (a *Adapter) ReviewEvidence(events [][]byte, worktrees []string) bool {
	want := aa.PathSet(worktrees)
	if len(want) == 0 {
		return false
	}
	for _, raw := range events {
		var s toolStep
		if json.Unmarshal(raw, &s) != nil || s.ToolName != "run_command" || s.State != "DONE" {
			continue
		}
		if aa.IsDiffOf(s.ToolInfo.Parameters.CommandLine, want) {
			return true
		}
	}
	return false
}
