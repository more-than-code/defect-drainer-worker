package worker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"unicode/utf8"

	"github.com/more-than-code/defect-drainer-worker/exec"
	"github.com/more-than-code/defect-drainer-worker/git"
	"github.com/more-than-code/defect-drainer-worker/internal/botagent/config"
	"github.com/more-than-code/defect-drainer-worker/workerproto"
)

// handleControl is §6.3: a stored result answers a repeated request_id
// without running it again; a new one runs once, is stored, then sent.
func (w *Worker) handleControl(ctx context.Context, f workerproto.Control) {
	switch f.Control.Kind {
	case exec.ControlCancel, exec.ControlResume, exec.ControlShipPreview, exec.ControlDiff, exec.ControlShip, exec.ControlDiscard:
	default:
		w.sendError(workerproto.CodeBadFrame, workerproto.TypeControl, "unknown control.kind")
		return
	}
	if w.resendStored(f.JobID, f.RequestID) {
		return
	}
	j := w.job(f.JobID)
	if j == nil || (f.Control.Kind == exec.ControlResume && !w.hasSeqRecord(f.JobID)) {
		// Resume and every other control need the job's dir; a resume also
		// needs its seq record, else the run would restart at 1 (Main Dev,
		// 2026-11-16). Nothing ran, so the answer is not stored.
		w.send(result(f, false, nil, workerproto.DeclineBadSpec, "job dir or seq record not on this worker"))
		return
	}
	j.ctrlMu.Lock()
	defer j.ctrlMu.Unlock()
	j.mu.Lock()
	j.ctrlActive++
	j.mu.Unlock()
	defer func() {
		j.mu.Lock()
		j.ctrlActive--
		j.mu.Unlock()
	}()
	if w.resendStored(f.JobID, f.RequestID) {
		return
	}
	if f.JobToken != "" {
		j.mu.Lock()
		j.token = f.JobToken
		j.mu.Unlock()
	}
	res := w.runControl(ctx, j, f)
	b, err := json.Marshal(w.redactResult(res))
	if err != nil {
		return
	}
	if err := config.WriteFileAtomic(w.resultPath(f.JobID, f.RequestID), b, 0o600, 0o700); err != nil {
		w.logf("job %s: control result not stored: %v", f.JobID, err)
	}
	w.sendRaw(b)
}

// redactResult replaces the host's secret values in every string of a
// result (a diff or a git error can carry what the agent wrote, D5).
func (w *Worker) redactResult(res workerproto.ControlResult) workerproto.ControlResult {
	r := w.currentRedactor()
	res.Detail = r.Values(res.Detail)
	if len(res.Result) == 0 {
		return res
	}
	var tree any
	if json.Unmarshal(res.Result, &tree) != nil {
		return res
	}
	tree = walkStrings(tree, "", func(_, s string) string { return r.Values(s) })
	if b, err := json.Marshal(tree); err == nil {
		res.Result = b
	}
	return res
}

func (w *Worker) resendStored(jobID, requestID string) bool {
	b, err := os.ReadFile(w.resultPath(jobID, requestID))
	if err != nil {
		return false
	}
	w.sendRaw(b)
	return true
}

func result(f workerproto.Control, ok bool, v any, code, detail string) workerproto.ControlResult {
	r := workerproto.ControlResult{
		Header: workerproto.NewHeader(workerproto.TypeControlResult),
		JobID:  f.JobID, RequestID: f.RequestID, OK: ok, Code: code, Detail: detail,
	}
	if v == nil {
		v = struct{}{}
	}
	if ok {
		r.Result, _ = json.Marshal(v)
	}
	return r
}

func (w *Worker) runControl(ctx context.Context, j *job, f workerproto.Control) workerproto.ControlResult {
	ok := func(v any) workerproto.ControlResult { return result(f, true, v, "", "") }
	fail := func(code, detail string) workerproto.ControlResult { return result(f, false, nil, code, detail) }
	j.mu.Lock()
	live := j.live
	meta := j.meta
	j.mu.Unlock()
	switch f.Control.Kind {
	case exec.ControlCancel:
		j.cancelRun()
		return ok(nil)
	case exec.ControlResume:
		w.reloadHostFiles()
		if live {
			return fail(workerproto.CodeInternal, "job is running")
		}
		if meta.Spec.VerifySubmitted() {
			return fail(workerproto.DeclineBadSpec, "a verify_submitted job does not resume")
		}
		if !w.serverHasGitToken() {
			return fail(workerproto.DeclineBadSpec, workerproto.DetailServerLacksGitToken)
		}
		if err := checkSpecURL(f.JobID, f.Control.SpecURL); err != nil {
			return fail(workerproto.DeclineBadSpec, err.Error())
		}
		j.mu.Lock()
		tok := j.token
		j.mu.Unlock()
		spec, err := w.fetchSpec(ctx, f.JobID, tok)
		if err != nil {
			return fail(workerproto.CodeInternal, "spec fetch: "+err.Error())
		}
		if spec.JobID != f.JobID {
			return fail(workerproto.DeclineBadSpec, "spec jobId differs")
		}
		d := w.applyPolicy(spec, "", "", meta.Adapter)
		if d.reason != "" {
			return fail(d.reason, d.detail)
		}
		st := d.host.check(ctx, w.opts.Paths, w.secretsNow().DenySet(d.host.keep(), nil))
		w.updateStatus(st)
		if !st.Ready {
			return fail(workerproto.ReasonAdapterUnavailable, d.host.name+": "+st.Reason)
		}
		j.mu.Lock()
		j.live = true
		lease := j.meta.LeaseID
		j.mu.Unlock()
		go j.launch(d, lease)
		return ok(nil)
	case exec.ControlShipPreview:
		out := workerproto.ShipPreviewResult{Repos: []workerproto.ShipPreviewRepo{}}
		for _, b := range meta.Spec.Worktrees {
			files, err := dirtyFiles(b)
			if err != nil {
				return fail(workerproto.CodeInternal, b.Repo+": "+err.Error())
			}
			out.Repos = append(out.Repos, workerproto.ShipPreviewRepo{Repo: b.Repo, Files: files, Message: "Fix " + meta.Spec.Label})
		}
		return ok(out)
	case exec.ControlDiff:
		b, found := binding(meta, f.Control.Repo)
		if !found {
			return fail(workerproto.DeclineBadSpec, "unknown repo")
		}
		base := meta.Spec.DiffBase[b.Repo]
		if base == "" {
			base = b.BaseRef
		}
		d, err := diffAgainst(b, base)
		if err != nil {
			return fail(workerproto.CodeInternal, err.Error())
		}
		d = w.currentRedactor().Values(d)
		truncated := false
		if len(d) > workerproto.MaxDiffBytes {
			d, truncated = cutUTF8(d, workerproto.MaxDiffBytes), true
		}
		// JSON escaping can double a diff: keep the frame under 1 MiB.
		for encodedLen(d) > workerproto.MaxFrameBytes-64<<10 {
			d, truncated = cutUTF8(d, len(d)*3/4), true
		}
		return ok(workerproto.DiffResult{Repo: b.Repo, Diff: d, Truncated: truncated})
	case exec.ControlShip:
		w.reloadHostFiles()
		w.mu.Lock()
		allow := w.policy.Usable() && w.policy.AllowShip
		w.mu.Unlock()
		if !allow {
			return fail(workerproto.ReasonPolicyDenied, "allow_ship is off on this host")
		}
		if live {
			return fail(workerproto.CodeInternal, "job is running")
		}
		if meta.Spec.Kind != exec.KindBatchFix {
			return fail(workerproto.DeclineBadSpec, "only a batch fix ships")
		}
		if meta.Spec.VerifySubmitted() {
			// §16: the worker never pushes in this mode.
			return fail(workerproto.DeclineBadSpec, "a verify_submitted job never ships")
		}
		for _, r := range f.Control.Repos {
			b, found := binding(meta, r.Repo)
			if !found {
				return fail(workerproto.DeclineBadSpec, "unknown repo "+r.Repo)
			}
			if b.BaseSource != "origin" {
				return fail(workerproto.DeclineBadSpec, "ship is PR-only: "+r.Repo+" is not an origin base")
			}
			if b.Branch != "dd/"+j.id+"/"+b.Repo || b.Branch == b.BaseBranch {
				return fail(workerproto.DeclineBadSpec, "not a job branch: "+r.Repo)
			}
		}
		out := workerproto.ShipPushResult{Repos: []workerproto.ShipPushRepo{}}
		for _, r := range f.Control.Repos {
			b, _ := binding(meta, r.Repo)
			out.Repos = append(out.Repos, j.shipRepo(ctx, b, r))
		}
		return ok(out)
	case exec.ControlDiscard:
		if live {
			return fail(workerproto.CodeInternal, "job is running")
		}
		dir := w.opts.Paths.JobDir(j.id)
		git.DiscardWorktrees(append(append([]git.WorktreeBinding(nil), meta.Spec.Worktrees...), meta.Spec.BaselineWorktrees...), dir)
		if err := os.RemoveAll(dir); err != nil {
			return fail(workerproto.CodeInternal, "discard: "+err.Error())
		}
		_ = os.RemoveAll(w.opts.Paths.AgentState(j.id))
		w.mu.Lock()
		delete(w.jobs, j.id)
		w.mu.Unlock()
		return ok(nil)
	}
	return fail(workerproto.CodeBadFrame, "unknown control.kind")
}

func encodedLen(s string) int {
	b, _ := json.Marshal(s)
	return len(b)
}

func binding(meta jobMeta, repo string) (git.WorktreeBinding, bool) {
	for _, b := range meta.Spec.Worktrees {
		if b.Repo == repo {
			return b, true
		}
	}
	return git.WorktreeBinding{}, false
}

// shipRepo commits the confirmed files on the job branch and pushes it
// with a write token for this job and repo (§14). It runs no gh: the server
// opens or updates the pull request from this result.
//
// The worktree's .git file is the agent's to rewrite, so ship never lets
// git find the repository through it (a redirected git dir would bring the
// agent's config: a proxy, sslVerify, insteadOf, a filter). add, commit and
// rev-parse run pinned (git.PinnedWorktreeGit) on the worktree's admin dir
// as registered in the clone, and the push runs in the clone, to the
// canonical URL, with the head commit.
func (j *job) shipRepo(ctx context.Context, b git.WorktreeBinding, r exec.ShipRepo) workerproto.ShipPushRepo {
	res := workerproto.ShipPushRepo{Repo: b.Repo, Branch: b.Branch, Base: b.BaseBranch}
	wt := b.WorktreeAbs
	gitDir, ok := git.WorktreeAdminDir(b.PrimaryAbs, wt)
	if !ok {
		res.Error = "worktree missing"
		return res
	}
	// pinned runs one git command, with env appended; a refused command
	// (lfs_missing) is its error output.
	pinnedEnv := func(env []string, wt string, args ...string) ([]byte, error) {
		cmd, err := git.PinnedWorktreeGit(b.PrimaryAbs, gitDir, wt, args...)
		if errors.Is(err, git.ErrLFSMissing) {
			return []byte(workerproto.ReasonLFSMissing + ": " + err.Error()), err
		} else if err != nil {
			return []byte(err.Error()), err
		}
		cmd.Env = append(cmd.Env, env...)
		return cmd.CombinedOutput()
	}
	pinned := func(wt string, args ...string) ([]byte, error) { return pinnedEnv(nil, wt, args...) }
	if len(r.Files) > 0 {
		for _, f := range r.Files {
			if _, err := Under(wt, f); err != nil {
				res.Error = "invalid file " + f
				return res
			}
		}
		msg := strings.TrimSpace(r.Message)
		if msg == "" || len(msg) > 2000 {
			res.Error = "commit message empty or too long"
			return res
		}
		if out, err := pinned(wt, append([]string{"add", "--"}, r.Files...)...); err != nil {
			res.Error = "git add: " + lastLine(out)
			return res
		}
		if _, err := pinned(wt, "diff", "--cached", "--quiet"); err != nil {
			if out, err := pinnedEnv(commitIdentity, wt, "commit", "-m", msg); err != nil {
				res.Error = "git commit: " + lastLine(out)
				return res
			}
		}
	}
	out, err := pinned("", "rev-parse", "--verify", "HEAD^{commit}")
	head := strings.TrimSpace(string(out))
	if err != nil || !commitRe.MatchString(head) {
		res.Error = "git rev-parse HEAD failed"
		return res
	}
	if b.BaseRef != "" {
		n, err := pinned("", "rev-list", "--count", b.BaseRef+".."+head, "--")
		if err != nil {
			res.Error = "git rev-list failed"
			return res
		}
		if strings.TrimSpace(string(n)) == "0" {
			res.Error = "no commits ahead of base"
			return res
		}
	}
	tok, err := j.gitToken(ctx, b.Repo, workerproto.GitAccessWrite, b.RepoURL)
	if err != nil {
		res.Error = err.Error()
		return res
	}
	if out, err := tokenGit(tok, "-C", b.PrimaryAbs, "push", b.RepoURL, head+":refs/heads/"+b.Branch).CombinedOutput(); err != nil {
		res.Error = j.w.gitFailure("push", b.Repo, out).Error()
		return res
	}
	res.Pushed, res.HeadSHA = true, head
	return res
}

// dirtyFiles lists uncommitted, non-ignored paths in the worktree.
func dirtyFiles(b git.WorktreeBinding) ([]string, error) {
	out, err := pinnedOut(b, "status", "--porcelain=v1", "-z", "--untracked-files=all")
	if err != nil {
		return nil, err
	}
	files := []string{}
	parts := bytes.Split(out, []byte{0})
	for i := 0; i < len(parts); i++ {
		e := parts[i]
		if len(e) < 4 {
			continue
		}
		files = append(files, string(e[3:]))
		if e[0] == 'R' || e[0] == 'C' {
			i++ // the rename's source path
		}
	}
	return files, nil
}

// diffAgainst is the worktree's diff (committed and uncommitted) against
// base, plus untracked files as additions.
func diffAgainst(b git.WorktreeBinding, base string) (string, error) {
	args := []string{"diff", "--no-color"}
	if strings.HasPrefix(base, "-") {
		return "", fmt.Errorf("invalid diff base")
	}
	if base != "" {
		args = append(args, base)
	}
	out, err := pinnedOut(b, append(args, "--")...)
	if err != nil {
		return "", err
	}
	s := string(out)
	if untracked, err := pinnedOut(b, "ls-files", "--others", "--exclude-standard"); err == nil {
		for _, f := range strings.Split(strings.TrimSpace(string(untracked)), "\n") {
			if f != "" {
				s += "\nuntracked: " + f
			}
		}
	}
	if !utf8.ValidString(s) {
		s = strings.ToValidUTF8(s, "�")
	}
	return s, nil
}

// pinnedOut runs git pinned (git.PinnedBindingGit) in b's worktree and
// returns its stdout.
func pinnedOut(b git.WorktreeBinding, args ...string) ([]byte, error) {
	cmd, err := git.PinnedBindingGit(b, args...)
	if err != nil {
		return nil, err
	}
	return cmd.Output()
}

// The worker commits as Defect Drainer on every host (the maintainers, 2026-11-18): the
// pinned git reads no global config, and a host's GIT_AUTHOR_* never
// reaches a pinned call, so the identity is set here for ship's commit only.
const (
	commitName  = "Defect Drainer"
	commitEmail = "dd-worker@users.noreply.defectdrainer.com"
)

var commitIdentity = []string{
	"GIT_AUTHOR_NAME=" + commitName, "GIT_AUTHOR_EMAIL=" + commitEmail,
	"GIT_COMMITTER_NAME=" + commitName, "GIT_COMMITTER_EMAIL=" + commitEmail,
}
