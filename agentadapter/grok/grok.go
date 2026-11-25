// Package grok is the Grok Build adapter. Its argv, decoder, session lookup
// and reviewer evidence moved here from internal/git and internal/jobs with
// no behaviour change (criterion 1); the field names and values are the ones
// verified against Grok Build 1.0.40 (2026-09-22).
package grok

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	aa "github.com/more-than-code/defect-drainer-worker/agentadapter"
)

// MinVersion is the version the stream decoder was verified against.
const MinVersion = "1.0.40"

// Config is what the caller resolved from its environment.
type Config struct {
	// Bin is the resolved grok binary ("" when not installed).
	Bin string
	// Home is grok's state dir ($GROK_HOME or ~/.grok), where sessions live.
	Home string
}

// Adapter is Grok Build.
type Adapter struct{ cfg Config }

// New returns the grok adapter.
func New(cfg Config) *Adapter { return &Adapter{cfg: cfg} }

func (a *Adapter) Name() aa.Name           { return aa.Grok }
func (a *Adapter) Bin() string             { return a.cfg.Bin }
func (a *Adapter) MinVersion() string      { return MinVersion }
func (a *Adapter) VersionArgs() []string   { return []string{"--version"} }
func (a *Adapter) LogSource() string       { return "Grok" }
func (a *Adapter) SupportsSimulator() bool { return true }
func (a *Adapter) ResumesByID() bool       { return false }
func (a *Adapter) CredentialEnv() []string { return []string{"XAI_API_KEY", "GROK_API_KEY"} }

// Prepare is a no-op: the job-scoped sandbox profile is written and
// byte-checked by the jobs package, which owns the Simulator lease.
func (a *Adapter) Prepare(aa.Task) error { return nil }

// Ready keeps today's behaviour: grok's login is `grok login` by the owner,
// and a signed-out run fails at run time.
func (a *Adapter) Ready(context.Context, aa.Task) error { return nil }

// FilterStderr forwards grok's stderr unchanged.
func (a *Adapter) FilterStderr(line string) string { return line }

// Command builds the implementer or reviewer argv. The implementer argv is
// identical across iterations except for -c / -r and the prompt, so the
// sandbox and turn budget cannot drift.
func (a *Adapter) Command(t aa.Task) (aa.Command, error) {
	if a.cfg.Bin == "" {
		return aa.Command{}, fmt.Errorf("coding agent binary not found")
	}
	if t.Role == aa.Reviewer {
		return a.reviewer(t), nil
	}
	if t.Role == aa.Chat {
		return aa.Command{}, aa.ErrChatUnsupported
	}
	sandboxArg := t.Sandbox
	if t.SandboxProfile != "" {
		sandboxArg = t.SandboxProfile
	}
	maxTurns := t.MaxTurnsArg
	if maxTurns == "" {
		maxTurns = strconv.Itoa(t.MaxTurns)
	}
	args := []string{}
	if t.ResumeID != "" {
		// Only a UUID-shaped id: anything else is a title search, and a
		// leading "-" would read as a flag.
		if !sessionID.MatchString(t.ResumeID) {
			return aa.Command{}, fmt.Errorf("grok: invalid session id %q", t.ResumeID)
		}
		args = append(args, "-r", t.ResumeID)
	} else if t.Continue {
		args = append(args, "-c")
	}
	args = append(args,
		"-p", t.Prompt,
		"--cwd", t.Handoff,
		"--sandbox", sandboxArg,
		"--always-approve",
		"--max-turns", maxTurns,
		// streaming-json, not plain: plain prints prose at exit and hides
		// stopReason, so the harness cannot tell turn exhaustion from a finish.
		"--output-format", "streaming-json",
		"--verbatim",
	)
	if t.GrokBypassPermissions {
		args = append(args, "--permission-mode", "bypassPermissions")
	}
	if tools := strings.TrimSpace(t.GrokTools); tools != "" {
		args = append(args, "--tools", tools)
	}
	env := append(append([]string(nil), t.Env...),
		"CI=1",
		"GROK_SANDBOX="+sandboxArg,
		"SKILL_FORGE_AGENT_ROLE=worker",
	)
	return aa.Command{Bin: a.cfg.Bin, Args: args, Dir: t.Handoff, Env: env}, nil
}

// reviewer is a fresh process. Do not add -c, -r, --resume, or
// --always-approve. -c resumes the most recent session for this cwd.
// --always-approve would let a prompt injected into the implementer's diff
// read any file and exfiltrate it: read-only still reads everywhere, and the
// child-network block does not hold on macOS. The permission gate stays.
func (a *Adapter) reviewer(t aa.Task) aa.Command {
	args := []string{
		"-p", t.Prompt,
		"--cwd", t.Handoff,
		"--sandbox", "read-only",
		"--no-subagents",
		"--output-format", "streaming-json",
		"--max-turns", strconv.Itoa(t.MaxTurns),
		"--verbatim",
	}
	env := append(append([]string(nil), t.Env...),
		"CI=1",
		"GROK_SANDBOX=read-only",
		"SKILL_FORGE_AGENT_ROLE=worker",
	)
	return aa.Command{Bin: a.cfg.Bin, Args: args, Dir: t.Handoff, Env: env}
}

// ResumeID ignores the stored id: grok resumes what `grok -c` would continue,
// the newest session for the handoff cwd.
func (a *Adapter) ResumeID(handoff, _ string) (string, bool) {
	return LatestSession(a.cfg.Home, handoff)
}

// sessionID is the shape of a session directory name. Grok treats a
// UUID-shaped -r argument as an ID and anything else as a title search, and a
// leading "-" would read as a flag, so only this shape is ever passed on.
var sessionID = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// SessionsDir is where Grok Build keeps the sessions for one cwd:
// <home>/sessions/<url-escaped absolute cwd>/<uuid>/ (verified on 1.0.40).
func SessionsDir(home, cwd string) (string, error) {
	if home == "" || !filepath.IsAbs(home) {
		return "", fmt.Errorf("grok home %q is not an absolute path", home)
	}
	abs, err := filepath.Abs(cwd)
	if err != nil {
		return "", err
	}
	return filepath.Join(home, "sessions", url.PathEscape(abs)), nil
}

// LatestSession returns the newest session for cwd that has a chat history,
// which is what `grok -c` would continue. ok is false when there is none,
// including when Grok's layout has moved: Resume then fails closed.
func LatestSession(home, cwd string) (id string, ok bool) {
	dir, err := SessionsDir(home, cwd)
	if err != nil {
		return "", false
	}
	ents, err := os.ReadDir(dir)
	if err != nil {
		return "", false
	}
	var newest time.Time
	for _, e := range ents {
		if !e.IsDir() || !sessionID.MatchString(e.Name()) {
			continue
		}
		st, err := os.Stat(filepath.Join(dir, e.Name(), "chat_history.jsonl"))
		if err != nil || !st.Mode().IsRegular() {
			continue
		}
		if id == "" || st.ModTime().After(newest) {
			id, newest = e.Name(), st.ModTime()
		}
	}
	return id, id != ""
}

// NewDecoder returns grok's streaming-json decoder.
func (a *Adapter) NewDecoder() aa.Decoder { return Decoder{} }

// Decoder decodes Grok Build's streaming-json (ACP session updates).
type Decoder struct{}

// Decode maps one stdout line. Non-JSON is kept as an ordinary log line (an
// older CLI, or a fake bin that echoes argv).
func (Decoder) Decode(line []byte) []aa.Event {
	t := bytes.TrimSpace(line)
	if len(t) == 0 {
		return nil
	}
	if t[0] != '{' {
		return []aa.Event{{Kind: aa.Raw, Text: string(t)}}
	}
	// Type only. tool_call and tool_call_update carry `content` as an array
	// (1.0.40); decoding that field as a string fails the whole event. `end`
	// is decoded on its own so a nested `usage` object does not disturb End.
	var head struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(t, &head); err != nil {
		return []aa.Event{{Kind: aa.Invalid, Raw: t}}
	}
	switch head.Type {
	case "text":
		return []aa.Event{{Kind: aa.Text, Text: TextPayload(t)}}
	case "end":
		var ev struct{ aa.End }
		if err := json.Unmarshal(t, &ev); err != nil {
			return []aa.Event{{Kind: aa.Raw, Text: string(t)}}
		}
		return []aa.Event{{Kind: aa.EndEvent, End: ev.End}}
	case "tool_call", "tool_call_update":
		return []aa.Event{{Kind: aa.Tool, Raw: append([]byte(nil), t...)}}
	case "max_turns_reached":
		return []aa.Event{{Kind: aa.MaxTurns}}
	case "error":
		// message, with content kept as a string so a non-string content still
		// falls through to the raw line.
		var ev struct {
			Message string `json:"message"`
			Content string `json:"content"`
		}
		if err := json.Unmarshal(t, &ev); err != nil {
			return []aa.Event{{Kind: aa.Raw, Text: string(t)}}
		}
		msg := ev.Message
		if msg == "" {
			msg = string(t)
		}
		return []aa.Event{{Kind: aa.Error, Text: msg}}
	default:
		// thought, usage, available_commands, and whatever a later version adds.
		return []aa.Event{{Kind: aa.Ignore}}
	}
}

// TextPayload is the operator-visible body of a text event. Grok 1.0.40 sends
// `data`. Older shapes use text, then message, then content. Each key is read
// on its own so a sibling that is not a string (content:[] or text:1) does
// not drop a string sitting next to it. Absent, null, non-string, and empty
// values are skipped. No string payload logs nothing.
func TextPayload(raw []byte) string {
	var ev map[string]json.RawMessage
	if err := json.Unmarshal(raw, &ev); err != nil {
		return ""
	}
	for _, key := range []string{"data", "text", "message", "content"} {
		v, ok := ev[key]
		if !ok || len(v) == 0 || string(v) == "null" {
			continue
		}
		var s string
		if err := json.Unmarshal(v, &s); err != nil {
			continue
		}
		if s == "" {
			continue
		}
		return s
	}
	return ""
}

// Denied reports a permission-gate refusal. Grok ends the run cancelled and
// puts "User cancelled the execution for tool `<name>`" in the
// tool_call_update content, which is an array, so the raw event is scanned.
func (a *Adapter) Denied(events [][]byte, _ aa.End) (string, bool) {
	return ToolDenied(events)
}

// ToolDenied is Denied without the adapter.
func ToolDenied(events [][]byte) (string, bool) {
	const phrase = "User cancelled the execution for tool"
	names := map[string]string{}
	for _, raw := range events {
		var ev struct {
			Type       string `json:"type"`
			ToolCallID string `json:"toolCallId"`
			ToolName   string `json:"toolName"`
		}
		if json.Unmarshal(raw, &ev) != nil {
			continue
		}
		if ev.Type == "tool_call" && ev.ToolCallID != "" && ev.ToolName != "" {
			names[ev.ToolCallID] = ev.ToolName
		}
		if ev.Type != "tool_call_update" || !bytes.Contains(raw, []byte(phrase)) {
			continue
		}
		name := ev.ToolName
		if name == "" {
			name = deniedToolName(raw)
		}
		if name == "" {
			name = names[ev.ToolCallID]
		}
		if name == "" {
			name = "unknown"
		}
		return name, true
	}
	return "", false
}

func deniedToolName(raw []byte) string {
	const mark = "User cancelled the execution for tool `"
	s := string(raw)
	i := strings.Index(s, mark)
	if i < 0 {
		return ""
	}
	rest := s[i+len(mark):]
	j := strings.IndexByte(rest, '`')
	if j <= 0 {
		return ""
	}
	return rest[:j]
}

// ReviewEvidence reports whether the stream shows a completed git diff of one
// of worktrees. read_file and grep do not count.
func (a *Adapter) ReviewEvidence(events [][]byte, worktrees []string) bool {
	want := aa.PathSet(worktrees)
	if len(want) == 0 {
		return false
	}
	diffCall := map[string]bool{}
	completed := map[string]bool{}
	for _, raw := range events {
		var ev struct {
			Type       string `json:"type"`
			ToolCallID string `json:"toolCallId"`
			ToolName   string `json:"toolName"`
			Status     string `json:"status"`
			RawInput   struct {
				Command string `json:"command"`
			} `json:"rawInput"`
		}
		if json.Unmarshal(raw, &ev) != nil || ev.ToolCallID == "" {
			continue
		}
		switch ev.Type {
		case "tool_call":
			if ev.ToolName == "run_terminal_command" && aa.IsDiffOf(ev.RawInput.Command, want) {
				diffCall[ev.ToolCallID] = true
			}
		case "tool_call_update":
			if ev.Status == "completed" {
				completed[ev.ToolCallID] = true
			}
		}
	}
	for id := range diffCall {
		if completed[id] {
			return true
		}
	}
	return false
}
