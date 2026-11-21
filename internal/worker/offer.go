package worker

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/more-than-code/defect-drainer-worker/agentadapter"
	"github.com/more-than-code/defect-drainer-worker/exec"
	"github.com/more-than-code/defect-drainer-worker/workerproto"
)

// pendingOffer is an offer between receipt and start.
type pendingOffer struct {
	offer   workerproto.Offer
	once    sync.Once
	aborted chan struct{}
}

func (o *pendingOffer) abort() { o.once.Do(func() { close(o.aborted) }) }

func (o *pendingOffer) isAborted() bool {
	select {
	case <-o.aborted:
		return true
	default:
		return false
	}
}

// decision is the policy's answer for one spec.
type decision struct {
	reason string // "" accepts; else a decline reason
	detail string
	spec   exec.JobSpec // the narrowed spec
	host   *hostAdapter
	notes  []string // log lines on what the policy changed
	// domains are the hosts the agent's sandbox may reach: the spec's
	// request ∩ policy agent_allowed_domains, or the policy's list.
	domains []string
	// browser is policy agent_browser when the job's Browser is effective,
	// and masked the fresh socket scan its sandbox hides.
	browser string
	masked  []string
}

func declineOf(reason, detail string) decision { return decision{reason: reason, detail: detail} }

// handleOffer is §5.2: fetch and validate the spec, apply the policy,
// answer within 30 s, and start only once the accept has drawn no
// lease_lost within the grace.
func (w *Worker) handleOffer(ctx context.Context, s *socket, f workerproto.Offer) {
	o := &pendingOffer{offer: f, aborted: make(chan struct{})}
	w.mu.Lock()
	if w.sock != s {
		w.mu.Unlock()
		return
	}
	existing := w.jobs[f.JobID]
	if _, dup := w.offers[f.JobID]; dup {
		w.mu.Unlock()
		w.decline(s, f, workerproto.DeclineBusy, "offer already pending")
		return
	}
	// Offer modes (Main Dev, 2026-11-16): "fresh" for a new job, "rerun"
	// to the sticky worker, on the job's existing worktrees and seq record.
	var startedBy string
	switch f.Mode {
	case exec.ModeFresh:
		if existing != nil {
			w.mu.Unlock()
			w.decline(s, f, workerproto.DeclineBadSpec, "fresh offer for a job already on this worker")
			return
		}
	case exec.ModeRerun:
		if existing == nil || !w.hasSeqRecord(f.JobID) {
			w.mu.Unlock()
			w.decline(s, f, workerproto.DeclineBadSpec, "rerun of a job whose dir or seq record is gone")
			return
		}
	default:
		w.mu.Unlock()
		w.sendError(workerproto.CodeBadFrame, workerproto.TypeOffer, "unknown mode")
		return
	}
	// Snapshot under w.mu, then read each job's state after unlocking: a
	// job's flush sends (and so takes w.mu) while holding j.mu, so j.mu is
	// never taken under w.mu. The pending offers and the job set come from
	// one snapshot, as before; this offer is registered in it.
	active := len(w.offers)
	jobs := w.jobList()
	maxJobs := w.policy.MaxJobs
	w.offers[f.JobID] = o
	w.mu.Unlock()
	release := func() {
		w.mu.Lock()
		if w.offers[f.JobID] == o {
			delete(w.offers, f.JobID)
		}
		w.mu.Unlock()
	}

	if existing != nil {
		existing.mu.Lock()
		live := existing.live
		startedBy = existing.meta.Adapter
		verified := existing.meta.Spec.VerifySubmitted()
		existing.mu.Unlock()
		if verified {
			// §16: a submitted ref is verified once; a new submit is a new job.
			release()
			w.decline(s, f, workerproto.DeclineBadSpec, "a verify_submitted job does not rerun")
			return
		}
		if live {
			release()
			w.decline(s, f, workerproto.DeclineBusy, "job is running")
			return
		}
	}
	for _, j := range jobs {
		j.mu.Lock()
		if j.live {
			active++
		}
		j.mu.Unlock()
	}
	if active >= maxJobs {
		release()
		w.decline(s, f, workerproto.DeclineBusy, "max_jobs reached")
		return
	}
	if w.spoolBytes() > w.opts.SpoolMax {
		release()
		w.decline(s, f, workerproto.DeclineBusy, "spool over cap")
		return
	}
	actx, cancel := context.WithTimeout(ctx, workerproto.OfferAnswerTimeout-2*time.Second)
	defer cancel()
	d := w.evaluate(actx, f, startedBy)
	if d.reason != "" {
		release()
		w.decline(s, f, d.reason, d.detail)
		return
	}
	// Readiness before each accept (§4.2), then status.
	st := d.host.check(actx, w.opts.Paths, d.spec.StripEnv)
	w.updateStatus(st)
	w.send(w.statusFrame())
	if !st.Ready {
		release()
		w.decline(s, f, workerproto.DeclineAdapterUnavailable, d.host.name+": "+st.Reason)
		return
	}
	w.mu.Lock()
	stillHere := w.sock == s
	w.mu.Unlock()
	if !stillHere {
		release()
		return
	}
	if actx.Err() != nil {
		release()
		w.decline(s, f, workerproto.DeclineInternal, "answer window ran out")
		return
	}
	w.send(workerproto.Accept{Header: workerproto.NewHeader(workerproto.TypeAccept), JobID: f.JobID, LeaseID: f.LeaseID})
	select {
	case <-o.aborted:
		release()
		w.logf("offer %s withdrawn (lease_lost after accept)", f.JobID)
		return
	case <-ctx.Done():
		release()
		return
	case <-time.After(w.t.AcceptGrace):
	}
	if existing != nil {
		w.mu.Lock()
		aborted := o.isAborted()
		w.mu.Unlock()
		if !aborted {
			existing.rerun(f, d)
		}
		release()
		return
	}
	if _, err := w.startJob(o, d); err != nil {
		w.logf("job %s: cannot start: %v", f.JobID, err)
	}
	release()
}

// hasSeqRecord reports whether the job's seq record (job.json) is on disk.
func (w *Worker) hasSeqRecord(jobID string) bool {
	m, err := readMeta(w.opts.Paths.JobMeta(jobID))
	return err == nil && m.JobID == jobID && m.NextSeq >= 1
}

func (w *Worker) decline(s *socket, f workerproto.Offer, reason, detail string) {
	w.logf("declined offer %s: %s %s", f.JobID, reason, detail)
	w.mu.Lock()
	cur := w.sock
	w.mu.Unlock()
	if cur != s {
		return
	}
	w.send(workerproto.Decline{Header: workerproto.NewHeader(workerproto.TypeDecline), JobID: f.JobID, LeaseID: f.LeaseID, Reason: reason})
}

func (w *Worker) secretsNow() Secrets {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.secrets
}

func (w *Worker) updateStatus(st workerproto.AdapterStatus) {
	w.mu.Lock()
	defer w.mu.Unlock()
	for i := range w.statuses {
		if w.statuses[i].Name == st.Name {
			w.statuses[i] = st
		}
	}
}

// evaluate fetches the spec and applies validation and policy.
func (w *Worker) evaluate(ctx context.Context, f workerproto.Offer, startedBy string) decision {
	w.reloadHostFiles()
	w.mu.Lock()
	pol := w.policy
	org := w.cfg.OrgID
	w.mu.Unlock()
	if !pol.Usable() {
		return declineOf(workerproto.DeclinePolicyDenied, "policy.toml not valid or not enabled: "+pol.Reason)
	}
	if org == "" || !pol.HasOrg(org) {
		return declineOf(workerproto.DeclinePolicyDenied, "org not in policy.orgs")
	}
	if !w.serverHasGitToken() {
		// §14: this worker clones and ships only with server-minted tokens.
		return declineOf(workerproto.DeclineBadSpec, workerproto.DetailServerLacksGitToken)
	}
	if err := checkSpecURL(f.JobID, f.SpecURL); err != nil {
		return declineOf(workerproto.DeclineBadSpec, err.Error())
	}
	spec, err := w.fetchSpec(ctx, f.JobID, f.JobToken)
	if err != nil {
		return declineOf(workerproto.DeclineInternal, "spec fetch: "+err.Error())
	}
	if spec.JobID != f.JobID {
		return declineOf(workerproto.DeclineBadSpec, "spec jobId differs from the offer")
	}
	// §16: a verify_submitted spec comes in a fresh offer. A worker before
	// it declines here, bad_spec, as for any mode it does not know.
	if spec.Mode != f.Mode && !(f.Mode == exec.ModeFresh && spec.VerifySubmitted()) {
		return declineOf(workerproto.DeclineBadSpec, "spec mode differs from the offer")
	}
	return w.applyPolicy(spec, f.Adapter, f.Model, startedBy)
}

var (
	branchRe = regexp.MustCompile(`^[A-Za-z0-9._/-]{1,200}$`)
	commitRe = regexp.MustCompile(`^[0-9a-f]{40}([0-9a-f]{24})?$`)
)

func validRef(s string) bool {
	return branchRe.MatchString(s) && !strings.HasPrefix(s, "-") && !strings.Contains(s, "..") &&
		!strings.HasSuffix(s, "/") && !strings.HasSuffix(s, ".lock") && !strings.Contains(s, "//")
}

// validateSpec is the bad_spec check (§2, §8): ids, paths, the remote
// shape. Server-local fields are ignored and replaced by worker paths.
func validateSpec(s exec.JobSpec) error {
	if !workerproto.ValidID(s.JobID) {
		return errors.New("invalid jobId")
	}
	switch s.Kind {
	case exec.KindBatchFix, exec.KindTestRun:
	default:
		return fmt.Errorf("unknown kind %q", s.Kind)
	}
	switch s.Mode {
	case exec.ModeFresh, exec.ModeRerun, exec.ModeResume, exec.ModeVerifySubmitted:
	default:
		return fmt.Errorf("unknown mode %q", s.Mode)
	}
	if err := validateSubmitted(s); err != nil {
		return err
	}
	if s.Label == "" || !workerproto.ValidID(s.Label) {
		return errors.New("invalid label")
	}
	for _, id := range append(append([]string(nil), s.DefectIDs...), s.HarvestIDs...) {
		if !workerproto.ValidID(id) {
			return errors.New("invalid defect id")
		}
	}
	if s.Adapter != "" && !known(s.Adapter) {
		return errors.New("unknown adapter")
	}
	if !workerproto.ValidModel(s.Model) {
		return errors.New("invalid model")
	}
	for repo, base := range s.DiffBase {
		if !workerproto.ValidRelPath(repo) || !commitRe.MatchString(base) {
			return errors.New("diffBase must map repos to commit ids")
		}
	}
	if len(s.Worktrees) == 0 {
		return errors.New("no worktrees")
	}
	seen := map[string]bool{}
	for _, wt := range s.Worktrees {
		if !workerproto.ValidRelPath(wt.Repo) || strings.Contains(wt.Repo, "/") || seen[wt.Repo] {
			return errors.New("invalid worktree repo")
		}
		seen[wt.Repo] = true
		if wt.BaseSource != "origin" {
			return errors.New("base_source must be origin for a remote job")
		}
		if wt.RepoURL == "" {
			return errors.New("worktree without repoUrl")
		}
		switch {
		case s.VerifySubmitted() && wt.Branch != "":
			return errors.New("a verify_submitted run's worktrees are detached")
		case s.VerifySubmitted():
		case s.Kind == exec.KindBatchFix && wt.Branch != "dd/"+s.JobID+"/"+wt.Repo:
			return errors.New("worktree branch must be dd/<job_id>/<repo>")
		case s.Kind == exec.KindTestRun && wt.Branch != "":
			return errors.New("a test run's worktrees are detached")
		}
		if wt.BaseBranch != "" && !validRef(wt.BaseBranch) {
			return errors.New("invalid base branch")
		}
	}
	for _, r := range s.App.RepoEntries {
		if !workerproto.ValidRelPath(r.Name) {
			return errors.New("invalid repo entry name")
		}
		if r.BaseSource == "local" {
			return errors.New("base_source local is refused for remote jobs")
		}
	}
	for _, v := range s.App.VerifyCommands {
		if v.Repo != "" && !workerproto.ValidRelPath(v.Repo) {
			return errors.New("invalid verify repo")
		}
	}
	if s.Fix != nil {
		for _, ev := range s.Fix.Evidence {
			if !workerproto.ValidRelPath(ev.Path) {
				return errors.New("invalid evidence path")
			}
			if !workerproto.ValidSHA256(ev.SHA256) {
				return errors.New("evidence without sha256")
			}
		}
	}
	if err := validatePreview(s.App); err != nil {
		return err
	}
	if s.TestRun != nil {
		if s.TestRun.SecretEnv != "" && !ValidEnvName(s.TestRun.SecretEnv) {
			return errors.New("invalid secretEnv")
		}
		if s.TestRun.RunID != "" && !workerproto.ValidID(s.TestRun.RunID) {
			return errors.New("invalid test run id")
		}
	}
	return nil
}

// validateSubmitted is §16's spec shape. A verify_submitted run is a fresh
// batch fix with exactly one valid submitted ref per worktree repo; the
// worker derives its diff base, baseline and base worktrees itself. Any
// other mode carries no submitted refs.
func validateSubmitted(s exec.JobSpec) error {
	if len(s.BaselineWorktrees) > 0 {
		return errors.New("baselineWorktrees are derived by the worker")
	}
	if !s.VerifySubmitted() {
		if len(s.SubmittedRefs) > 0 {
			return errors.New("submittedRefs need mode verify_submitted")
		}
		return nil
	}
	switch {
	case s.Kind != exec.KindBatchFix:
		return errors.New("verify_submitted is for a batch fix")
	case len(s.SubmittedRefs) == 0:
		return errors.New("verify_submitted without submittedRefs")
	case len(s.DiffBase) > 0 || s.Baseline != nil || s.PriorAgentRun != nil || s.ResumeSession != "":
		return errors.New("verify_submitted measures its own diff base and baseline")
	case len(s.SubmittedRefs) != len(s.Worktrees):
		return errors.New("submittedRefs must name each worktree repo once")
	}
	repos := map[string]bool{}
	for _, wt := range s.Worktrees {
		repos[wt.Repo] = true
	}
	for _, r := range s.SubmittedRefs {
		if !repos[r.Repo] {
			return errors.New("submittedRefs must name each worktree repo once")
		}
		delete(repos, r.Repo)
		if !validRef(r.Ref) {
			return errors.New("invalid submitted ref")
		}
	}
	return nil
}

// serverHasGitToken reports whether the current hello_ok listed gittoken.v1.
func (w *Worker) serverHasGitToken() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return slices.Contains(w.serverCaps, workerproto.CapGitTokenV1)
}

// hostPushCredentials are the host's own GitHub and SSH credentials, which
// git.HostToolEnv keeps for local mode. On this worker nothing uses them
// (§14: the App's tokens only): a job child (agent, reviewer, verify) never
// gets them, and the worker's token git commands drop them too
// (gitTokenEnvDrop).
var hostPushCredentials = []string{
	"GITHUB_TOKEN", "GH_TOKEN", "GITHUB_ENTERPRISE_TOKEN", "GH_ENTERPRISE_TOKEN",
	"SSH_AUTH_SOCK", "SSH_ASKPASS", "GIT_ASKPASS",
}

// applyPolicy is §6.5 and §6.6 for a fresh spec (startedBy == "") or a
// resume spec (startedBy is the adapter that started the job).
func (w *Worker) applyPolicy(spec exec.JobSpec, offerAdapter, offerModel, startedBy string) decision {
	if err := validateSpec(spec); err != nil {
		return declineOf(workerproto.DeclineBadSpec, err.Error())
	}
	w.mu.Lock()
	pol := w.policy
	sec := w.secrets
	adapters := w.adapters
	statuses := append([]workerproto.AdapterStatus(nil), w.statuses...)
	w.mu.Unlock()
	if !pol.Usable() {
		return declineOf(workerproto.DeclinePolicyDenied, "policy.toml not valid or not enabled")
	}

	// Repos: every URL the job would clone or name must be allowed.
	for _, wt := range spec.Worktrees {
		if !pol.RepoAllowed(wt.RepoURL) {
			return declineOf(workerproto.DeclinePolicyDenied, "repo not allowed: "+wt.Repo)
		}
	}
	// §14, after the policy: every clone goes through a GitHub token, so every repo is a
	// GitHub https repo (the server refuses others at start).
	for _, wt := range spec.Worktrees {
		if c, err := CanonicalRepoURL(wt.RepoURL); err != nil || !strings.HasPrefix(c, gitHubOrigin) {
			return declineOf(workerproto.DeclineBadSpec, "not a GitHub https repo: "+wt.Repo)
		}
	}
	for _, r := range spec.App.RepoEntries {
		if r.URL != "" && !pol.RepoAllowed(r.URL) {
			return declineOf(workerproto.DeclinePolicyDenied, "repo not allowed: "+r.Name)
		}
	}

	// Adapter and model (Q4).
	want := spec.Adapter
	if offerAdapter != "" {
		if want != "" && want != offerAdapter {
			return declineOf(workerproto.DeclineBadSpec, "offer adapter differs from the spec")
		}
		want = offerAdapter
	}
	if startedBy != "" {
		if want != "" && want != startedBy {
			return declineOf(workerproto.DeclineBadSpec, "resume names another adapter")
		}
		want = startedBy
	}
	model := spec.Model
	if offerModel != "" {
		model = offerModel
	}
	if !workerproto.ValidModel(model) {
		return declineOf(workerproto.DeclinePolicyDenied, "model refused")
	}
	ready := map[string]bool{}
	for _, st := range statuses {
		ready[st.Name] = st.Ready
	}
	var chosen string
	if want != "" {
		if !pol.AdapterAllowed(want) {
			return declineOf(workerproto.DeclinePolicyDenied, "adapter not allowed: "+want)
		}
		if !ready[want] {
			return declineOf(workerproto.DeclineAdapterUnavailable, want+" not ready")
		}
		chosen = want
	} else {
		for _, a := range pol.Adapters {
			if ready[a] {
				chosen = a
				break
			}
		}
		if chosen == "" {
			return declineOf(workerproto.DeclineAdapterUnavailable, "no allowed adapter is ready")
		}
	}
	h := adapters[chosen]
	if h == nil || h.adapter == nil {
		return declineOf(workerproto.DeclineAdapterUnavailable, chosen+" not implemented")
	}
	if !pol.ModelAllowed(chosen, model) {
		shown := model
		if shown == "" {
			shown = "(CLI default)"
		}
		return declineOf(workerproto.DeclineModelNotAllowed, "model "+shown+" not allowed (policy)")
	}

	out := spec
	out.Adapter, out.Model = chosen, model
	var notes []string

	// Sandbox: the stricter of the spec's and the host floor. exec runs
	// strict or workspace; a read-only floor cannot run a job that writes.
	eff := EffectiveSandbox(spec.App.Sandbox, pol.MinSandbox)
	if eff == SandboxReadOnly {
		return declineOf(workerproto.DeclinePolicyDenied, "host min_sandbox read-only")
	}
	if eff != spec.App.Sandbox && !(spec.App.Sandbox == "" && eff == SandboxStrict) {
		notes = append(notes, fmt.Sprintf("host policy: sandbox narrowed to %s", eff))
	}
	out.App.Sandbox = eff
	if eff != SandboxWorkspace && spec.App.SimulatorWrites {
		out.App.SimulatorWrites = false
		notes = append(notes, "host policy: simulator writes off")
	}
	// D1: only grok has a workspace profile. Every other adapter runs its
	// CLI's own sandbox, strict, so the brief, the prompt and the
	// NO-SCREENSHOT release all see strict.
	if chosen != string(agentadapter.Grok) && (out.App.Sandbox != SandboxStrict || out.App.SimulatorWrites) {
		out.App.Sandbox = SandboxStrict
		out.App.SimulatorWrites = false
		notes = append(notes, "sandbox strict for "+chosen)
	}

	// Caps: clamped to max_turns and max_wall_sec.
	if out.App.MaxTurns > pol.MaxTurns || (out.App.MaxTurns == 0 && pol.MaxTurns < DefaultMaxTurns) {
		out.App.MaxTurns = pol.MaxTurns
		notes = append(notes, fmt.Sprintf("host policy: agent_max_turns clamped to %d", pol.MaxTurns))
	}
	if out.Caps.AgentMaxWallSec <= 0 || out.Caps.AgentMaxWallSec > pol.MaxWallSec {
		out.Caps.AgentMaxWallSec = pol.MaxWallSec
	}
	if out.Caps.ReviewMaxTurns > pol.MaxTurns {
		out.Caps.ReviewMaxTurns = pol.MaxTurns
	}
	if out.Caps.ReviewMaxWallSec <= 0 {
		out.Caps.ReviewMaxWallSec = exec.DefaultReviewMaxWallSec
	}
	if out.Caps.ReviewMaxWallSec > pol.MaxWallSec {
		out.Caps.ReviewMaxWallSec = pol.MaxWallSec
	}

	// Cost cap (D5): the server's, lowered by policy max_cost_usd, else the
	// policy's, else the default. A worker can lower the cap, never raise it.
	capUSD := out.Caps.AgentMaxCostUSD
	switch {
	case capUSD > 0 && pol.MaxCostUSD > 0 && float64(pol.MaxCostUSD) < capUSD:
		capUSD = float64(pol.MaxCostUSD)
		notes = append(notes, fmt.Sprintf("host policy: max cost clamped to $%d", pol.MaxCostUSD))
	case capUSD <= 0 && pol.MaxCostUSD > 0:
		capUSD = float64(pol.MaxCostUSD)
	case capUSD <= 0:
		capUSD = DefaultAgentMaxCostUSD
	}
	out.Caps.AgentMaxCostUSD = capUSD

	// Verify commands are org-admin shell: only with allow_verify_commands.
	// VerifySkipped is the worker's own record: one the server sent is
	// dropped first.
	out.App.VerifySkipped, out.App.VerifySkippedCommands = "", 0
	if len(out.App.VerifyCommands) > 0 && !pol.AllowVerifyCommands {
		out.App.VerifySkipped = exec.VerifySkippedHostPolicy
		out.App.VerifySkippedCommands = len(out.App.VerifyCommands)
		out.App.VerifyCommands = nil
		out.Baseline = nil
		notes = append(notes, "verify skipped by host policy")
	}

	// D5: the one secret this job needs must be on the host.
	if out.TestRun != nil && out.TestRun.SecretEnv != "" {
		if _, ok := sec.Get(out.TestRun.SecretEnv); !ok {
			return declineOf(workerproto.DeclineSecretMissing, out.TestRun.SecretEnv)
		}
	}
	// The deny set (§6.6): every secrets.env name but the adapter's own
	// key, plus the spec's StripEnv, plus this job's SecretEnv.
	strip := append([]string(nil), out.StripEnv...)
	if out.TestRun != nil && out.TestRun.SecretEnv != "" {
		strip = append(strip, out.TestRun.SecretEnv)
	}
	strip = append(strip, hostPushCredentials...)
	out.StripEnv = sec.DenySet(h.keep(), strip)

	domains, dropped := intersectHosts(out.App.AgentHosts, pol.AllowedDomains)
	for _, d := range dropped {
		notes = append(notes, "host policy: network host dropped: "+d)
	}
	d := decision{spec: out, host: h, notes: notes, domains: domains}
	w.applyBrowser(&d, pol, chosen)
	return d
}

// applyBrowser makes the spec's Browser effective only when the host allows
// it (B1, B4, B6): policy agent_browser, a ready browser, a claude job (the
// only adapter whose sandbox settings carry the socket switch) and a
// complete socket scan, re-run now so the job's masks are fresh. Otherwise
// Browser is cleared with one job-log note and the job runs without it.
func (w *Worker) applyBrowser(d *decision, pol Policy, adapter string) {
	app := &d.spec.App
	if !app.Browser {
		return
	}
	why := ""
	switch {
	case pol.AgentBrowser == "":
		why = "host policy: browser off (no agent_browser)"
	case adapter != string(agentadapter.Claude):
		why = "browser: claude only"
	case d.spec.Kind != exec.KindBatchFix:
		why = "browser: fix jobs only"
	default:
		if st := w.browserStatus(); !st.Ready {
			why = "browser not ready: " + st.Reason
		}
	}
	var masked []string
	if why == "" {
		var complete bool
		masked, complete = w.scanSockets(pol)
		if !complete {
			why = "browser not ready: " + workerproto.ReasonScanIncomplete
		}
	}
	if why != "" {
		app.Browser = false
		d.notes = append(d.notes, why)
		return
	}
	d.browser, d.masked = pol.AgentBrowser, masked
}

// browserStatus is the last readiness of the browser tool.
func (w *Worker) browserStatus() workerproto.ToolStatus {
	for _, t := range w.toolsNow() {
		if t.Name == workerproto.ToolBrowser {
			return t
		}
	}
	return workerproto.ToolStatus{Name: workerproto.ToolBrowser, Reason: workerproto.ReasonMissing}
}

// intersectHosts is B4: no request keeps the policy's list (rc2); a request
// keeps, in its order, the hosts the policy also allows, and returns the
// others as dropped.
func intersectHosts(request, allowed []string) (keep, dropped []string) {
	if len(request) == 0 {
		return append([]string(nil), allowed...), nil
	}
	for _, h := range request {
		if slices.Contains(allowed, h) {
			keep = append(keep, h)
		} else {
			dropped = append(dropped, h)
		}
	}
	return keep, dropped
}

// maxPreviewCmd bounds AppSettings.PreviewCmd.
const maxPreviewCmd = 512

// validatePreview is the bad_spec check of the browser fields. PreviewCmd
// runs inside the agent's sandbox (the agent runs dd-shot), so it is not an
// allow_verify_commands matter; it must still be one line.
func validatePreview(a exec.AppSettings) error {
	if len(a.AgentHosts) > 0 && !validDomains(a.AgentHosts) {
		return errors.New("invalid agentHosts")
	}
	if (a.PreviewCmd == "") != (a.PreviewURL == "") {
		return errors.New("previewCmd and previewUrl go together")
	}
	if len(a.PreviewCmd) > maxPreviewCmd || strings.ContainsAny(a.PreviewCmd, "\x00\r\n") {
		return errors.New("invalid previewCmd")
	}
	if a.PreviewURL != "" && !loopbackURL(a.PreviewURL) {
		return errors.New("previewUrl must be http://127.0.0.1, localhost or [::1] with a port")
	}
	return nil
}

// loopbackURL accepts http://<loopback>:<port>, with at most a "/" path.
func loopbackURL(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "http" || u.User != nil || u.Opaque != "" || u.RawQuery != "" || u.Fragment != "" || u.ForceQuery {
		return false
	}
	if (u.Path != "" && u.Path != "/") || u.RawPath != "" {
		return false
	}
	switch u.Hostname() {
	case "127.0.0.1", "localhost", "::1":
	default:
		return false
	}
	port, err := strconv.Atoi(u.Port())
	return err == nil && port >= 1 && port <= 65535 && u.Port() == strconv.Itoa(port)
}
