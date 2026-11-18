// Package agentadapter runs one coding-agent CLI (Grok Build, Claude Code,
// Antigravity) behind one interface: binary and version, readiness, argv for
// the implementer and the reviewer, the per-job settings file, the stream
// decoder, the resume id, and reviewer evidence and denials.
//
// Input is a plain Task and output is events plus the process the caller
// starts. Persistence, the driver loop, verify and review orchestration stay
// with the caller (serve today, a worker later).
//
// Import rule (docs/claude-code-runner.md criterion 11): nothing under this
// package imports internal/store, db, api, jobs, feedback or env, and no code
// here reads os.Environ or a DEFECT_DRAINER_* variable. The caller resolves
// configuration and hands it over. boundary_test.go enforces it.
package agentadapter

import (
	"context"
	"errors"
	"time"
)

// Name is an adapter name, as DEFECT_DRAINER_AGENT_RUNNER spells it.
type Name string

const (
	Grok        Name = "grok"
	Claude      Name = "claude"
	Antigravity Name = "antigravity"
)

// Role is which kind of agent process a Task starts.
type Role int

const (
	// Implementer is the fix or test-run agent: it may edit the handoff and
	// the worktrees, and it resumes across iterations.
	Implementer Role = iota
	// Reviewer is one fresh, read-only review lens. It never resumes.
	Reviewer
)

// CredentialMode is how the adapter's CLI authenticates.
type CredentialMode string

const (
	// Subscription is the CLI's own login, owned by the host owner.
	Subscription CredentialMode = "subscription"
	// APIKey is a key the host owner put in serve's env file. It is passed
	// to this adapter's child only, under the CLI's native variable name.
	APIKey CredentialMode = "api_key"
)

// Credential is what the caller resolved for this host. The adapter never
// reads a credential itself.
type Credential struct {
	Mode CredentialMode
	// Key is the API key value in APIKey mode. Never logged, never in argv.
	Key string
	// ConfigDir is the CLI's config home: the logged-in dir in subscription
	// mode, or a per-job dir in API-key mode (Claude). Empty keeps the
	// CLI's default.
	ConfigDir string
}

// Task is one agent process to start.
type Task struct {
	Role Role
	// Handoff is the cwd: the job's handoff dir, or a review lens dir.
	Handoff string
	// Worktrees are the job's worktree paths the agent may edit.
	Worktrees []string
	// ReadOnly are absolute paths (files or dirs) the agent may read but never
	// edit: a reviewer's view of the job's BRIEF.md, NOTES.md and fix-notes.
	ReadOnly []string
	// Prompt is sent as is. On a resume the caller passes the continue prompt.
	Prompt string
	// MaxTurns is the per-iteration turn cap. Adapters without one ignore it.
	MaxTurns int
	// MaxTurnsArg, when set, is passed to grok's --max-turns verbatim (the
	// env override has always been passed through unparsed).
	MaxTurnsArg string
	// Continue resumes the previous iteration of this job. Grok continues
	// the latest session for the cwd; the others need ResumeID.
	Continue bool
	// ResumeID resumes that exact session. Wins over Continue.
	ResumeID string
	// Sandbox is the app's base sandbox (strict, workspace, read-only) and
	// SandboxProfile a job-scoped grok profile extending it.
	Sandbox        string
	SandboxProfile string
	// Env is the child's base environment, already filtered by the caller
	// (see ChildEnv). The adapter appends its own variables.
	Env []string
	// Credential is this adapter's credential for the host.
	Credential Credential
	// StateDir is a per-job directory outside the handoff that the agent
	// cannot write. Settings files live here.
	StateDir string
	// DenyRead are absolute paths the agent must not read (host secrets,
	// DD's data, other jobs' handoffs, env files, CLI config dirs). The
	// caller builds the list; adapters with read rules enforce it.
	DenyRead []string
	// Timeout bounds one invocation for CLIs that need it (agy).
	Timeout time.Duration
	// AllowCommands are binaries the agent's shell may run, for CLIs that
	// allow-list commands (agy). git is always added.
	AllowCommands []string
	// Grok-only switches the caller read from its env.
	GrokBypassPermissions bool
	GrokTools             string
}

// Command is a process to start: exec.Command(Bin, Args...) with Dir and Env.
type Command struct {
	Bin  string
	Args []string
	Dir  string
	Env  []string
}

// End is the run's last event, normalised across adapters. The JSON names
// are grok's `end` event, which the jobs package already decodes.
type End struct {
	StopReason   string  `json:"stopReason"`
	SessionID    string  `json:"sessionId"`
	NumTurns     int     `json:"num_turns"`
	TotalCostUSD float64 `json:"total_cost_usd"`
	// IsError is the CLI's own failure flag (Claude is_error, agy status
	// ERROR). Grok has none.
	IsError bool `json:"-"`
	// AuthFailed means the CLI could not authenticate: the job reason is
	// agent_auth, never a retry.
	AuthFailed bool `json:"-"`
	// Message is the CLI's error text when IsError.
	Message string `json:"-"`
	// Denials are the tools the CLI's permission layer refused.
	Denials []string `json:"-"`
	// Tokens is the run's total token count where the CLI reports no cost.
	Tokens int `json:"-"`
}

// EventKind is a decoded stream event.
type EventKind int

const (
	// Ignore is anything the job log does not show (thoughts, usage, …).
	Ignore EventKind = iota
	// Text is assistant prose.
	Text
	// Tool is a tool call or result. Raw is what ReviewEvidence and Denied
	// read; the job log never shows it.
	Tool
	// EndEvent is the run's end.
	EndEvent
	// MaxTurns is the positive turn-cap signal. Turn exhaustion also exits
	// non-zero, and this is what tells it apart from a failure.
	MaxTurns
	// Error is an error event; Text holds the message.
	Error
	// Raw is a line that is not an event (an older CLI, a fake bin): it is
	// logged as is.
	Raw
	// Invalid is a `{` line that did not decode.
	Invalid
)

// Event is one decoded stream line. A line can yield several events.
type Event struct {
	Kind EventKind
	Text string
	Raw  []byte
	End  End
}

// Decoder turns stdout lines into events. One decoder per process.
type Decoder interface {
	Decode(line []byte) []Event
}

// Adapter is one coding-agent CLI.
type Adapter interface {
	Name() Name
	// Bin is the resolved binary path, "" when not installed.
	Bin() string
	// MinVersion is DD's minimum supported version.
	MinVersion() string
	// VersionArgs is the argv that prints the version.
	VersionArgs() []string
	// Prepare writes the per-task settings or sandbox file. It runs before
	// every spawn, so a file the agent changed is rewritten.
	Prepare(t Task) error
	// Command builds the process for t.
	Command(t Task) (Command, error)
	// NewDecoder returns a fresh stream decoder.
	NewDecoder() Decoder
	// FilterStderr maps one stderr line to what may be logged ("" drops it).
	FilterStderr(line string) string
	// ResumeID is the session to resume: from the CLI's own store (grok) or
	// the id stored from the last run (the others). ok false means the job
	// cannot be resumed.
	ResumeID(handoff, stored string) (id string, ok bool)
	// ResumesByID reports whether later iterations need ResumeID rather than
	// Continue.
	ResumesByID() bool
	// ReviewEvidence reports whether the reviewer's tool events show a
	// completed git diff of one of the worktrees.
	ReviewEvidence(tools [][]byte, worktrees []string) bool
	// Denied reports a permission refusal that must block a reviewer lens.
	Denied(tools [][]byte, end End) (tool string, ok bool)
	// Ready checks the login or key under the task's exact child env.
	Ready(ctx context.Context, t Task) error
	// CredentialEnv are the native credential variable names of this CLI.
	// Every other adapter's child, and every non-agent child, is denied them.
	CredentialEnv() []string
	// LogSource is the job-log source name.
	LogSource() string
	// SupportsSimulator reports whether the adapter can run the workspace
	// sandbox or Simulator writes. Only grok can in v1.
	SupportsSimulator() bool
}

// ErrAuth wraps a readiness or run failure caused by the login or key. The
// job reason is agent_auth.
var ErrAuth = errors.New("agent_auth")

// AuthError is a readiness failure with the CLI's reason.
type AuthError struct{ Reason string }

func (e *AuthError) Error() string { return "agent_auth: " + e.Reason }
func (e *AuthError) Unwrap() error { return ErrAuth }

// HostLimiter is implemented by an adapter that cannot run on some host OS.
// HostUnsupported returns a short marker ("unsupported-on-darwin") or "".
type HostLimiter interface {
	HostUnsupported() string
}

// HostNote is a's host marker, "" when a runs here.
func HostNote(a Adapter) string {
	if h, ok := a.(HostLimiter); ok {
		return h.HostUnsupported()
	}
	return ""
}
