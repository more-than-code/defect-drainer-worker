package worker

import (
	"bytes"
	"context"
	"errors"
	"os"
	osexec "os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/more-than-code/defect-drainer-worker/agentadapter"
	"github.com/more-than-code/defect-drainer-worker/agentadapter/antigravity"
	"github.com/more-than-code/defect-drainer-worker/agentadapter/claude"
	"github.com/more-than-code/defect-drainer-worker/agentadapter/grok"
	"github.com/more-than-code/defect-drainer-worker/exec"
	"github.com/more-than-code/defect-drainer-worker/git"
	"github.com/more-than-code/defect-drainer-worker/workerproto"
)

// AdapterConfig is how the host's coding-agent CLIs are found. Empty bins
// are looked up on PATH; empty homes take the CLI's default.
type AdapterConfig struct {
	ClaudeBin       string
	ClaudeConfigDir string // subscription mode: the logged-in config dir
	GrokBin         string
	GrokHome        string
	AgyBin          string
	AgyHome         string
}

// hostAdapter is one adapter as this host runs it.
type hostAdapter struct {
	name            string
	cfg             AdapterConfig
	adapter         agentadapter.Adapter // nil: no adapter implementation (codex)
	cred            agentadapter.Credential
	perJobConfigDir bool
	keyName         string
}

// buildAdapters makes every known adapter. An adapter key in secrets.env
// puts that adapter in api_key mode; otherwise it uses the CLI's own login
// under the worker user's HOME.
func buildAdapters(cfg AdapterConfig, sec Secrets) map[string]*hostAdapter {
	out := map[string]*hostAdapter{}
	for _, name := range KnownAdapters {
		h := &hostAdapter{name: name, cfg: cfg, cred: agentadapter.Credential{Mode: agentadapter.Subscription}}
		switch name {
		case "claude":
			h.adapter = claude.New(claude.Config{Bin: agentadapter.LookBin(cfg.ClaudeBin, "claude", "/usr/local/bin/claude", "/opt/homebrew/bin/claude")})
			h.keyName = adapterKeyNames[agentadapter.Claude]
			if key, ok := sec.Get(h.keyName); ok {
				h.cred = agentadapter.Credential{Mode: agentadapter.APIKey, Key: key}
				h.perJobConfigDir = true
			} else {
				h.cred.ConfigDir = cfg.ClaudeConfigDir
			}
		case "grok":
			home := cfg.GrokHome
			if home == "" {
				if hd, err := os.UserHomeDir(); err == nil {
					home = filepath.Join(hd, ".grok")
				}
			}
			h.adapter = grok.New(grok.Config{Bin: agentadapter.LookBin(cfg.GrokBin, "grok", "/usr/local/bin/grok", "/opt/homebrew/bin/grok"), Home: home})
		case "antigravity":
			h.adapter = antigravity.New(antigravity.Config{Bin: agentadapter.LookBin(cfg.AgyBin, "agy", "/usr/local/bin/agy", "/opt/homebrew/bin/agy"), Home: cfg.AgyHome})
			h.cred.ConfigDir = cfg.AgyHome
		}
		out[name] = h
	}
	return out
}

// forJob returns the adapter for one job: antigravity takes the job's model;
// the others have no model switch and ignore it.
func (h *hostAdapter) forJob(model string) agentadapter.Adapter {
	if h.name == "antigravity" && model != "" {
		a := antigravity.New(antigravity.Config{Bin: h.adapter.Bin(), Home: h.cfg.AgyHome, Model: model})
		return a
	}
	return h.adapter
}

// keep is the credential names this adapter's child may keep.
func (h *hostAdapter) keep() []string {
	if h.keyName == "" {
		return nil
	}
	return []string{h.keyName}
}

// host is the exec.Host for a job of this adapter.
func (h *hostAdapter) host(p Paths, jobID, model string) exec.Host {
	return exec.Host{
		DataRoot:        p.StateDir,
		Adapter:         h.forJob(model),
		Credential:      h.cred,
		PerJobConfigDir: h.perJobConfigDir,
		// The agent may not read the worker's config (secrets.env, policy),
		// its credential (env, env.prev), the clones or the stored control
		// results. exec adds {state}/jobs (other jobs, spools, blobs) and
		// agent-state.
		EnvFiles:    append([]string{p.ConfigDir, p.CredentialDir(), p.ReposDir(), p.ControlsDir()}, homeCredentialFiles()...),
		DefectsRoot: p.Evidence(jobID),
	}
}

// homeCredentialFiles are credential files in the worker user's HOME that
// exec's own deny list does not name.
func homeCredentialFiles() []string {
	home, err := os.UserHomeDir()
	if err != nil || !filepath.IsAbs(home) {
		return nil
	}
	var out []string
	for _, f := range []string{".git-credentials", ".npmrc", ".pypirc", ".cargo/credentials", ".cargo/credentials.toml", ".local/share/gh"} {
		out = append(out, filepath.Join(home, f))
	}
	return out
}

// check runs the install and login checks under the exact child env a job
// of this adapter would get: agentadapter.ChildEnv over the worker's own
// environment, with strip (the secrets.env deny set) removed (§4.1).
func (h *hostAdapter) check(ctx context.Context, p Paths, strip []string) workerproto.AdapterStatus {
	st := workerproto.AdapterStatus{Name: h.name, CredentialMode: string(h.cred.Mode)}
	if h.adapter == nil {
		st.Reason = workerproto.ReasonMissing
		return st
	}
	if agentadapter.HostNote(h.adapter) != "" {
		st.Reason = workerproto.ReasonUnsupportedOS
		return st
	}
	env := agentadapter.ChildEnv(os.Environ(), h.adapter.CredentialEnv(), strip)
	probe := agentadapter.ProbeVersion(ctx, h.adapter, env)
	st.Version = probe.Found
	switch probe.State {
	case agentadapter.VersionMissing:
		st.Reason = workerproto.ReasonMissing
		return st
	case agentadapter.VersionTooOld:
		st.Reason = workerproto.ReasonTooOld
		return st
	}
	st.MinOK = true
	if h.name == "antigravity" && h.cfg.AgyHome == "" {
		st.Reason = workerproto.ReasonSignedOut
		return st
	}
	dir := filepath.Join(p.StateDir, "readiness", h.name)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		st.Reason = workerproto.ReasonMissing
		return st
	}
	cred := h.cred
	if h.perJobConfigDir {
		cred.ConfigDir = filepath.Join(dir, "cli-config")
	}
	if err := git.AgentReady(ctx, h.adapter, dir, cred, strip); err != nil {
		var ae *agentadapter.AuthError
		switch {
		case h.cred.Mode == agentadapter.APIKey:
			st.Reason = workerproto.ReasonNoKey
		case errors.As(err, &ae):
			st.Reason = workerproto.ReasonSignedOut
		default:
			st.Reason = workerproto.ReasonMissing
		}
		return st
	}
	st.Ready = true
	return st
}

// toolProbeTimeout bounds the git probe.
const toolProbeTimeout = 10 * time.Second

// checkTools reports whether this host's git can clone and push for a job:
// a git of at least MinGitVersion, run through git.HostGit. Under
// gittoken.v1 that is all a worker needs (§14): no gh, no login, no
// credential helper. Only the version, ready and reason leave this function.
func checkTools(ctx context.Context) []workerproto.ToolStatus {
	g := workerproto.ToolStatus{Name: workerproto.ToolGit}
	g.Version, g.Reason = probeVersion(ctx, git.HostGit("--version"), workerproto.MinGitVersion)
	g.Ready = g.Reason == ""
	return []workerproto.ToolStatus{g}
}

// probeVersion runs a --version command: "" and missing when it does not
// run, too_old under min, else the version and no reason.
func probeVersion(ctx context.Context, cmd *osexec.Cmd, min string) (version, reason string) {
	out, err := runProbe(ctx, cmd)
	if err != nil {
		return "", workerproto.ReasonMissing
	}
	v := versionRe.FindString(out)
	if v == "" {
		return "", workerproto.ReasonMissing
	}
	if versionLess(v, min) {
		return v, workerproto.ReasonTooOld
	}
	return v, ""
}

var versionRe = regexp.MustCompile(`\d+\.\d+(?:\.\d+)?`)

// versionLess compares dotted numeric versions; a missing part is 0.
func versionLess(a, b string) bool {
	as, bs := strings.Split(a, "."), strings.Split(b, ".")
	for i := 0; i < 3; i++ {
		var x, y int
		if i < len(as) {
			x, _ = strconv.Atoi(as[i])
		}
		if i < len(bs) {
			y, _ = strconv.Atoi(bs[i])
		}
		if x != y {
			return x < y
		}
	}
	return false
}

// runProbe runs cmd from / (no repository's config applies) with stdin
// closed and a timeout, and returns its stdout. Stderr is dropped unread.
func runProbe(ctx context.Context, cmd *osexec.Cmd) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, toolProbeTimeout)
	defer cancel()
	cmd.Dir = "/"
	cmd.WaitDelay = time.Second
	var out bytes.Buffer
	cmd.Stdout = &out
	if err := cmd.Start(); err != nil {
		return "", err
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		return out.String(), err
	case <-ctx.Done():
		_ = cmd.Process.Kill()
		<-done
		return "", ctx.Err()
	}
}
