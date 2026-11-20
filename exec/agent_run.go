package exec

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/more-than-code/defect-drainer-worker/agentadapter"
)

// AgentRun records what the driver loop did. Completion is measured in
// artifacts on disk, not in the child's exit code or its prose (K6), so this
// is where an operator sees why the loop stopped.
type AgentRun struct {
	Iterations           int    `json:"iterations"`
	CompletedByArtifacts bool   `json:"completedByArtifacts"`
	StoppedBy            string `json:"stoppedBy,omitempty"`
	// StopReason is the agent's own words for a "blocked" stop (its
	// BLOCKED: line, at most 200 characters). Additive.
	StopReason   string   `json:"stopReason,omitempty"`
	StopReasons  []string `json:"stopReasons,omitempty"`
	Turns        int      `json:"turns,omitempty"`
	TotalCostUSD float64  `json:"totalCostUsd,omitempty"`
	SessionID    string   `json:"sessionId,omitempty"`
	Missing      []string `json:"missing,omitempty"`
	// Resumes counts operator Resumes. Iterations, turns and cost above are
	// cumulative across every run of the job, not just the latest.
	Resumes int `json:"resumes,omitempty"`
	// Runner is the agent adapter that ran the job (grok, claude,
	// antigravity). Additive; older jobs have none, which means grok.
	Runner string `json:"runner,omitempty"`
	// Tokens is the cumulative token count from adapters that report no cost
	// (antigravity).
	Tokens int `json:"tokens,omitempty"`
}

// Reasons the loop stopped. Only StoppedByArtifacts means the worker delivered
// what BRIEF.md asked for; the rest are caps and failures.
const (
	StoppedByArtifacts  = "artifacts"
	StoppedByIterations = "iterations"
	StoppedByWall       = "wall"
	StoppedByExit       = "exit"
	StoppedByCancelled  = "cancelled"
	// The agent declared a blocker (a BLOCKED: line in NOTES.md).
	StoppedByBlocked = "blocked"
	// Two iterations in a row changed nothing (ProgressFingerprint) and left
	// the same artifacts missing.
	StoppedByNoProgress = "no_progress"
	// The job's total agent cost reached Caps.AgentMaxCostUSD.
	StoppedByBudget = "budget"
)

// Loop caps. Wall covers only the driver loop — never baseline or verify,
// which are 20 min per command on their own clock (verify.go).
const (
	DefaultAgentMaxIters   = 8
	DefaultAgentMaxWallSec = 2700
)

// ParsePositiveInt falls back to def for empty, non-numeric, and n <= 0 so a
// typo'd env var cannot silently disable a cap.
func ParsePositiveInt(s string, def int) int {
	n, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil || n <= 0 {
		return def
	}
	return n
}

// markerLine reports whether line is exactly "<marker>: <id>" after trimming.
// Full-line exact on purpose: a substring match would let "UNFIXED: DEF-1"
// satisfy DEF-10, and a trailing comment would let the worker decline a defect
// while appearing to explain something else.
func markerLine(line, marker, id string) bool {
	return strings.TrimSpace(line) == marker+": "+id
}

// NotesMarkers scans NOTES.md once for the two loop markers. A missing or
// unreadable NOTES.md yields no markers and ok=false — the loop then cannot be
// complete, which is the fail-closed direction.
func NotesMarkers(handoff string, defectIDs []string, cutoff time.Time) (unfixed, noShot map[string]bool, ok bool) {
	unfixed, noShot = map[string]bool{}, map[string]bool{}
	data, _, ok := ReadRegularFile(handoff, filepath.Join(handoff, "NOTES.md"), cutoff)
	if !ok {
		return unfixed, noShot, false
	}
	for _, line := range strings.Split(string(data), "\n") {
		for _, id := range defectIDs {
			if markerLine(line, "UNFIXED", id) {
				unfixed[id] = true
			}
			if markerLine(line, "NO-SCREENSHOT", id) {
				noShot[id] = true
			}
		}
	}
	return unfixed, noShot, true
}

// NotesForHarvest separates a missing NOTES.md from one the reader refuses.
// Missing means no markers. Present but unreadable (too large, a symlink, a
// hardlink, another device) must not look like an empty decline list.
func NotesForHarvest(handoff string, defectIDs []string, cutoff time.Time) (unfixed map[string]bool, unreadable bool) {
	unfixed, _, ok := NotesMarkers(handoff, defectIDs, cutoff)
	if ok {
		return unfixed, false
	}
	_, err := os.Lstat(filepath.Join(handoff, "NOTES.md"))
	if os.IsNotExist(err) {
		return unfixed, false
	}
	return unfixed, true
}

// HarvestableImageName is the name-level half of harvest's evidence predicate.
// Both CollectFixHarvest and ArtifactsComplete call it so the loop can never
// stop on a file harvest would then skip. ReadRegularFile is the other half and
// each caller applies it to the same path.
func HarvestableImageName(name string, isDir bool) bool {
	if isDir || strings.HasPrefix(name, ".") {
		return false
	}
	return ImageExts[strings.ToLower(filepath.Ext(name))]
}

// hasHarvestableImage mirrors harvest exactly: real directory, name predicate,
// then ReadRegularFile (regular file under the handoff, not a link, inside the
// cutoff and size limit).
func hasHarvestableImage(handoff, imgDir string, cutoff time.Time) bool {
	if !IsRealDir(imgDir) {
		return false
	}
	ents, err := os.ReadDir(imgDir)
	if err != nil {
		return false
	}
	for _, e := range ents {
		// DirEntry.IsDir follows the link, so a symlinked directory reads as a
		// file here; ReadRegularFile is the real gate.
		if !HarvestableImageName(e.Name(), e.IsDir()) {
			continue
		}
		if _, _, ok := ReadRegularFile(handoff, filepath.Join(imgDir, e.Name()), cutoff); ok {
			return true
		}
	}
	return false
}

// ArtifactsComplete decides whether the driver loop may stop. It answers only
// "did the worker deliver the files BRIEF.md demands" — never "may this defect
// resolve". Harvest keeps its own gates: a loop that completes on
// NO-SCREENSHOT or UNFIXED still resolves nothing for those defects.
//
// missing lists what was absent, in defect order, for the job log and for
// agentRun.missing when a cap ends the loop instead.
func ArtifactsComplete(handoff string, defectIDs []string, sandbox string, cutoff time.Time) (bool, []string) {
	if len(defectIDs) == 0 {
		return false, []string{"job has no defect ids"}
	}
	unfixed, noShot, notesOK := NotesMarkers(handoff, defectIDs, cutoff)
	var missing []string
	if !notesOK {
		missing = append(missing, "NOTES.md")
	}
	for _, id := range defectIDs {
		if unfixed[id] {
			continue // declined: no notes and no image required
		}
		noteFile := filepath.Join(handoff, "fix-notes", id+".md")
		if _, _, ok := ReadRegularFile(handoff, noteFile, cutoff); !ok {
			missing = append(missing, "fix-notes/"+id+".md")
		}
		if hasHarvestableImage(handoff, filepath.Join(handoff, "fix-evidence", id), cutoff) {
			continue
		}
		// A strict sandbox genuinely cannot screenshot, so an explicit marker
		// releases the loop. It does not release harvest.
		if sandbox == "strict" && noShot[id] {
			continue
		}
		missing = append(missing, "fix-evidence/"+id+"/<image>")
	}
	return len(missing) == 0, missing
}

// LoopState is what NextLoopStop judges after one iteration.
type LoopState struct {
	Iter, MaxIters   int
	Elapsed, MaxWall time.Duration
	ExitErr          error
	SawMaxTurns      bool
	Complete         bool
	// Blocked: NOTES.md has a fresh BLOCKED: line.
	Blocked bool
	// NoProgress: this iteration's fingerprint and missing list equal the
	// previous iteration's.
	NoProgress bool
	// CostUSD is the job's total so far (Resumes included); MaxCostUSD <= 0
	// disables the budget rule.
	CostUSD, MaxCostUSD float64
}

// NextLoopStop decides whether the driver loop stops, and why. Empty means
// iterate again. First match wins:
//
//	exit → artifacts → blocked → no_progress → budget → wall → iterations
//
// The exit-code rule is the subtle one. Turn exhaustion also exits non-zero
// (verified on Grok Build 1.0.40: exit 1, stopReason "cancelled", plus a
// max_turns_reached event), so breaking on any non-zero exit would treat every
// turn cap as a job failure and never resume — which is the whole point of the
// loop. sawMaxTurns is the positive signal that separates the two.
//
// complete is checked before the other rules: if the worker delivered the
// artifacts on its last legal iteration, that is why the loop stopped, and the
// operator should not see it reported as having run out of budget. The agent's
// own blocker comes next because it says the most; budget precedes the wall
// and iteration caps because it is the money rule.
func NextLoopStop(st LoopState) string {
	switch {
	case st.ExitErr != nil && !st.SawMaxTurns:
		return StoppedByExit
	case st.Complete:
		return StoppedByArtifacts
	case st.Blocked:
		return StoppedByBlocked
	case st.NoProgress:
		return StoppedByNoProgress
	case st.MaxCostUSD > 0 && st.CostUSD >= st.MaxCostUSD:
		return StoppedByBudget
	case st.Elapsed >= st.MaxWall:
		return StoppedByWall
	case st.Iter >= st.MaxIters:
		return StoppedByIterations
	}
	return ""
}

// AgentRunAcc accumulates streaming-json events. OnEnd fires on the exec
// package's stdout copy goroutine while the loop goroutine reads the totals, so
// every field is behind the mutex.
type AgentRunAcc struct {
	mu          sync.Mutex
	stopReasons []string
	turns       int
	costUSD     float64
	sessionID   string
	maxTurns    bool
	tokens      int
	// errMsg / errSet: the CLI flagged its own failure this iteration.
	errMsg string
	errSet bool
	// auth is the latest auth failure's message ("" when none).
	auth string
	// lastErr is the CLI's latest error text, kept for the job error.
	lastErr string
}

// EndDetail records what only some adapters report: tokens, the CLI's own
// error flag and an auth failure. Grok sets none of them.
func (a *AgentRunAcc) EndDetail(e agentadapter.End) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.tokens += e.Tokens
	if e.IsError {
		a.errSet = true
		a.errMsg = e.Message
		a.lastErr = e.Message
	}
	if e.AuthFailed {
		a.auth = e.Message
		if a.auth == "" {
			a.auth = "the CLI could not authenticate"
		}
	}
}

// TakeError returns and clears this iteration's CLI error flag.
func (a *AgentRunAcc) TakeError() (string, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	msg, set := a.errMsg, a.errSet
	a.errMsg, a.errSet = "", false
	return msg, set
}

// LastError is the CLI's latest error text ("" when none).
func (a *AgentRunAcc) LastError() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.lastErr
}

// AuthReason is the auth failure to report as the job reason.
func (a *AgentRunAcc) AuthReason() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.auth
}

// LastSession is the session id to resume (seeded from a prior run).
func (a *AgentRunAcc) LastSession() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.sessionID
}

// Seed carries a prior run's totals into this one, so a Re-run or Resume adds
// to the job's record instead of replacing it.
func (a *AgentRunAcc) Seed(prior *AgentRun) {
	if prior == nil {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.stopReasons = append([]string(nil), prior.StopReasons...)
	a.turns = prior.Turns
	a.costUSD = prior.TotalCostUSD
	a.tokens = prior.Tokens
	a.sessionID = prior.SessionID
}

func (a *AgentRunAcc) End(stopReason, sessionID string, turns int, cost float64) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if stopReason != "" {
		a.stopReasons = append(a.stopReasons, stopReason)
	}
	if sessionID != "" {
		a.sessionID = sessionID
	}
	a.turns += turns
	a.costUSD += cost
}

// MaxTurnsReached latches per iteration; the loop clears it before each start.
func (a *AgentRunAcc) MaxTurnsReached() {
	a.mu.Lock()
	a.maxTurns = true
	a.mu.Unlock()
}

func (a *AgentRunAcc) TakeMaxTurns() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	v := a.maxTurns
	a.maxTurns = false
	return v
}

// CostUSD is the job's total agent cost so far, Resumes included.
func (a *AgentRunAcc) CostUSD() float64 {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.costUSD
}

func (a *AgentRunAcc) Snapshot(iter int, stoppedBy string, missing []string) *AgentRun {
	a.mu.Lock()
	defer a.mu.Unlock()
	run := &AgentRun{
		Iterations:           iter,
		CompletedByArtifacts: stoppedBy == StoppedByArtifacts,
		StoppedBy:            stoppedBy,
		Turns:                a.turns,
		TotalCostUSD:         a.costUSD,
		SessionID:            a.sessionID,
		Tokens:               a.tokens,
	}
	if len(a.stopReasons) > 0 {
		run.StopReasons = append([]string(nil), a.stopReasons...)
	}
	if len(missing) > 0 {
		run.Missing = append([]string(nil), missing...)
	}
	return run
}

// LoopIterationLine is the operator-facing summary of one iteration. It always
// says what the child did and what, if anything, is still missing, so a capped
// run reads as "ran out of budget with X absent" rather than just "completed".
func LoopIterationLine(iter int, exitErr error, sawMaxTurns, complete bool, stoppedBy string, missing []string) string {
	parts := []string{"agent iteration " + strconv.Itoa(iter)}
	switch {
	case sawMaxTurns:
		parts = append(parts, "hit --max-turns")
	case exitErr != nil:
		parts = append(parts, "exited non-zero")
	default:
		parts = append(parts, "exited 0")
	}
	if complete {
		parts = append(parts, "artifacts complete")
	} else {
		miss := "artifacts incomplete"
		if len(missing) > 0 {
			miss += " (missing: " + strings.Join(missing, ", ") + ")"
		}
		parts = append(parts, miss)
	}
	switch stoppedBy {
	case "":
		parts = append(parts, "resuming")
	case StoppedByArtifacts:
		parts = append(parts, "stopping: required files exist")
	case StoppedByIterations:
		parts = append(parts, "stopping: iteration cap")
	case StoppedByWall:
		parts = append(parts, "stopping: wall clock")
	case StoppedByExit:
		parts = append(parts, "stopping: agent failed")
	case StoppedByBlocked:
		parts = append(parts, "stopping: agent declared a blocker")
	case StoppedByNoProgress:
		parts = append(parts, "stopping: no progress in two iterations")
	case StoppedByBudget:
		parts = append(parts, "stopping: cost cap reached")
	}
	return strings.Join(parts, " — ")
}

func LoopLevel(stoppedBy string) string {
	switch stoppedBy {
	case StoppedByExit:
		return "error"
	case StoppedByIterations, StoppedByWall, StoppedByBlocked, StoppedByNoProgress, StoppedByBudget:
		return "warn"
	default:
		return "info"
	}
}
