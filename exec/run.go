package exec

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/more-than-code/defect-drainer-worker/agentadapter"
	"github.com/more-than-code/defect-drainer-worker/git"
)

// Outcome is how Start ended. Launched: the driver loop is running on its
// own goroutine. Parked: the job waits for a host lease and will be started
// again. Neither: the run stopped before an agent started (job gone, or an
// error).
type Outcome struct {
	Launched bool
	Parked   bool
}

// run is one Start: the spec, where events go, and the run's own state.
type run struct {
	ctx   context.Context
	host  Host
	spec  JobSpec
	sink  Sink
	hooks Hooks
	// secret is the test account password, read from the host env once the
	// orchestrator has accepted the run. It never leaves this host.
	secret string
}

// loopFinish is what the driver loop hands the after-loop phase once it has
// stopped for a reason other than Stop or Delete.
type loopFinish struct {
	handoff      string
	failed       bool
	wts          []git.WorktreeBinding
	diffBase     map[string]string
	verifyEnv    map[string]string
	realByRepo   map[string]string
	gitDirByRepo map[string]string
	baseline     *VerificationRun
}

// Start runs one job on this host: handoff, readiness, toolchain, baseline,
// then the first agent iteration. It returns once that iteration is running,
// or when the run stops before it. The driver loop, verify, review and
// harvest go on in the background and report through sink. ctx cancels
// baseline and verify.
func Start(ctx context.Context, h Host, spec JobSpec, sink Sink, hooks Hooks) (Outcome, error) {
	r := &run{ctx: ctx, host: h, spec: spec, sink: sink, hooks: hooks}
	return r.start()
}

func (r *run) isTestRun() bool { return r.spec.Kind == KindTestRun }

// emit sends one event. Log lines and the agent-authored text in a status
// or outcome are redacted here, so a transport never carries the test
// account's password.
func (r *run) emit(e Event) error {
	e.JobID = r.spec.JobID
	if r.secret == "" {
		return r.sink.Apply(e)
	}
	redact := func(s string) string { return strings.ReplaceAll(s, r.secret, "[redacted]") }
	if len(e.Lines) > 0 {
		lines := make([]LogLine, len(e.Lines))
		for i, l := range e.Lines {
			l.Text = redact(l.Text)
			lines[i] = l
		}
		e.Lines = lines
	}
	if e.Status != nil {
		st := *e.Status
		st.Error = redact(st.Error)
		e.Status = &st
	}
	if e.Done != nil {
		d := *e.Done
		d.Line = redact(d.Line)
		e.Done = &d
	}
	if e.AgentRun != nil && e.AgentRun.StopReason != "" {
		ar := *e.AgentRun
		ar.StopReason = redact(ar.StopReason)
		e.AgentRun = &ar
	}
	return r.sink.Apply(e)
}

func (r *run) status(phase string, lines ...LogLine) error {
	return r.emit(Event{Kind: EventStatus, Status: &Status{Phase: phase}, Lines: lines})
}

// log is an unguarded line: kept while the job exists, even once stopped.
func (r *run) log(line, level, source string) {
	_ = r.emit(Event{Kind: EventLog, Lines: []LogLine{{Text: line, Level: level, Source: source}}})
}

// guarded is a line dropped once the job is stopped.
func guarded(line, level string) LogLine {
	return LogLine{Text: line, Level: level, Source: "DefectDrainer", Guarded: true}
}

// stopOn maps an answer from the sink to Start's result: inactive is a quiet
// stop, anything else is the run's error.
func stopOn(err error) (Outcome, error) {
	if err == ErrInactive {
		return Outcome{}, nil
	}
	return Outcome{}, err
}

func parseCutoff(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return time.Time{}
	}
	return t
}

// harvestCutoff is the job's creation: older files do not count.
func (r *run) harvestCutoff() time.Time { return parseCutoff(r.spec.CreatedAt) }

// importCutoff is the stricter cutoff for importing images: the start of
// the latest restart, else the job's creation. The loop keeps harvestCutoff
// so artifacts a resumed session already wrote still count toward
// completion; only the import, and so resolve, is per run.
func (r *run) importCutoff() time.Time {
	if r.spec.RunStartedAt != "" {
		if t, err := time.Parse(time.RFC3339Nano, r.spec.RunStartedAt); err == nil {
			return t
		}
	}
	return r.harvestCutoff()
}

func (r *run) writeHandoff() error {
	s := r.spec
	if r.isTestRun() {
		in := TestBriefInput{AppID: s.AppID, Worktrees: s.Worktrees}
		if s.TestRun != nil {
			in.TestRunID = s.TestRun.RunID
			in.Run = s.TestRun.Run
			in.SecretEnv = s.TestRun.SecretEnv
		}
		return WriteTestHandoff(s.Handoff, in)
	}
	// A deleted job gets no brief.
	if r.sink.Gone() {
		return nil
	}
	in := BatchFixBriefInput{DefectsRoot: s.DefectsRoot, GrokSandbox: s.App.Sandbox, Worktrees: s.Worktrees}
	// Only Claude's settings carry the host's network allow list.
	if r.host.AgentAdapter().Name() == agentadapter.Claude {
		in.AllowedDomains = r.host.AllowedDomains
		in.NoNetwork = len(in.AllowedDomains) == 0
	}
	if s.Fix != nil {
		in.Batch = s.Fix.Batch
		in.Defects = s.Fix.Defects
	}
	return WriteHandoffBrief(s.Handoff, in)
}

func (r *run) prepare() {
	if r.isTestRun() {
		_ = os.MkdirAll(filepath.Join(r.spec.Handoff, "results"), 0o755)
		_ = os.MkdirAll(filepath.Join(r.spec.Handoff, "evidence"), 0o755)
		return
	}
	_ = os.MkdirAll(filepath.Join(r.spec.Handoff, "fix-evidence"), 0o755)
	_ = os.MkdirAll(filepath.Join(r.spec.Handoff, "fix-notes"), 0o755)
}

// complete is the loop's stop: the artifacts BRIEF.md demands.
func (r *run) complete(cutoff time.Time) (bool, []string) {
	if r.isTestRun() {
		if r.spec.TestRun == nil || !r.spec.TestRun.Loaded {
			var missing []string
			if _, _, ok := ReadRegularFile(r.spec.Handoff, filepath.Join(r.spec.Handoff, "RUN.md"), cutoff); !ok {
				missing = append(missing, "RUN.md")
			}
			return false, append(missing, "test run missing")
		}
		return testArtifactsComplete(r.spec.Handoff, r.spec.TestRun.Run.Steps, cutoff, r.secret)
	}
	return ArtifactsComplete(r.spec.Handoff, r.spec.DefectIDs, r.spec.App.Sandbox, cutoff)
}

func (r *run) start() (Outcome, error) {
	s := r.spec
	handoff, wts, label := s.Handoff, s.Worktrees, s.Label
	if err := r.writeHandoff(); err != nil {
		return Outcome{}, err
	}
	if err := r.status(PhaseHandoff); err != nil {
		return stopOn(err)
	}
	sandbox := s.App.Sandbox
	toolchainKind := s.App.Toolchain
	maxTurns := s.App.MaxTurns
	r.prepare()
	turnsNote := "default"
	if maxTurns > 0 {
		turnsNote = strconv.Itoa(maxTurns)
	}
	appLine := LogLine{Text: fmt.Sprintf("app %s grok_sandbox=%s agent_toolchain=%s agent_max_turns=%s", s.AppID, sandbox, toolchainKind, turnsNote), Level: "info", Source: "DefectDrainer"}
	if err := r.status(PhasePrepared, appLine); err != nil {
		return stopOn(err)
	}
	strip := s.StripEnv
	// Login or key, under the child's exact env, before baseline spends time.
	if err := r.host.CheckReady(r.ctx, s.JobID, handoff, strip); err != nil {
		return Outcome{}, err
	}
	if err := r.status(PhaseReady); err != nil {
		if err == ErrParked {
			return Outcome{Parked: true}, nil
		}
		return stopOn(err)
	}

	spawnLog := func(line, level string) { r.log(line, level, "DefectDrainer") }
	provisioned := NO_TOOLCHAIN
	if toolchainKind == "flutter" {
		provisioned = ProvisionFlutterToolchain(handoff, wts, spawnLog, strip)
	}

	var sandboxProfile string
	var profileBody []byte
	var profileNotes []string
	if s.App.SimulatorWrites {
		profile, err := WriteSimulatorSandboxProfile(handoff, sandbox, label)
		if err != nil {
			spawnLog("sandbox: job profile failed — skipped ("+err.Error()+")", "warn")
		} else {
			sandboxProfile = profile.Profile
			profileBody = profile.Body
			profileNotes = profile.Notes
			spawnLog("sandbox: job profile '"+profile.Profile+"' extends "+sandbox+" + Simulator device writes", "info")
		}
	}

	diffBaseByRepo := map[string]string{}
	for _, w := range wts {
		if sha := s.DiffBase[w.Repo]; sha != "" {
			diffBaseByRepo[w.Repo] = sha
		} else {
			diffBaseByRepo[w.Repo] = WorktreeHead(w)
		}
	}
	realByRepo := SnapshotWorktreeReals(wts)
	gitDirByRepo := SnapshotGitDirs(wts)

	verifyCommands := s.App.VerifyCommands
	var baseline *VerificationRun
	if !r.isTestRun() {
		if s.Mode != ModeFresh {
			baseline = s.Baseline
			if baseline != nil {
				spawnLog("baseline: reusing the first run's (measured before any agent edits)", "info")
			} else if len(verifyCommands) > 0 {
				spawnLog("baseline: none stored — skipped; verify judges without one, so every red command blocks", "warn")
			}
		} else if len(verifyCommands) > 0 {
			vrun := RunVerification(r.ctx, verifyCommands, wts, filepath.Join(handoff, "baseline"), s.App.RepoEntries, provisioned.VerifyEnv, realByRepo, func(line, level string) {
				spawnLog("baseline "+line, level)
			}, strip)
			baseline = &vrun
			if vrun.Ran {
				_ = r.emit(Event{Kind: EventBaseline, Baseline: baseline, Lines: []LogLine{guarded("baseline: "+SummarizeVerification(vrun), "info")}})
			}
		}
	}

	if r.sink.Cancelled() {
		return Outcome{}, nil
	}

	adapter := r.host.AgentAdapter()
	bin := adapter.Bin()
	source := adapter.LogSource()
	onStdout := func(line string) { r.log(line, "info", source) }
	onStderr := func(line string) { r.log(line, "warn", source) }
	var verifySpecs []git.VerifySpec
	for _, v := range verifyCommands {
		verifySpecs = append(verifySpecs, git.VerifySpec{Repo: v.Repo, Command: v.Command})
	}
	notes := append(append([]string{}, provisioned.Notes...), profileNotes...)
	acc := &AgentRunAcc{}
	// Re-run and Resume add to the job's record. The caps below still count
	// this run's iterations only, so every restart gets a fresh budget.
	priorIters, resumes := 0, 0
	if s.Mode != ModeFresh && s.PriorAgentRun != nil {
		acc.Seed(s.PriorAgentRun)
		priorIters, resumes = s.PriorAgentRun.Iterations, s.PriorAgentRun.Resumes
	}
	if s.Mode == ModeResume {
		resumes++
	}
	runRecord := func(iter int, stoppedBy string, missing []string) *AgentRun {
		rec := acc.Snapshot(priorIters+iter, stoppedBy, missing)
		rec.Resumes = resumes
		rec.Runner = string(adapter.Name())
		return rec
	}
	// Every iteration uses identical argv except for -c + the continue prompt,
	// so the sandbox and turn budget cannot drift between them.
	// agy has no turn cap; its per-invocation limit is --print-timeout, set to
	// the loop's wall budget.
	agentTimeout := time.Duration(s.Caps.AgentMaxWallSec) * time.Second
	startIter := func(resume bool, session string) (*exec.Cmd, func(), error) {
		// The previous iteration ran with this handoff as its writable cwd,
		// and grok re-reads the profile on every start.
		if sandboxProfile != "" {
			if err := CheckSimulatorSandboxProfile(handoff, profileBody); err != nil {
				return nil, nil, err
			}
		}
		extra := map[string]string{}
		for k, v := range provisioned.Env {
			extra[k] = v
		}
		opts := &git.CodingAgentOpts{
			ExtraEnv:       extra,
			SandboxProfile: sandboxProfile,
			ToolchainNotes: notes,
			VerifyCommands: verifySpecs,
			AlreadyFailing: AlreadyFailingLines(baseline),
			MaxTurns:       maxTurns,
			MaxTurnsArg:    s.Caps.AgentMaxTurnsArg,
			Continue:       resume,
			ResumeSession:  session,
			StripEnv:       strip,
			OnEnd: func(e git.AgentEnd) {
				acc.End(e.StopReason, e.SessionID, e.NumTurns, e.TotalCostUSD)
				acc.EndDetail(e)
			},
			OnMaxTurns: acc.MaxTurnsReached,
			Adapter:    adapter,
			Credential: r.host.CredentialFor(s.JobID),
			StateDir:   r.host.StateDir(s.JobID),
			Timeout:    agentTimeout,
			DenyRead:   r.host.DenyRead(),
			// On a worker: the job's clone .git (read-only git) and its
			// report evidence (spec §4.1 items 2 and 5).
			ReadOnly:       r.host.ReadOnly,
			AllowedDomains: r.host.AllowedDomains,
			DenyWrite:      r.host.DenyWrite,

			GrokBypassPermissions: s.Caps.GrokBypassPermissions,
			GrokTools:             s.Caps.GrokTools,
		}
		// Claude and agy resume by the id from the last result, never by
		// "most recent session": iteration 2+ passes the stored id.
		if resume && session == "" && adapter.ResumesByID() {
			opts.ResumeSession = acc.LastSession()
			if opts.ResumeSession == "" {
				return nil, nil, fmt.Errorf("agent (%s) reported no session id to resume", adapter.Name())
			}
		}
		if r.isTestRun() {
			opts.Prompt = BuildTestPrompt(label, handoff, s.DefectsRoot, sandbox, wts, opts)
			opts.ContinuePrompt = testContinuePrompt
			opts.VerifyCommands = nil
			opts.AlreadyFailing = nil
		}
		if r.secret != "" {
			opts.Secret = r.secret
			opts.ExtraEnv["DD_TEST_ACCOUNT_PASSWORD"] = r.secret
		}
		return git.StartCodingAgent(handoff, label, s.DefectsRoot, wts, sandbox, onStdout, onStderr, opts)
	}

	// Running reaches the job record before the child exists.
	spawnLine := fmt.Sprintf(
		"spawning batch fix: %s — %s sandbox=%s cwd=%s (%d worktree(s))",
		bin, s.BatchID, sandbox, handoff, len(wts),
	)
	if r.isTestRun() {
		spawnLine = fmt.Sprintf(
			"spawning test run: %s — %s sandbox=%s cwd=%s (%d worktree(s))",
			bin, label, sandbox, handoff, len(wts),
		)
	}
	lines := []LogLine{guarded(spawnLine, "info")}
	for _, w := range wts {
		if w.Branch != "" {
			lines = append(lines, guarded(fmt.Sprintf("worktree %s → %s (%s)", w.Repo, w.WorktreeAbs, w.Branch), "info"))
		} else {
			lines = append(lines, guarded(fmt.Sprintf("worktree %s → %s (detached %s)", w.Repo, w.WorktreeAbs, diffBaseByRepo[w.Repo]), "info"))
		}
	}
	if err := r.emit(Event{Kind: EventStatus, Status: &Status{Phase: PhaseRunning, DiffBase: diffBaseByRepo}, Lines: lines}); err != nil {
		return stopOn(err)
	}
	if s.TestRun != nil && s.TestRun.SecretEnv != "" {
		r.secret = os.Getenv(s.TestRun.SecretEnv)
	}
	if r.sink.Cancelled() {
		return Outcome{}, nil
	}

	cmd, flush, err := startIter(false, s.ResumeSession)
	if err != nil {
		return Outcome{}, err
	}
	_ = r.status(PhaseStarted)
	// The wall clock starts here — at iter-1 Start, not at Start. Baseline is
	// 20 min per command on its own clock and must not eat the agent budget.
	loopStart := time.Now()
	maxIters := s.Caps.AgentMaxIters
	maxWall := time.Duration(s.Caps.AgentMaxWallSec) * time.Second
	if r.hooks.AfterStart != nil {
		r.hooks.AfterStart()
	}
	proc := &Proc{Cmd: cmd}
	if !r.sink.Track(proc) {
		abandonStarted(cmd, flush)
		return Outcome{}, nil
	}
	_ = r.status(PhaseLaunched)

	go func() {
		// endLoop tells the orchestrator no further child can start. It does
		// NOT span verify and harvest: a delete may proceed while they run.
		var closeLoopOnce sync.Once
		endLoop := func() {
			closeLoopOnce.Do(func() { _ = r.status(PhaseLoopEnded) })
		}
		defer func() {
			endLoop()
			_ = r.status(PhaseExited)
		}()

		// Driver loop: completion is the artifacts BRIEF.md demands, not the
		// child's exit code (K6). A non-zero exit carrying max_turns_reached is
		// turn exhaustion and resumes; anything else non-zero is a real failure.
		curProc, curFlush := proc, flush
		iter := 1
		stoppedBy, stopReason := "", ""
		var missing, prevMissing []string
		prevFP := ""
		var exitErr error
		cutoff := r.harvestCutoff()
		maxCost := s.Caps.AgentMaxCostUSD
		for {
			exitErr = curProc.Reap()
			if curFlush != nil {
				curFlush()
			}
			sawMaxTurns := acc.TakeMaxTurns()
			// A CLI that flags its own failure (Claude is_error, agy ERROR)
			// fails the iteration even on exit 0. Grok never sets it.
			// The flag is consumed every iteration so a turn cap's is_error
			// never carries into the next one.
			if msg, failed := acc.TakeError(); failed && exitErr == nil && !sawMaxTurns {
				exitErr = fmt.Errorf("agent (%s) reported an error: %s", adapter.Name(), msg)
			}
			r.sink.Untrack(curProc)
			if r.sink.Cancelled() {
				return
			}

			complete, miss := r.complete(cutoff)
			missing = miss
			reason, blocked := BlockedReason(handoff, cutoff, loopStart)
			fp := ProgressFingerprint(handoff, wts)
			noProgress := iter >= 2 && fp == prevFP && slices.Equal(missing, prevMissing)
			prevFP, prevMissing = fp, missing
			stoppedBy = NextLoopStop(LoopState{
				Iter: iter, MaxIters: maxIters,
				Elapsed: time.Since(loopStart), MaxWall: maxWall,
				ExitErr: exitErr, SawMaxTurns: sawMaxTurns, Complete: complete,
				Blocked: blocked, NoProgress: noProgress,
				CostUSD: acc.CostUSD(), MaxCostUSD: maxCost,
			})
			if stoppedBy == StoppedByBlocked {
				stopReason = reason
			}

			// Recorded after every iteration so a killed loop still leaves its
			// cost and stop reasons on disk.
			rec := runRecord(iter, stoppedBy, missing)
			rec.StopReason = stopReason
			lines := []LogLine{guarded(LoopIterationLine(priorIters+iter, exitErr, sawMaxTurns, complete, stoppedBy, missing), LoopLevel(stoppedBy))}
			switch stoppedBy {
			case StoppedByBlocked:
				lines = append(lines, guarded("blocked: "+stopReason, "warn"))
			case StoppedByBudget:
				lines = append(lines, guarded(fmt.Sprintf("cost $%.2f reached the job's cap of $%.2f", acc.CostUSD(), maxCost), "warn"))
			}
			_ = r.emit(Event{Kind: EventAgentRun, AgentRun: rec, Lines: lines})
			if stoppedBy != "" {
				break
			}

			iter++
			if r.hooks.BeforeNext != nil {
				r.hooks.BeforeNext()
			}
			if r.sink.Cancelled() {
				return
			}
			nextCmd, nextFlush, nerr := startIter(true, "")
			if nerr != nil {
				exitErr = nerr
				stoppedBy = StoppedByExit
				_ = r.emit(Event{Kind: EventAgentRun, AgentRun: runRecord(iter, stoppedBy, missing), Lines: []LogLine{
					guarded("agent iteration "+strconv.Itoa(priorIters+iter)+" failed to start: "+nerr.Error(), "error"),
				}})
				break
			}
			next := &Proc{Cmd: nextCmd}
			if !r.sink.Track(next) {
				abandonStarted(nextCmd, nextFlush)
				return
			}
			curProc, curFlush = next, nextFlush
		}

		// The loop is over; no further child will start.
		endLoop()

		failed := stoppedBy == StoppedByExit
		exit := &Status{Phase: PhaseAgentExit, Failed: failed}
		var exitLines []LogLine
		if failed {
			msg := FormatAgentExit(adapter, exitErr)
			if last := acc.LastError(); last != "" && adapter.Name() != agentadapter.Grok && !strings.Contains(msg, last) {
				msg += ": " + last
			}
			if reason := acc.AuthReason(); reason != "" {
				msg = "agent_auth: " + reason
			}
			exit.Error = msg
			exitLines = []LogLine{guarded(msg, "error")}
		}
		if err := r.emit(Event{Kind: EventStatus, Status: exit, Lines: exitLines}); err != nil {
			return
		}
		fin := loopFinish{
			handoff: handoff, failed: failed, wts: wts, diffBase: diffBaseByRepo,
			verifyEnv: provisioned.VerifyEnv, realByRepo: realByRepo, gitDirByRepo: gitDirByRepo,
			baseline: baseline,
		}
		if r.isTestRun() {
			r.afterTestLoop(fin, spawnLog)
			if failed {
				_ = r.emit(Event{Kind: EventDone, Done: &Done{State: "failed"}})
			}
			return
		}
		r.afterFixLoop(fin, spawnLog)
	}()
	return Outcome{Launched: true}, nil
}

// afterFixLoop is verify, diff hygiene, review and harvest for a batch fix.
func (r *run) afterFixLoop(fin loopFinish, log func(line, level string)) {
	if r.sink.Gone() {
		return
	}
	s := r.spec
	harvest := &FixHarvest{}
	if !fin.failed {
		vrun := RunVerification(r.ctx, s.App.VerifyCommands, fin.wts, fin.handoff, s.App.RepoEntries, fin.verifyEnv, fin.realByRepo, log, s.StripEnv)
		hygiene := MeasureDiffHygiene(fin.wts, fin.diffBase, log)
		verdict := JudgeVerification(&vrun, fin.baseline)
		level := "info"
		if !verdict.Ok {
			level = "warn"
		}
		summary := "verify: " + SummarizeVerdict(verdict)
		// D6: commands dropped by the host's policy are a skip, never a
		// pass. The run records why and resolves nothing.
		skipped := s.App.VerifySkipped != ""
		if skipped {
			vrun.Ran, vrun.Ok, vrun.SkippedReason = false, false, s.App.VerifySkipped
			summary = fmt.Sprintf("verify: skipped by %s (%d command(s) configured)", s.App.VerifySkipped, s.App.VerifySkippedCommands)
			level = "warn"
		}
		cp := vrun
		h := hygiene
		_ = r.emit(Event{Kind: EventVerification, Verification: &cp, DiffHygiene: &h, Baseline: fin.baseline, Lines: []LogLine{
			guarded(summary, level),
			guarded(r.host.LogSource()+" process exited OK — reviewing, then harvesting fix-evidence…", "info"),
		}})
		harvest.Verification = &vrun
		harvest.AllowResolve = !skipped
	}
	if r.sink.Cancelled() {
		return
	}
	// Review sits between hygiene and harvest. A failed implementer records a
	// skip and does not spawn.
	rev, reviewStopped := r.review(fin)
	if reviewStopped {
		return
	}
	if !fin.failed {
		harvest.Review = rev
	}
	if r.sink.Cancelled() {
		return
	}
	collected := CollectFixHarvest(fin.handoff, s.DefectIDs, s.HarvestIDs, r.harvestCutoff(), r.importCutoff())
	harvest.Defects = collected.Defects
	harvest.Unfixed = collected.Unfixed
	harvest.NotesUnreadable = collected.NotesUnreadable
	if err := r.emit(Event{Kind: EventHarvest, Harvest: harvest}); err != nil {
		return
	}
	if fin.failed {
		_ = r.emit(Event{Kind: EventDone, Done: &Done{State: "failed"}})
		return
	}
	_ = r.emit(Event{Kind: EventDone, Done: &Done{State: "completed", Line: "batch complete — review worktrees; merge when satisfied"}})
}

// afterTestLoop is diff hygiene and the results harvest for a test run.
func (r *run) afterTestLoop(fin loopFinish, log func(line, level string)) {
	if r.sink.Gone() {
		return
	}
	hygiene := MeasureDiffHygiene(fin.wts, fin.diffBase, log)
	changed := HygieneChanged(hygiene)
	if !changed {
		changed = WorktreeDirty(fin.wts)
	}
	var lines []LogLine
	if changed {
		lines = append(lines, guarded("worktree changes are never shipped from a test job", "warn"))
	}
	h := hygiene
	_ = r.emit(Event{Kind: EventDiffHygiene, DiffHygiene: &h, Lines: lines})
	if r.sink.Cancelled() {
		return
	}
	var steps []RunStep
	if r.spec.TestRun != nil {
		steps = r.spec.TestRun.Run.Steps
	}
	results := CollectTestResults(fin.handoff, steps, r.harvestCutoff(), r.importCutoff(), r.secret)
	results.Finish = !fin.failed
	_ = r.emit(Event{Kind: EventTestResults, TestResults: &results})
	if fin.failed {
		return
	}
	_ = r.emit(Event{Kind: EventDone, Done: &Done{State: "completed"}})
}

// CollectedFix is a batch fix's handoff as the harvest reads it.
type CollectedFix struct {
	Defects         []HarvestedDefect
	Unfixed         map[string]bool
	NotesUnreadable bool
}

// CollectFixHarvest reads each harvestable defect's fix images and note.
// defectIDs is every defect in the job (the UNFIXED markers are read for
// all); harvestIDs are the ones whose directories may be read. Reads are
// confined to real files under the handoff (ReadRegularFile).
func CollectFixHarvest(handoff string, defectIDs, harvestIDs []string, cutoff, imgCutoff time.Time) CollectedFix {
	fixRoot := filepath.Join(handoff, "fix-evidence")
	notesRoot := filepath.Join(handoff, "fix-notes")
	out := CollectedFix{}
	out.Unfixed, out.NotesUnreadable = NotesForHarvest(handoff, defectIDs, cutoff)
	for _, defectID := range harvestIDs {
		// The orchestrator sends checked ids; a path-shaped one is refused
		// here too, so a spec from the wire cannot steer a directory read.
		if !safePathID(defectID) {
			continue
		}
		d := HarvestedDefect{DefectID: defectID}
		imgDir := filepath.Join(fixRoot, defectID)
		if ents, err := os.ReadDir(imgDir); err == nil && IsRealDir(imgDir) {
			for _, e := range ents {
				name := e.Name()
				// Shared with ArtifactsComplete so the driver loop can never
				// stop on a file this harvest would then skip. DirEntry.IsDir
				// follows the link, so a symlinked directory would read as a
				// file here; ReadRegularFile is the real gate.
				if !HarvestableImageName(name, e.IsDir()) {
					continue
				}
				data, tooLarge, ok := ReadRegularFile(handoff, filepath.Join(imgDir, name), imgCutoff)
				if tooLarge {
					d.OversizedImages = append(d.OversizedImages, name)
					continue
				}
				if !ok {
					continue
				}
				d.Images = append(d.Images, HarvestFile{Filename: name, Data: data})
			}
		}
		d.Note, d.OversizedNotes = ReadFirstNote(
			handoff,
			cutoff,
			filepath.Join(notesRoot, defectID+".md"),
			filepath.Join(notesRoot, defectID+".txt"),
			filepath.Join(fixRoot, defectID, "NOTES.md"),
			filepath.Join(fixRoot, defectID, "resolution.md"),
		)
		out.Defects = append(out.Defects, d)
	}
	return out
}

// review is the batch fix's independent pass. stopped is true when the job was
// stopped or deleted and harvest must not run.
func (r *run) review(fin loopFinish) (rev *ReviewRun, stopped bool) {
	if r.sink.Cancelled() {
		return nil, true
	}
	reviewLog := func(line, level string) {
		_ = r.emit(Event{Kind: EventLog, Lines: []LogLine{guarded(line, level)}})
	}
	save := func(rev *ReviewRun) {
		_ = r.emit(Event{Kind: EventReview, Review: CloneReview(rev)})
	}
	if fin.failed {
		rev = &ReviewRun{SkippedReason: "implementer failed"}
		save(rev)
		return rev, false
	}
	// No diff: nothing in this job may resolve, notes and REVIEW=0 included.
	// A placeholder fix-notes file is not a change to the tree.
	if !fin.reviewInputs() {
		rev = &ReviewRun{
			Required:      true,
			SkippedReason: "no diff",
			Error:         "nothing to review",
		}
		reviewLog("review: skipped (no diff)", "info")
		save(rev)
		return rev, false
	}
	if r.spec.Caps.ReviewOff {
		rev = &ReviewRun{SkippedReason: "REVIEW=0"}
		reviewLog("review: skipped (REVIEW=0)", "warn")
		save(rev)
		return rev, false
	}

	rev = &ReviewRun{Required: true}
	_ = r.status(PhaseReviewBegin)
	defer func() { _ = r.status(PhaseReviewEnd) }()
	mio := ManifestIO{}
	if r.hooks.Manifest != nil {
		mio = r.hooks.Manifest()
	}
	before, err := CaptureWorktreeManifests(fin.wts, fin.realByRepo, fin.gitDirByRepo, mio)
	if err != nil {
		rev.Error = "manifest: " + err.Error()
		reviewLog("review: "+rev.Error, "error")
		save(rev)
		return rev, false
	}
	defer func() {
		// The executor does not put the tree back. A difference means the
		// reviewer (or something else) wrote, and the fix must not resolve.
		if r.sink.Gone() {
			return
		}
		msg := manifestDiff(before, fin.wts, fin.realByRepo, fin.gitDirByRepo, mio)
		if msg == "" {
			return
		}
		rev.OK = false
		if rev.Error != "" {
			rev.Error += "; " + msg
		} else {
			rev.Error = msg
		}
		reviewLog("review: "+msg, "error")
		save(rev)
	}()

	ids := append([]string(nil), r.spec.DefectIDs...)
	brief := filepath.Join(fin.handoff, "BRIEF.md")
	maxTurns := r.spec.Caps.ReviewMaxTurns
	wall := time.Duration(r.spec.Caps.ReviewMaxWallSec) * time.Second
	for _, lens := range ReviewLenses(fin.wts) {
		if r.sink.Cancelled() {
			return rev, true
		}
		ran, stopped := r.reviewLens(fin, rev, lens, brief, ids, maxTurns, wall, reviewLog)
		if stopped {
			return rev, true
		}
		if ran {
			save(rev)
			if r.hooks.LensFinished != nil {
				r.hooks.LensFinished(lens)
			}
		}
	}
	rev.Finish()
	line, level := ReviewSummary(rev)
	reviewLog(line, level)
	save(rev)
	return rev, false
}

// reviewLens spawns one lens. ran is false when the lens was not started
// because the job was already gone. stopped means do not start another lens.
func (r *run) reviewLens(fin loopFinish, rev *ReviewRun, lens, brief string, ids []string, maxTurns int, wall time.Duration, reviewLog func(line, level string)) (ran, stopped bool) {
	if r.sink.Cancelled() {
		return false, true
	}
	dir := filepath.Join(fin.handoff, "review", lens)
	row := ReviewLens{Lens: lens, Findings: []ReviewFinding{}}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		row.Error = lens + ": " + err.Error()
		rev.Lenses = append(rev.Lenses, row)
		return true, false
	}
	md := BuildReviewMD(lens, brief, fin.wts, fin.diffBase, ids)
	if err := os.WriteFile(filepath.Join(dir, "REVIEW.md"), []byte(md), 0o644); err != nil {
		row.Error = lens + ": " + err.Error()
		rev.Lenses = append(rev.Lenses, row)
		return true, false
	}
	reviewLog("review: spawn "+lens, "info")

	output := func(line, level string) {
		_ = r.emit(Event{Kind: EventLog, Lines: []LogLine{{Text: line, Level: level, Source: r.host.LogSource(), Guarded: true}}})
	}
	var installed *Proc
	adapter := r.host.AgentAdapter()
	res, err := git.StartReviewAgent(git.ReviewAgentOpts{
		Adapter:    adapter,
		Worktrees:  reviewWorktrees(fin.wts),
		Credential: r.host.CredentialFor(r.spec.JobID),
		StateDir:   r.host.StateDir(r.spec.JobID),
		DenyRead:   r.host.DenyRead(),
		ReadOnly:   append(ReviewReadOnly(fin.handoff), r.host.ReadOnly...),
		DenyWrite:  r.host.DenyWrite,
		Cwd:        dir,
		Prompt:     reviewAgentPrompt,
		MaxTurns:   maxTurns,
		TextLimit:  HarvestFileLimit,
		Wall:       wall,
		StripEnv:   r.spec.StripEnv,
		OnStdout: func(line string) {
			output(line, "info")
		},
		OnStderr: func(line string) {
			output(line, "warn")
		},
		Register: func(cmd *exec.Cmd) func() error {
			proc := &Proc{Cmd: cmd, Group: true}
			if !r.sink.Track(proc) {
				git.KillReviewProcess(cmd)
				return proc.Reap
			}
			installed = proc
			if r.hooks.LensStarted != nil {
				r.hooks.LensStarted(lens)
			}
			return proc.Reap
		},
	})
	if installed != nil {
		r.sink.Untrack(installed)
	}
	if r.sink.Cancelled() {
		return false, true
	}
	if err != nil {
		if msg, ok := sandboxSocketError(err.Error()); ok {
			row.Error = msg
			reviewLog("review: "+lens+" "+msg, "error")
		} else {
			row.Error = lens + ": " + err.Error()
			reviewLog("review: "+row.Error, "error")
		}
		rev.Lenses = append(rev.Lenses, row)
		return true, false
	}
	row.Turns = res.End.NumTurns
	row.CostUSD = res.End.TotalCostUSD
	row.SessionID = res.End.SessionID
	// A wall kill is deadline even if stderr also mentions the runtime socket.
	// The socket phrase is the cause only when the process exited on its own.
	if res.Deadline {
		row.Error = "deadline"
		reviewLog("review: "+lens+" deadline", "error")
	} else if msg, ok := sandboxSocketError(res.Stderr); ok {
		row.Error = msg
		reviewLog("review: "+lens+" "+msg, "error")
	} else if res.End.AuthFailed {
		row.OK = false
		row.Error = "agent_auth: " + res.End.Message
		reviewLog("review: "+lens+": "+row.Error, "error")
	} else if tool, denied := adapter.Denied(res.ToolEvents, res.End); denied {
		// The permission gate cancelled the command and ended the run.
		// That is not a missing payload: name the tool and keep the fix blocked.
		row.OK = false
		row.Error = "tool-denied: " + tool
		reviewLog("review: "+lens+": "+row.Error, "error")
	} else {
		parsed := ParseLensText(lens, res.Text, res.TooLarge, ids)
		row.OK = parsed.OK
		row.Error = parsed.Error
		row.Findings = parsed.Findings
		if row.OK && !adapter.ReviewEvidence(res.ToolEvents, reviewWorktrees(fin.wts)) {
			// Valid JSON is not proof the model saw the diff.
			row.OK = false
			row.Error = "no-read-evidence"
		}
		if row.Error != "" {
			msg := row.Error
			if !strings.HasPrefix(msg, lens) {
				msg = lens + ": " + msg
			}
			reviewLog("review: "+msg, "warn")
		}
	}
	rev.Lenses = append(rev.Lenses, row)
	return true, false
}

// safePathID is a defect id that names one directory under the handoff.
func safePathID(id string) bool {
	return id != "" && !strings.Contains(id, "..") && filepath.Base(id) == id && !strings.ContainsAny(id, "/\\\x00")
}
