// Package worker is the worker side of worker protocol v1
// (docs/contracts/worker-protocol-v1.md): enrolment, the session loop, the
// host policy, offers, running jobs through internal/exec with
// worker-derived paths, the spool, blobs and controls. cmd/dd-worker is its
// command line. It imports no serve-side package (boundary_test.go).
package worker

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/more-than-code/defect-drainer-worker/workerproto"
)

// Paths is the worker's on-disk layout (docs/worker-w3.md §3.2). ConfigDir
// holds the host owner's files (policy.toml, secrets.env); it is root-owned
// and the worker never writes it. StateDir holds what the worker writes: the
// rotating credential (credential/env, env.prev), repos, jobs, controls.
type Paths struct {
	ConfigDir string
	StateDir  string
}

// CredentialDir is {state}/credential (mode 0700): env and env.prev.
func (p Paths) CredentialDir() string { return filepath.Join(p.StateDir, "credential") }
func (p Paths) Env() string           { return filepath.Join(p.CredentialDir(), "env") }
func (p Paths) EnvPrev() string       { return filepath.Join(p.CredentialDir(), "env.prev") }
func (p Paths) Policy() string        { return filepath.Join(p.ConfigDir, "policy.toml") }
func (p Paths) Secrets() string       { return filepath.Join(p.ConfigDir, "secrets.env") }

// LegacyEnv and LegacyEnvPrev are where a worker before W3 kept the
// credential; MigrateCredential moves them once.
func (p Paths) LegacyEnv() string     { return filepath.Join(p.ConfigDir, "env") }
func (p Paths) LegacyEnvPrev() string { return filepath.Join(p.ConfigDir, "env.prev") }

// StatusFile is {state}/status.json (dd-worker status); ReadinessDir is
// {state}/readiness (the adapters' readiness checks). Both are the
// worker's own files that an agent must not write (spec §4.1.11).
func (p Paths) StatusFile() string   { return filepath.Join(p.StateDir, "status.json") }
func (p Paths) ReadinessDir() string { return filepath.Join(p.StateDir, "readiness") }

// ReposDir is {state}/repos; JobsDir is {state}/jobs (protocol v1 §8.2).
func (p Paths) ReposDir() string    { return filepath.Join(p.StateDir, "repos") }
func (p Paths) JobsDir() string     { return filepath.Join(p.StateDir, "jobs") }
func (p Paths) ControlsDir() string { return filepath.Join(p.StateDir, "controls") }

// JobDir is {state}/jobs/<job_id>. id must already be a valid protocol id;
// JobDir panics otherwise, because a path built from an unchecked id is a
// traversal bug, not an input error.
func (p Paths) JobDir(id string) string {
	mustID(id)
	return filepath.Join(p.JobsDir(), id)
}

func (p Paths) Handoff(id string) string      { return filepath.Join(p.JobDir(id), "handoff") }
func (p Paths) WorktreeRoot(id string) string { return filepath.Join(p.JobDir(id), "worktrees") }
func (p Paths) Evidence(id string) string     { return filepath.Join(p.JobDir(id), "evidence") }
func (p Paths) Blobs(id string) string        { return filepath.Join(p.JobDir(id), "blobs") }
func (p Paths) JobMeta(id string) string      { return filepath.Join(p.JobDir(id), "job.json") }

// JobStackSecrets holds the job stack values of every run of the job
// (stacksecrets.go).
func (p Paths) JobStackSecrets(id string) string {
	return filepath.Join(p.JobDir(id), "stack-secrets.json")
}

// SpoolRoot is the dir the spool package puts its "spool" dir in.
func (p Paths) SpoolRoot(id string) string { return p.JobDir(id) }

// JobControls holds a job's stored control results, outside the job dir so
// a discard (which removes the job dir) can still answer a redelivery.
func (p Paths) JobControls(id string) string {
	mustID(id)
	return filepath.Join(p.ControlsDir(), id)
}

// Worktree is {state}/jobs/<job_id>/worktrees/<repo>.
func (p Paths) Worktree(id, repo string) (string, error) {
	return Under(p.WorktreeRoot(id), repo)
}

// BaselineRoot holds a verify_submitted job's base worktrees (§16).
func (p Paths) BaselineRoot(id string) string {
	return filepath.Join(p.JobDir(id), "baseline-worktrees")
}

// BaselineWorktree is {state}/jobs/<job_id>/baseline-worktrees/<repo>.
func (p Paths) BaselineWorktree(id, repo string) (string, error) {
	return Under(p.BaselineRoot(id), repo)
}

// RepoClone is {state}/repos/<sha256 of the canonical URL> (D1).
func (p Paths) RepoClone(canonicalURL string) string {
	sum := sha256.Sum256([]byte(canonicalURL))
	return filepath.Join(p.ReposDir(), hex.EncodeToString(sum[:]))
}

// AgentState is exec's per-job adapter state dir under DataRoot (the state
// dir): exec.Host.StateDir.
func (p Paths) AgentState(id string) string {
	mustID(id)
	return filepath.Join(p.StateDir, "agent-state", id)
}

// Under joins base and a protocol relative path (§2) and checks the result
// stays under base. Every path the worker builds from a spec or frame goes
// through it.
func Under(base, rel string) (string, error) {
	if !workerproto.ValidRelPath(rel) {
		return "", fmt.Errorf("invalid path %q", rel)
	}
	p := filepath.Join(base, filepath.FromSlash(rel))
	if !Inside(base, p) {
		return "", fmt.Errorf("path %q leaves its directory", rel)
	}
	return p, nil
}

// Inside reports whether p is base or below it, lexically.
func Inside(base, p string) bool {
	r, err := filepath.Rel(filepath.Clean(base), filepath.Clean(p))
	if err != nil {
		return false
	}
	return r == "." || (r != ".." && !strings.HasPrefix(r, ".."+string(filepath.Separator)))
}

func mustID(id string) {
	if !workerproto.ValidID(id) {
		panic(fmt.Sprintf("worker: unchecked id %q", id))
	}
}
