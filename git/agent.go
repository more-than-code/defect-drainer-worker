package git

import (
	"context"
	"os"
	"os/exec"

	"github.com/more-than-code/defect-drainer-worker/agentadapter"
	"github.com/more-than-code/defect-drainer-worker/agentadapter/grok"
)

// DefaultAdapter is grok, configured from this process's environment: the
// binary from ResolveGrokBin and the session store from GrokHome. The adapter
// itself reads no environment.
func DefaultAdapter() agentadapter.Adapter {
	home, _ := GrokHome()
	return grok.New(grok.Config{Bin: ResolveGrokBin(), Home: home})
}

// childBaseEnv is the inherited part of an agent child's environment, before
// the adapter adds its own variables: env hygiene, keeping only this
// adapter's own credential names.
func childBaseEnv(a agentadapter.Adapter, strip []string) []string {
	return agentadapter.ChildEnv(os.Environ(), a.CredentialEnv(), strip)
}

// filterStderr passes each stderr line through the adapter's filter before
// the job log sees it.
func filterStderr(a agentadapter.Adapter, emit func(string)) func(string) {
	if emit == nil {
		return nil
	}
	return func(line string) {
		if out := a.FilterStderr(line); out != "" {
			emit(out)
		}
	}
}

// redactCredential wraps the child's stdout and stderr so the adapter's
// key, and a subscription token the child holds (CLAUDE_CODE_OAUTH_TOKEN),
// never reach the decoder, agent.log or the job log. flush drains the
// wrappers before the rest of the pipeline. Empty values are skipped.
func redactCredential(cmd *exec.Cmd, key string) (flush func()) {
	var ws []*redactWriter
	for _, secret := range []string{key, agentadapter.GetEnv(cmd.Env, "CLAUDE_CODE_OAUTH_TOKEN")} {
		if secret == "" {
			continue
		}
		out := newRedactWriter(cmd.Stdout, secret)
		errw := newRedactWriter(cmd.Stderr, secret)
		cmd.Stdout, cmd.Stderr = out, errw
		ws = append(ws, out, errw)
	}
	return func() {
		// Outermost first: each flush feeds the wrapper beneath it.
		for i := len(ws) - 1; i >= 0; i-- {
			_ = ws[i].Flush()
		}
	}
}

// AgentReady runs the adapter's readiness check (login or key) under the
// same base env the agent child gets.
func AgentReady(ctx context.Context, a agentadapter.Adapter, handoff string, cred agentadapter.Credential, strip []string) error {
	return a.Ready(ctx, agentadapter.Task{
		Role: agentadapter.Implementer, Handoff: handoff, Credential: cred,
		Env: childBaseEnv(a, strip),
	})
}

// SandboxHeadline is the prompt's one-line sandbox statement. Grok keeps its
// historical wording; the other adapters run strict only (v1).
func SandboxHeadline(a agentadapter.Adapter, sandbox string) string {
	if a != nil && a.Name() != agentadapter.Grok {
		return "SANDBOX: the " + string(a.Name()) + " CLI's own sandbox, strict (Defect Drainer runs " + string(a.Name()) + " strict only)."
	}
	if sandbox == "workspace" {
		return "SANDBOX: --sandbox workspace (App Settings)."
	}
	return "SANDBOX: --sandbox strict / restrict (App Settings)."
}

// adapterSandboxNotes is the sandbox paragraph for a non-grok adapter: the
// handoff and the worktrees are the only writable paths (the Claude settings
// file's Edit rules, agy's workspace), shell commands are sandboxed, and
// there is no ~/.grok.
func adapterSandboxNotes(a agentadapter.Adapter, handoffAbs, defectsRoot string) []string {
	return []string{
		SandboxHeadline(a, "strict"),
		"- Write: cwd (" + handoffAbs + ") and the worktrees listed below, nothing else; other writes are refused.",
		"- Shell commands run inside the sandbox; outbound network may be blocked. Temp dirs are not private: keep nothing secret there.",
		"- Product code: edit only under the worktrees listed below.",
		"- Historical fix evidence outside cwd may be unreadable; write new proof to handoff fix-evidence/.",
		"- Inventory updates under " + defectsRoot + " are blocked; write new proof to handoff fix-evidence/.",
		"- A refused tool is reported back as a denial; do not try to work around it.",
	}
}
