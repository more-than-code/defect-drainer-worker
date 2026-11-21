package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/more-than-code/defect-drainer-worker/exec"
	"github.com/more-than-code/defect-drainer-worker/git"
	"github.com/more-than-code/defect-drainer-worker/internal/botagent/config"
	"github.com/more-than-code/defect-drainer-worker/workerproto"
)

// startJob registers an accepted offer and starts its first run.
func (w *Worker) startJob(o *pendingOffer, d decision) (*job, error) {
	f := o.offer
	j, err := w.openJob(jobMeta{
		JobID: f.JobID, LeaseID: f.LeaseID, Adapter: d.host.name, Model: d.spec.Model,
		State: stateStarting, Spec: d.spec,
	})
	if err != nil {
		return nil, err
	}
	j.mu.Lock()
	j.token = f.JobToken
	j.attached = true
	j.live = true // busy from here, before the run goroutine starts
	j.mu.Unlock()
	// The abort check and the registration happen under w.mu, which
	// onError(lease_lost) also takes: a lease_lost either aborts the offer
	// here or finds the registered job.
	w.mu.Lock()
	if o.isAborted() {
		w.mu.Unlock()
		_ = os.RemoveAll(w.opts.Paths.JobDir(f.JobID))
		return nil, errors.New("lease lost before start")
	}
	w.jobs[j.id] = j
	w.mu.Unlock()
	go j.launch(d, f.LeaseID)
	return j, nil
}

// rerun takes the new lease of a rerun offer and starts a run on the
// existing worktrees; seq continues from the job's record.
func (j *job) rerun(f workerproto.Offer, d decision) {
	j.mu.Lock()
	j.meta.LeaseID = f.LeaseID
	j.meta.LeaseLost = false
	j.token = f.JobToken
	j.attached = true
	j.live = true
	_ = j.saveMetaLocked()
	j.mu.Unlock()
	j.w.rememberToken(f.JobToken)
	go j.launch(d, f.LeaseID)
}

// launch starts one run (fresh, rerun or resume) of the job: worktrees,
// evidence, the secret, then exec.Start. Any failure before the agent
// starts ends the run with done{failed, "<code>: …"}.
func (j *job) launch(d decision, leaseID string) {
	j.applyMu.Lock()
	j.mu.Lock()
	if j.meta.LeaseLost || j.meta.LeaseID != leaseID {
		// A lease_lost (or a newer lease) arrived between the decision and
		// this goroutine: the run must not start.
		j.live = false
		j.mu.Unlock()
		j.applyMu.Unlock()
		return
	}
	j.gen++
	gen := j.gen
	j.live = true
	j.cancelled = false
	j.doneSpooled = false
	j.ctx, j.cancel = context.WithCancel(context.Background())
	ctx := j.ctx
	j.meta.State = stateRunning
	_ = j.saveMetaLocked()
	host, _ := os.Hostname()
	j.emitLocked(logEvent(fmt.Sprintf("dd-worker %s on %s: %s run, adapter %s", Version, host, d.spec.Mode, d.host.name), "info"))
	for _, n := range d.notes {
		j.emitLocked(logEvent(n, "warn"))
	}
	j.mu.Unlock()
	j.applyMu.Unlock()

	spec, err := j.setup(ctx, d.spec)
	if err != nil {
		j.fail(gen, withCode(err))
		return
	}
	w := j.w
	if spec.TestRun != nil && spec.TestRun.SecretEnv != "" {
		v, ok := w.secretsNow().Get(spec.TestRun.SecretEnv)
		if !ok {
			j.fail(gen, workerproto.ReasonSecretMissing+": "+spec.TestRun.SecretEnv)
			return
		}
		w.acquireSecretEnv(spec.TestRun.SecretEnv, v)
		j.mu.Lock()
		if gen != j.gen || j.cancelled {
			// Lost or cancelled meanwhile: nothing will release it later.
			j.mu.Unlock()
			w.releaseSecretEnv(spec.TestRun.SecretEnv)
			return
		}
		j.secretEnv = spec.TestRun.SecretEnv
		j.mu.Unlock()
	}
	j.mu.Lock()
	j.meta.Spec = spec
	_ = j.saveMetaLocked()
	j.mu.Unlock()

	eh := d.host.host(w.opts.Paths, j.id, spec.Model, spec.Worktrees, d.domains)
	// A worker browser job (worker-browser B1, B2): the browser and the
	// fresh socket masks from the offer's decision.
	eh.Browser, eh.MaskedSockets = d.browser, d.masked
	eh.Postgres = d.postgres
	if spec.App.JobStackSignIn != nil {
		eh.ShotBin = shotBin()
	}
	out, err := exec.Start(ctx, eh, spec, runSink{j: j, gen: gen}, exec.Hooks{})
	if err != nil {
		j.fail(gen, withCode(err))
		return
	}
	if !out.Launched {
		j.mu.Lock()
		stopped := j.cancelled || gen != j.gen
		j.mu.Unlock()
		if !stopped {
			j.fail(gen, workerproto.CodeInternal+": run stopped before the agent started")
		}
	}
}

// withCode gives an error the §9 "<code>: " prefix when it has none.
func withCode(err error) string {
	msg := err.Error()
	if i := strings.Index(msg, ": "); i > 0 && reasonCodes[msg[:i]] {
		return msg
	}
	return workerproto.CodeInternal + ": " + msg
}

// reasonCodes are the §9 job reasons a worker can end a run with.
var reasonCodes = map[string]bool{
	workerproto.CodeInternal: true, workerproto.ReasonWorkerRestarted: true, workerproto.ReasonSecretMissing: true,
	workerproto.ReasonPolicyDenied: true, workerproto.ReasonAdapterUnavailable: true, workerproto.ReasonBaseMissing: true,
	workerproto.ReasonSpoolFull: true, workerproto.ReasonAgentAuth: true, workerproto.DeclineBadSpec: true,
	workerproto.ReasonRepoAuth: true, workerproto.ReasonSubmittedRefMissing: true, workerproto.ReasonNoMergeBase: true,
}

// fail ends run gen with done{failed}.
func (j *job) fail(gen int, line string) {
	j.applyMu.Lock()
	defer j.applyMu.Unlock()
	j.mu.Lock()
	defer j.mu.Unlock()
	if gen != j.gen {
		return
	}
	j.emitLocked(doneEvent("failed", line))
}

// setup derives every host path (§8.1–8.3): the handoff, the evidence root,
// the clones (D1) and the worktrees, and downloads the evidence blobs.
func (j *job) setup(ctx context.Context, in exec.JobSpec) (exec.JobSpec, error) {
	w := j.w
	p := w.opts.Paths
	// A deep copy: the spec's pointers (Fix, TestRun, Baseline…) are shared
	// with job.json's copy, which a flush may be marshalling right now.
	spec, err := cloneSpec(in)
	if err != nil {
		return in, err
	}
	spec.Handoff = p.Handoff(j.id)
	spec.DefectsRoot = p.Evidence(j.id)
	if spec.Fix != nil {
		spec.Fix.Batch.Path = ""
		defects := append([]exec.Defect(nil), spec.Fix.Defects...)
		for i := range defects {
			defects[i].Path = ""
		}
		spec.Fix.Defects = defects
	}
	if err := os.MkdirAll(spec.Handoff, 0o755); err != nil {
		return spec, err
	}
	wts := make([]git.WorktreeBinding, 0, len(spec.Worktrees))
	for _, in := range spec.Worktrees {
		b, err := j.cutWorktree(ctx, spec, in)
		if err != nil {
			return spec, err
		}
		wts = append(wts, b)
	}
	spec.Worktrees = wts
	if spec.VerifySubmitted() {
		if err := j.cutBaselines(&spec); err != nil {
			return spec, err
		}
	}
	if spec.Fix != nil {
		j.mu.Lock()
		tok := j.token
		j.mu.Unlock()
		for _, ev := range spec.Fix.Evidence {
			dst, err := Under(spec.DefectsRoot, ev.Path)
			if err != nil {
				return spec, fmt.Errorf("%s: %v", workerproto.DeclineBadSpec, err)
			}
			if b, err := os.ReadFile(dst); err == nil && hashHex(b) == ev.SHA256 {
				continue
			}
			data, err := w.getBlob(ctx, j.id, ev.SHA256, tok)
			if err != nil {
				return spec, fmt.Errorf("%s: evidence %s: %v", workerproto.CodeInternal, ev.Path, err)
			}
			if err := config.WriteFileAtomic(dst, data, 0o600, 0o700); err != nil {
				return spec, err
			}
		}
	}
	return spec, nil
}

func cloneSpec(in exec.JobSpec) (exec.JobSpec, error) {
	b, err := json.Marshal(in)
	if err != nil {
		return in, err
	}
	var out exec.JobSpec
	err = json.Unmarshal(b, &out)
	return out, err
}

// cloneLocks serialises clone and fetch per clone dir.
var cloneLocks sync.Map

func lockClone(dir string) func() {
	v, _ := cloneLocks.LoadOrStore(dir, &sync.Mutex{})
	m := v.(*sync.Mutex)
	m.Lock()
	return m.Unlock
}

// cutWorktree clones the repo once per host, fetches the base, and cuts
// this job's worktree on the server-assigned branch (or detached for a test
// run). Clone and fetch use a read token for this job and repo (§14). An
// existing worktree (a rerun or resume on this worker) is reused.
func (j *job) cutWorktree(ctx context.Context, spec exec.JobSpec, in git.WorktreeBinding) (git.WorktreeBinding, error) {
	w, jobID := j.w, j.id
	p := w.opts.Paths
	canon, err := CanonicalRepoURL(in.RepoURL)
	if err != nil {
		return in, fmt.Errorf("%s: repo url %s", workerproto.ReasonPolicyDenied, in.Repo)
	}
	clone := p.RepoClone(canon)
	wt, err := p.Worktree(jobID, in.Repo)
	if err != nil {
		return in, fmt.Errorf("%s: %v", workerproto.DeclineBadSpec, err)
	}
	base := in.BaseBranch
	if base == "" {
		base = "main"
	}
	unlock := lockClone(clone)
	defer unlock()
	if err := os.MkdirAll(p.ReposDir(), 0o700); err != nil {
		return in, err
	}
	tok, err := j.gitToken(ctx, in.Repo, workerproto.GitAccessRead, canon)
	if err != nil {
		return in, err
	}
	if _, err := os.Stat(clone); errors.Is(err, os.ErrNotExist) {
		if out, err := tokenGit(tok, "clone", "--no-checkout", "--origin", "origin", "--", canon, clone).CombinedOutput(); err != nil {
			_ = os.RemoveAll(clone)
			return in, w.gitFailure("clone", in.Repo, out)
		}
	} else if validRef(base) {
		// Not best-effort any more (§14): a stale base must not be used
		// silently. A base the remote lacks is base_missing.
		if out, err := tokenGit(tok, "-C", clone, "fetch", "--no-tags", "origin", "--", base).CombinedOutput(); err != nil {
			if strings.Contains(strings.ToLower(string(out)), "couldn't find remote ref") {
				return in, fmt.Errorf("%s: %s %s", workerproto.ReasonBaseMissing, in.Repo, base)
			}
			return in, w.gitFailure("fetch", in.Repo, out)
		}
	}
	ref, br, err := git.ResolveJobBase(clone, "origin", "origin", base)
	if err != nil {
		return in, fmt.Errorf("%s: %s %s", workerproto.ReasonBaseMissing, in.Repo, base)
	}
	out := in
	out.PrimaryAbs, out.WorktreeAbs, out.RepoURL = clone, wt, canon
	out.BaseSource, out.BaseBranch = "origin", br
	// BaseRef is the worker's own resolution (§8.3), never the spec's.
	out.BaseRef = ref
	// Rerun/Resume: the clone's own record says whether wt is still its
	// worktree; wt/.git (the agent's to rewrite) is never asked.
	if git.PinnedWorktree(clone, wt) {
		return out, nil
	}
	if err := os.MkdirAll(p.WorktreeRoot(jobID), 0o755); err != nil {
		return in, err
	}
	var args []string
	switch {
	case spec.VerifySubmitted():
		sha, err := j.fetchSubmitted(clone, tok, spec, in.Repo)
		if err != nil {
			return in, err
		}
		args = []string{"worktree", "add", "--detach", wt, sha}
	case in.Branch == "":
		tr, err := testRef(clone, tok, spec, in.Repo, ref)
		if err != nil {
			return in, w.gitFailure("fetch", in.Repo, []byte(err.Error()))
		}
		args = []string{"worktree", "add", "--detach", wt, tr}
	case cloneGitOK(clone, "rev-parse", "--verify", "--quiet", "--end-of-options", "refs/heads/"+in.Branch):
		args = []string{"worktree", "add", wt, in.Branch}
	default:
		args = []string{"worktree", "add", "-b", in.Branch, wt, ref}
	}
	cmd, err := git.PinnedWorktreeGit(clone, "", "", args...)
	if errors.Is(err, git.ErrLFSMissing) {
		return in, fmt.Errorf("%s: %s: %v", workerproto.ReasonLFSMissing, in.Repo, err)
	} else if err != nil {
		return in, fmt.Errorf("%s: worktree %s: %v", workerproto.CodeInternal, in.Repo, err)
	}
	if b, err := cmd.CombinedOutput(); err != nil {
		return in, fmt.Errorf("%s: worktree %s: %s", workerproto.CodeInternal, in.Repo, lastLine(b))
	}
	return out, nil
}

// testRef is a test run's pinned ref for repo (its SHA, else its ref) when
// the clone has it after a fetch, else the base. A fetch the remote refuses
// for auth is returned as an error (git's output); any other failed fetch
// falls through to the next candidate as before.
func testRef(clone, tok string, spec exec.JobSpec, repo, base string) (string, error) {
	if spec.TestRun == nil {
		return base, nil
	}
	for _, r := range spec.TestRun.Run.Refs {
		if r.Repo != repo {
			continue
		}
		for _, cand := range []string{r.SHA, r.Ref} {
			if cand == "" || !validRef(cand) {
				continue
			}
			if out, err := tokenGit(tok, "-C", clone, "fetch", "--no-tags", "origin", "--", cand).CombinedOutput(); err != nil && gitAuthFailure(out) {
				return "", errors.New(string(out))
			}
			for _, c := range []string{cand, "origin/" + cand} {
				if cloneGitOK(clone, "rev-parse", "--verify", "--quiet", "--end-of-options", c+"^{commit}") {
					return c, nil
				}
			}
		}
	}
	return base, nil
}

// fetchSubmitted fetches repo's submitted ref with the job's read token
// and returns the commit it names (§16). A ref the remote lacks, or a SHA
// ref that resolves to another commit, is submitted_ref_missing.
func (j *job) fetchSubmitted(clone, tok string, spec exec.JobSpec, repo string) (string, error) {
	ref := ""
	for _, r := range spec.SubmittedRefs {
		if r.Repo == repo {
			ref = r.Ref
		}
	}
	missing := fmt.Errorf("%s: %s %s", workerproto.ReasonSubmittedRefMissing, repo, ref)
	if ref == "" || !validRef(ref) {
		return "", missing
	}
	if out, err := tokenGit(tok, "-C", clone, "fetch", "--no-tags", "origin", "--", ref).CombinedOutput(); err != nil {
		if gitAuthFailure(out) {
			return "", j.w.gitFailure("fetch", repo, out)
		}
		if refMissing(out) {
			return "", missing
		}
		return "", j.w.gitFailure("fetch", repo, out)
	}
	sha, ok := cloneGitOut(clone, "rev-parse", "--verify", "--quiet", "--end-of-options", "FETCH_HEAD^{commit}")
	if !ok || (commitRe.MatchString(ref) && sha != ref) {
		return "", missing
	}
	return sha, nil
}

// refMissing reports whether a failed fetch said the remote has no such ref.
func refMissing(out []byte) bool {
	s := strings.ToLower(string(out))
	for _, m := range []string{"couldn't find remote ref", "not our ref", "no such remote ref", "unadvertised object"} {
		if strings.Contains(s, m) {
			return true
		}
	}
	return false
}

// cutBaselines is the rest of §16's setup, all local to the clones: each
// submitted worktree's merge-base with its base becomes the diff base, and
// a second detached worktree at the base is where the baseline runs.
func (j *job) cutBaselines(spec *exec.JobSpec) error {
	p := j.w.opts.Paths
	spec.DiffBase = map[string]string{}
	spec.BaselineWorktrees = nil
	for _, b := range spec.Worktrees {
		ref := ""
		for _, r := range spec.SubmittedRefs {
			if r.Repo == b.Repo {
				ref = r.Ref
			}
		}
		head := exec.WorktreeHead(b)
		if head == "" {
			return fmt.Errorf("%s: worktree %s has no head", workerproto.CodeInternal, b.Repo)
		}
		mb, ok := cloneGitOut(b.PrimaryAbs, "merge-base", "--end-of-options", head, b.BaseRef)
		if !ok || !commitRe.MatchString(mb) {
			return fmt.Errorf("%s: %s %s", workerproto.ReasonNoMergeBase, b.Repo, ref)
		}
		spec.DiffBase[b.Repo] = mb
		dir, err := p.BaselineWorktree(j.id, b.Repo)
		if err != nil {
			return fmt.Errorf("%s: %v", workerproto.DeclineBadSpec, err)
		}
		base := b
		base.WorktreeAbs, base.Branch = dir, ""
		spec.BaselineWorktrees = append(spec.BaselineWorktrees, base)
		if git.PinnedWorktree(b.PrimaryAbs, dir) {
			continue
		}
		if err := os.MkdirAll(p.BaselineRoot(j.id), 0o755); err != nil {
			return err
		}
		unlock := lockClone(b.PrimaryAbs)
		cmd, err := git.PinnedWorktreeGit(b.PrimaryAbs, "", "", "worktree", "add", "--detach", dir, b.BaseRef)
		if errors.Is(err, git.ErrLFSMissing) {
			unlock()
			return fmt.Errorf("%s: %s: %v", workerproto.ReasonLFSMissing, b.Repo, err)
		} else if err != nil {
			unlock()
			return fmt.Errorf("%s: baseline worktree %s: %v", workerproto.CodeInternal, b.Repo, err)
		}
		out, err := cmd.CombinedOutput()
		unlock()
		if err != nil {
			return fmt.Errorf("%s: baseline worktree %s: %s", workerproto.CodeInternal, b.Repo, lastLine(out))
		}
	}
	return nil
}

// cloneGitOut runs a local (no network) git command in the clone, pinned,
// and returns its trimmed output when it succeeded.
func cloneGitOut(clone string, args ...string) (string, bool) {
	cmd, err := git.PinnedWorktreeGit(clone, "", "", args...)
	if err != nil {
		return "", false
	}
	out, err := cmd.Output()
	if err != nil {
		return "", false
	}
	return strings.TrimSpace(string(out)), true
}

func lastLine(b []byte) string {
	s := strings.TrimSpace(string(b))
	if i := strings.LastIndexByte(s, '\n'); i >= 0 {
		s = s[i+1:]
	}
	if len(s) > 300 {
		s = s[:300]
	}
	return s
}

// cloneGitOK runs a local (no network) git command in the clone, pinned,
// and reports whether it succeeded.
func cloneGitOK(clone string, args ...string) bool {
	cmd, err := git.PinnedWorktreeGit(clone, "", "", args...)
	return err == nil && cmd.Run() == nil
}

// shotBin is this dd-worker binary, symlinks resolved to its versioned
// path, which dd-shot runs as `dd-worker shot` for a Stack sign-in (S10).
// "" when it cannot be found: the run then fails before the agent starts.
func shotBin() string {
	p, err := os.Executable()
	if err != nil {
		return ""
	}
	if r, err := filepath.EvalSymlinks(p); err == nil {
		p = r
	}
	return p
}
