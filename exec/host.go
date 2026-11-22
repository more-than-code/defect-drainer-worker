package exec

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"

	"github.com/more-than-code/defect-drainer-worker/agentadapter"
	"github.com/more-than-code/defect-drainer-worker/git"
)

// The layout of DD's data and inventory dirs, as internal/paths names it.
// Spelled out here because internal/paths reads DD settings (Resolve) and
// this package reads none; internal/jobs pins them equal (paths_parity_test).
const (
	DataDBFile       = "defect-drainer.db"
	DataJobsDir      = "jobs"
	DataBatchJobsDir = "batch-jobs"
	EvidenceDirName  = "evidence"
)

// Host is this machine's coding-agent setup. It never travels in a JobSpec:
// the adapter, its credential and the paths an agent may not read belong to
// the host that runs the agent.
type Host struct {
	// DataRoot is the host's data directory (handoffs, agent state).
	DataRoot string
	// Adapter is the CLI. Nil means grok, resolved per use from the process
	// env (git.DefaultAdapter), exactly as before adapters.
	Adapter agentadapter.Adapter
	// Credential is the host's credential for that adapter.
	Credential agentadapter.Credential
	// PerJobConfigDir gives each job its own CLI config dir under the job's
	// state dir (Claude in API-key mode).
	PerJobConfigDir bool
	// EnvFiles are the host's env files; the agent may not read them.
	EnvFiles []string
	// DefectsRoot is the inventory root whose evidence an agent may not read.
	DefectsRoot string
	// ReadOnly are paths every agent of the job (implementer and reviewers)
	// may read but never edit. A worker sets the job's repo clone .git dirs
	// and its per-job report evidence; local serve sets none. A DenyRead
	// entry equal to or inside one of them is dropped; an ancestor stays
	// denied and the adapter re-allows only these paths.
	ReadOnly []string
	// AllowedDomains are the hosts the implementer's sandbox may reach
	// (empty: no network).
	AllowedDomains []string
	// DenyWrite are paths every agent of the job may not write (a worker's
	// status.json, readiness dir and $HOME, spec §4.1.11). Local serve sets
	// none.
	DenyWrite []string
	// Browser is the worker's headless browser when the job's Browser is
	// effective (worker-browser B1); "" otherwise and in local serve.
	// MaskedSockets are the host sockets the implementer's sandbox hides
	// once it has unix sockets (B2).
	Browser       string
	MaskedSockets []string
	// Postgres is policy agent_postgres when the job's stack needs
	// Postgres (job stack S2, S8); "" otherwise.
	Postgres string
	// ShotBin is the dd-worker binary dd-shot runs as `dd-worker shot` for
	// a Stack sign-in (S10); "" in local serve.
	ShotBin string
}

// AgentAdapter is the host's adapter.
func (h Host) AgentAdapter() agentadapter.Adapter {
	if h.Adapter != nil {
		return h.Adapter
	}
	return git.DefaultAdapter()
}

// LogSource is the job-log source for agent output, without resolving grok's
// binary on every line.
func (h Host) LogSource() string {
	if h.Adapter != nil {
		return h.Adapter.LogSource()
	}
	return "Grok"
}

// StateDir is the job's adapter state: settings files and, in Claude
// API-key mode, the CLI config dir. It sits beside the handoff, never in it,
// so the agent cannot rewrite its own permission rules.
func (h Host) StateDir(jobID string) string {
	return filepath.Join(h.DataRoot, "agent-state", jobID)
}

// DenyRead is every path an agent child must not read: the host owner's
// secrets, DD's data (database, evidence, intake jobs, clones, agent state
// and every handoff, this job's own re-allowed by the adapter), the host's
// env files, and the CLI config and login dirs. Fail closed: a path that
// does not exist is still listed.
func (h Host) DenyRead() []string {
	var out []string
	if home, err := os.UserHomeDir(); err == nil && filepath.IsAbs(home) {
		for _, p := range []string{".ssh", ".config", ".aws", ".gnupg", ".kube", ".docker", ".netrc", ".claude", ".claude.json", ".grok", ".gemini"} {
			out = append(out, filepath.Join(home, p))
		}
	}
	if h.DataRoot != "" {
		out = append(out,
			filepath.Join(h.DataRoot, DataDBFile), filepath.Join(h.DataRoot, DataDBFile)+"-wal", filepath.Join(h.DataRoot, DataDBFile)+"-shm",
			filepath.Join(h.DataRoot, "agent-state"),
			filepath.Join(h.DataRoot, DataJobsDir),
			filepath.Join(h.DataRoot, "clones"),
			filepath.Join(h.DataRoot, DataBatchJobsDir),
		)
	}
	if h.DefectsRoot != "" {
		out = append(out, filepath.Join(h.DefectsRoot, EvidenceDirName))
	}
	out = append(out, h.EnvFiles...)
	if c := h.Credential; c.Mode == agentadapter.Subscription && c.ConfigDir != "" {
		out = append(out, c.ConfigDir)
	}
	var clean []string
	seen := map[string]bool{}
	for _, p := range out {
		if p == "" || !filepath.IsAbs(p) || seen[filepath.Clean(p)] || within(p, h.ReadOnly) {
			continue
		}
		seen[filepath.Clean(p)] = true
		clean = append(clean, filepath.Clean(p))
	}
	return clean
}

// within reports whether p is one of dirs or inside one.
func within(p string, dirs []string) bool {
	p = filepath.Clean(p)
	for _, d := range dirs {
		d = filepath.Clean(d)
		if d != "" && (p == d || strings.HasPrefix(p, d+string(filepath.Separator))) {
			return true
		}
	}
	return false
}

// CredentialFor is the host credential with a per-job config dir when the
// adapter runs in API-key mode.
func (h Host) CredentialFor(jobID string) agentadapter.Credential {
	c := h.Credential
	if h.PerJobConfigDir {
		c.ConfigDir = filepath.Join(h.StateDir(jobID), "cli-config")
	}
	return c
}

// CheckReady runs the adapter's login/key check under the child env the
// first iteration will get. A failure is the job reason agent_auth.
func (h Host) CheckReady(ctx context.Context, jobID, handoff string, strip []string) error {
	a := h.AgentAdapter()
	err := git.AgentReady(ctx, a, handoff, h.CredentialFor(jobID), strip)
	if err == nil {
		return nil
	}
	var ae *agentadapter.AuthError
	if errors.As(err, &ae) {
		return fmt.Errorf("%s (%s)", ae.Error(), a.Name())
	}
	return err
}

// FormatGrokExit is grok's historical job error for a failed iteration.
func FormatGrokExit(err error) string {
	code, sig := "null", "null"
	if err == nil {
		return fmt.Sprintf("Grok exited code=%s signal=%s", code, sig)
	}
	ee, ok := err.(*exec.ExitError)
	if !ok {
		// A child that never started has no exit status; its reason is the
		// only useful thing to show (e.g. a refused sandbox profile).
		return err.Error()
	}
	if ee.ProcessState != nil {
		code = strconv.Itoa(ee.ExitCode())
		if ws, ok := ee.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
			sig = UnixSignalName(ws.Signal())
		}
	}
	return fmt.Sprintf("Grok exited code=%s signal=%s", code, sig)
}

// FormatAgentExit is the job error for a failed iteration. Grok keeps its
// historical text; the others name the adapter.
func FormatAgentExit(a agentadapter.Adapter, err error) string {
	msg := FormatGrokExit(err)
	if a.Name() == agentadapter.Grok {
		return msg
	}
	const prefix = "Grok exited "
	if len(msg) > len(prefix) && msg[:len(prefix)] == prefix {
		return "Agent (" + string(a.Name()) + ") exited " + msg[len(prefix):]
	}
	return msg
}

// Proc is one live child. Wait is once-only so a delete and the completion
// goroutine can both reap without racing exec.Cmd.Wait.
type Proc struct {
	Cmd      *exec.Cmd
	waitOnce sync.Once
	waitErr  error
	// Group is set for a reviewer. Stop and Delete kill its process group.
	// The implementer stays a normal process.
	Group bool
}

// Kill stops a live child. A reviewer is a process-group leader; SIGKILL the
// group so git and shell grandchildren die with it.
func (p *Proc) Kill() {
	if p == nil || p.Cmd == nil || p.Cmd.Process == nil {
		return
	}
	if p.Group {
		git.KillReviewProcess(p.Cmd)
		return
	}
	_ = p.Cmd.Process.Kill()
}

// Reap waits for the child once.
func (p *Proc) Reap() error {
	if p == nil {
		return nil
	}
	p.waitOnce.Do(func() {
		var pid int
		if p.Cmd != nil && p.Cmd.Process != nil {
			pid = p.Cmd.Process.Pid
		}
		if p.Cmd != nil {
			p.waitErr = p.Cmd.Wait()
			// A background child keeps the stdout pipe open, so Wait reports
			// ErrWaitDelay after the leader has already exited 0. That is not
			// an implementer failure; the group kill below reaps the child.
			if errors.Is(p.waitErr, exec.ErrWaitDelay) && p.Cmd.ProcessState != nil && p.Cmd.ProcessState.Success() {
				p.waitErr = nil
			}
		}
		// The reviewer's post-reap kill is suppressed (R12). The implementer's
		// descendants are the ones that must not outlive this Wait.
		if !p.Group && pid > 0 {
			git.KillImplementerGroup(pid)
		}
	})
	return p.waitErr
}

// abandonStarted kills and reaps a child started for a job that has gone.
func abandonStarted(cmd *exec.Cmd, flush func()) {
	if cmd != nil && cmd.Process != nil {
		pid := cmd.Process.Pid
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		git.KillImplementerGroup(pid)
	}
	if flush != nil {
		flush()
	}
}

// Sink is the transport's side of a run: where events go and where control
// comes from. The local transport is the orchestrator itself; W2's remote
// one is the worker's spool and its control frames.
type Sink interface {
	// Apply takes one event, in order. An error stops the run where it is:
	// ErrInactive quietly, ErrParked to be started again later, anything
	// else as the run's failure.
	Apply(Event) error
	// Gone reports the job deleted; Cancelled reports it deleted or
	// stopped. Neither starts work.
	Gone() bool
	Cancelled() bool
	// Track installs the live child so Stop and Delete can reach it. It
	// returns false when the job is gone or stopped: the caller kills it.
	Track(p *Proc) bool
	// Untrack removes p if it is still the installed child.
	Untrack(p *Proc)
}

// Hooks are test seams. Nil in production.
type Hooks struct {
	// AfterStart runs after the first child starts and before it is
	// tracked: a live child exists but nothing can reach it yet.
	AfterStart func()
	// BeforeNext runs after an iteration is reaped and the loop decided to
	// continue, before the next child starts: the gap with no process.
	BeforeNext func()
	// LensStarted runs once that lens's child is tracked, before Wait.
	LensStarted func(lens string)
	// LensFinished runs once that lens has been applied.
	LensFinished func(lens string)
	// Manifest returns the reviewer's manifest reader, read at capture time.
	Manifest func() ManifestIO
	// StackSecrets receives a run's job stack values before dd-stack is
	// written: its {random} value (also a generated sign-in password) and a
	// typed sign-in password. A worker keeps them for the diff and ship
	// controls after the run (§17). They are never sent in an event. An
	// error fails the run's setup.
	StackSecrets func(values []string) error
}
