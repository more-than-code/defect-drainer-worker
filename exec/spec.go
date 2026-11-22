package exec

import (
	"errors"

	"github.com/more-than-code/defect-drainer-worker/git"
)

// The seam between the orchestrator (internal/jobs) and this executor
// (docs/worker-agent.md §3.1). Everything here is plain data that survives a
// JSON round trip (spec_test.go), because in W2 it crosses a WebSocket.
//
//   - JobSpec goes in: a snapshot of what one run of one job needs.
//   - Event comes out, in order: what happened, for the orchestrator to apply
//     (job record, log, DB writes).
//   - Control goes in: cancel, resume.

// Job kinds.
const (
	KindBatchFix = "batch_fix"
	KindTestRun  = "test_run"
)

// Spawn modes. A fresh run measures baseline and diff base; a rerun or resume
// reuses the first run's and adds to its agent record.
const (
	ModeFresh  = "fresh"
	ModeRerun  = "rerun"
	ModeResume = "resume"
	// ModeVerifySubmitted verifies a ref submitted from outside DD (worker
	// protocol v1 §16): baseline, verify, diff hygiene, review and harvest,
	// with no coding agent and no branch of DD's own. Batch fix only.
	ModeVerifySubmitted = "verify_submitted"
)

// SubmittedRef is one repo's submitted code: a branch name or a commit id.
type SubmittedRef struct {
	Repo string `json:"repo"`
	Ref  string `json:"ref"`
}

// JobSpec is one run of one job.
type JobSpec struct {
	JobID     string `json:"jobId"`
	Kind      string `json:"kind"`
	ProjectID string `json:"projectId"`
	// Label names the job in log lines, the sandbox profile and the prompt:
	// the batch id, or the test run id for a test job.
	Label   string `json:"label"`
	BatchID string `json:"batchId,omitempty"`
	Mode    string `json:"mode"`
	// ResumeSession is the agent session iteration 1 continues (Resume).
	ResumeSession string `json:"resumeSession,omitempty"`

	// Handoff is the job's working directory on the host; DefectsRoot is the
	// inventory root the brief points at.
	Handoff     string                `json:"handoff"`
	DefectsRoot string                `json:"defectsRoot"`
	Worktrees   []git.WorktreeBinding `json:"worktrees"`
	DefectIDs   []string              `json:"defectIds,omitempty"`
	// HarvestIDs are the defect ids whose fix-evidence/ and fix-notes/ the
	// harvest reads: DefectIDs that passed the orchestrator's id check.
	HarvestIDs []string `json:"harvestIds,omitempty"`

	// The first run's measurements, reused by a rerun or resume.
	DiffBase      map[string]string `json:"diffBase,omitempty"`
	Baseline      *VerificationRun  `json:"baseline,omitempty"`
	PriorAgentRun *AgentRun         `json:"priorAgentRun,omitempty"`

	// CreatedAt and RunStartedAt (RFC 3339) are the harvest cutoffs: files
	// older than the job do not count, and fix images older than this run are
	// not imported.
	CreatedAt    string `json:"createdAt,omitempty"`
	RunStartedAt string `json:"runStartedAt,omitempty"`

	Project ProjectSettings `json:"project"`
	Fix     *FixSpec        `json:"fix,omitempty"`
	TestRun *TestRunSpec    `json:"testRun,omitempty"`

	// StripEnv are host variables removed from every child (every app's
	// test-account secret names), read when the run starts.
	StripEnv []string `json:"stripEnv,omitempty"`
	Caps     Caps     `json:"caps"`

	// Adapter and Model are the app's agent setting (worker spec Q4). Empty
	// Adapter means "the host's first ready adapter". Resume and Rerun carry
	// the adapter that started the job. The local transport ignores Adapter
	// and uses the host's DEFECT_DRAINER_AGENT_RUNNER (worker protocol v1
	// §8.6). Model reaches every agent Task of the job, implementer and
	// reviewers, on both transports; empty keeps the CLI's default. Claude
	// passes it as --model; antigravity takes its model from the worker's
	// per-job adapter instead, and grok ignores it.
	Adapter string `json:"adapter,omitempty"`
	Model   string `json:"model,omitempty"`

	// SubmittedRefs are the refs a ModeVerifySubmitted run checks, one per
	// worktree repo. BaselineWorktrees are derived by the worker, never sent
	// by a server: detached worktrees at each repo's base, where the
	// baseline runs, since Worktrees hold the submitted code.
	SubmittedRefs     []SubmittedRef        `json:"submittedRefs,omitempty"`
	BaselineWorktrees []git.WorktreeBinding `json:"baselineWorktrees,omitempty"`
}

// VerifySubmitted reports whether s verifies a submitted ref.
func (s JobSpec) VerifySubmitted() bool { return s.Mode == ModeVerifySubmitted }

// ProjectSettings are the project's agent settings, normalised by the orchestrator.
type ProjectSettings struct {
	Sandbox         string          `json:"sandbox"`
	Toolchain       string          `json:"toolchain"`
	SimulatorWrites bool            `json:"simulatorWrites,omitempty"`
	MaxTurns        int             `json:"maxTurns,omitempty"`
	VerifyCommands  []VerifyCommand `json:"verifyCommands,omitempty"`
	RepoEntries     []RepoEntry     `json:"repoEntries,omitempty"`
	// VerifySkipped is why VerifyCommands were dropped before the run
	// ("host policy"), and VerifySkippedCommands how many there were. Set
	// only by a worker at offer; a run with VerifySkipped never resolves a
	// defect (spec D6).
	VerifySkipped         string `json:"verifySkipped,omitempty"`
	VerifySkippedCommands int    `json:"verifySkippedCommands,omitempty"`

	// Worker browser (additive, browser.v1). The console's per-app requests;
	// a worker applies only what its policy also allows and clears Browser
	// when it does not, so the brief and prompt see the effective value.
	//   - Browser: after-fix screenshots with the host browser (dd-shot).
	//   - PreviewCmd: the app's dev server, run in the first worktree.
	//   - PreviewURL: where it answers, loopback only (http://127.0.0.1:5173).
	//   - AgentHosts: network hosts requested for the agent's sandbox; empty
	//     means the host policy's list, as before.
	Browser    bool     `json:"browser,omitempty"`
	PreviewCmd string   `json:"previewCmd,omitempty"`
	PreviewURL string   `json:"previewUrl,omitempty"`
	AgentHosts []string `json:"agentHosts,omitempty"`

	// Job stack (additive, job_stack.v1; S1–S9). The app's
	// job stack, booted by dd-stack inside the agent's sandbox:
	//   - JobStack: at most 4, started in order (stack.go).
	//   - PreviewEnv: env for the command dd-stack runs (dd-shot's preview,
	//     or the agent's), with the same placeholders.
	//   - PreviewTimeoutSec: the whole stack's bound and dd-shot's wait for
	//     the preview URL; 0 means 90, else 30–300.
	// A worker keeps them only on an effective browser job; otherwise it
	// clears them with one job-log note.
	JobStack          []JobStackService `json:"jobStack,omitempty"`
	PreviewEnv        map[string]string `json:"previewEnv,omitempty"`
	PreviewTimeoutSec int               `json:"previewTimeoutSec,omitempty"`
	// JobStackSignIn (S10) signs dd-shot's browser in as the stack's seed
	// user before its screenshots. It needs JobStack and PreviewURL.
	JobStackSignIn *StackSignIn `json:"jobStackSignIn,omitempty"`
}

// VerifySkippedHostPolicy is VerifySkipped when a worker's policy has
// allow_verify_commands off.
const VerifySkippedHostPolicy = "host policy"

// FixSpec is what a batch fix's brief is built from.
type FixSpec struct {
	Batch   Batch    `json:"batch"`
	Defects []Defect `json:"defects"`
	// Evidence lists the defects' evidence files. Local runs read them under
	// DefectsRoot; SHA256 is set when the file travels as a blob (W2).
	Evidence []EvidenceRef `json:"evidence,omitempty"`
}

// EvidenceRef is one evidence file, by inventory-relative path.
type EvidenceRef struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256,omitempty"`
}

// TestRunSpec is the test run a test job executes.
type TestRunSpec struct {
	RunID string  `json:"runId"`
	Run   TestRun `json:"run"`
	// Loaded is false when the orchestrator could not read the run. The
	// loop then never stops on artifacts ("test run missing").
	Loaded bool `json:"loaded"`
	// SecretEnv names the host variable holding the account password, when
	// the run's account has an env:NAME reference. The value never travels.
	SecretEnv string `json:"secretEnv,omitempty"`
}

// Caps are the loop and review budgets.
type Caps struct {
	AgentMaxIters    int  `json:"agentMaxIters"`
	AgentMaxWallSec  int  `json:"agentMaxWallSec"`
	ReviewOff        bool `json:"reviewOff,omitempty"`
	ReviewMaxTurns   int  `json:"reviewMaxTurns"`
	ReviewMaxWallSec int  `json:"reviewMaxWallSec"`
	// AgentMaxCostUSD stops the loop once the job's total agent cost (every
	// run of it, Resumes included) reaches it. Zero or less: no cost rule.
	AgentMaxCostUSD float64 `json:"agentMaxCostUsd,omitempty"`

	// The host's grok knobs, read by the orchestrator so this package reads
	// no DD setting (W1a deviation Q1, moved in W2b). AgentMaxTurnsArg is the
	// turn-cap override passed to grok verbatim when the app sets no cap. A
	// remote spec leaves all three empty: a worker's own policy decides.
	AgentMaxTurnsArg      string `json:"agentMaxTurnsArg,omitempty"`
	GrokBypassPermissions bool   `json:"grokBypassPermissions,omitempty"`
	GrokTools             string `json:"grokTools,omitempty"`
}

// Event kinds.
const (
	EventLog          = "log"
	EventStatus       = "status"
	EventAgentRun     = "agent_run"
	EventBaseline     = "baseline"
	EventVerification = "verification"
	EventDiffHygiene  = "diff_hygiene"
	EventReview       = "review"
	EventHarvest      = "harvest"
	EventTestResults  = "test_results"
	EventDone         = "done"
)

// Status phases, in the order a run reaches them. Each is a point where the
// orchestrator updates the job or answers (see Sink.Apply).
const (
	// PhaseHandoff: the handoff files are written. The orchestrator gates the
	// adapter here.
	PhaseHandoff = "handoff"
	// PhasePrepared: the app's settings line is logged.
	PhasePrepared = "prepared"
	// PhaseReady: the agent is signed in. A host lease may park the job here.
	PhaseReady = "ready"
	// PhaseRunning: about to start the first agent iteration. Carries the
	// diff base and the spawn lines.
	PhaseRunning = "running"
	// PhaseStarted: the first child exists.
	PhaseStarted = "started"
	// PhaseLaunched: the first child is tracked; the driver loop is running.
	PhaseLaunched = "launched"
	// PhaseLoopEnded: no further child will start.
	PhaseLoopEnded = "loop_ended"
	// PhaseAgentExit: the loop's outcome. Error is set when the agent failed.
	PhaseAgentExit = "agent_exit"
	// PhaseReviewBegin / PhaseReviewEnd bracket the reviewer's read-only
	// window (manifest capture to compare).
	PhaseReviewBegin = "review_begin"
	PhaseReviewEnd   = "review_end"
	// PhaseExited: the run is over on this host.
	PhaseExited = "exited"
)

// Event is one thing that happened. Kind says which payload is set. Lines
// are applied after the payload, in the same update.
type Event struct {
	Kind  string    `json:"kind"`
	JobID string    `json:"jobId"`
	Lines []LogLine `json:"lines,omitempty"`

	Status       *Status            `json:"status,omitempty"`
	AgentRun     *AgentRun          `json:"agentRun,omitempty"`
	Baseline     *VerificationRun   `json:"baseline,omitempty"`
	Verification *VerificationRun   `json:"verification,omitempty"`
	DiffHygiene  *DiffHygieneReport `json:"diffHygiene,omitempty"`
	Review       *ReviewRun         `json:"review,omitempty"`
	Harvest      *FixHarvest        `json:"harvest,omitempty"`
	TestResults  *TestHarvest       `json:"testResults,omitempty"`
	Done         *Done              `json:"done,omitempty"`
}

// LogLine is one job-log line. Guarded lines (and every payload) are
// dropped once the job is cancelled; unguarded ones, such as agent output
// still draining, are kept until the job is deleted.
type LogLine struct {
	Text    string `json:"text"`
	Level   string `json:"level"`
	Source  string `json:"source"`
	Guarded bool   `json:"guarded,omitempty"`
}

// Status is a phase change.
type Status struct {
	Phase string `json:"phase"`
	// DiffBase is set on PhaseRunning: each repo's diff base for this run.
	DiffBase map[string]string `json:"diffBase,omitempty"`
	// SubmittedHeads is set on PhaseRunning of a ModeVerifySubmitted run:
	// each repo's commit that was fetched and is being verified.
	SubmittedHeads map[string]string `json:"submittedHeads,omitempty"`
	// Failed and Error are set on PhaseAgentExit.
	Failed bool   `json:"failed,omitempty"`
	Error  string `json:"error,omitempty"`
}

// FixHarvest is what a batch fix left in the handoff, read by the executor
// and judged by the orchestrator.
type FixHarvest struct {
	Defects         []HarvestedDefect `json:"defects"`
	Unfixed         map[string]bool   `json:"unfixed,omitempty"`
	NotesUnreadable bool              `json:"notesUnreadable,omitempty"`
	// AllowResolve is false for a failed run; it then imports and resolves
	// nothing beyond what the gates allow (HarvestOpts).
	AllowResolve bool             `json:"allowResolve,omitempty"`
	Verification *VerificationRun `json:"verification,omitempty"`
	Review       *ReviewRun       `json:"review,omitempty"`
}

// HarvestedDefect is one defect's fix images and note.
type HarvestedDefect struct {
	DefectID string        `json:"defectId"`
	Images   []HarvestFile `json:"images,omitempty"`
	// OversizedImages are image names skipped for size, in directory order.
	OversizedImages []string `json:"oversizedImages,omitempty"`
	Note            string   `json:"note,omitempty"`
	// OversizedNotes are note paths skipped for size.
	OversizedNotes []string `json:"oversizedNotes,omitempty"`
	// NoteSHA256 and NoteSize name the note as a blob when a remote worker
	// sends it by reference (worker protocol v1 §8.4). Local runs leave them
	// empty and keep Note inline.
	NoteSHA256 string `json:"noteSha256,omitempty"`
	NoteSize   int64  `json:"noteSize,omitempty"`
}

// HarvestFile is one harvested file. Data travels inline in W1; W2 moves it
// to a content-addressed blob.
type HarvestFile struct {
	Filename string `json:"filename"`
	Data     []byte `json:"data,omitempty"`
	// SHA256 and Size name the bytes as a blob (worker protocol v1 §7, §8.4).
	// A remote event omits Data; a local one keeps Data inline.
	SHA256 string `json:"sha256,omitempty"`
	Size   int64  `json:"size,omitempty"`
}

// TestHarvest is a test run's results, parsed and redacted.
type TestHarvest struct {
	Steps   []TestStepResult `json:"steps"`
	Warns   []string         `json:"warns,omitempty"`
	Ignored int              `json:"ignored,omitempty"`
	Finish  bool             `json:"finish,omitempty"`
}

// TestStepResult mirrors store.HarvestedStep.
type TestStepResult struct {
	Key         string        `json:"key"`
	Status      string        `json:"status"`
	Actual      string        `json:"actual"`
	Exploratory bool          `json:"exploratory,omitempty"`
	Action      string        `json:"action,omitempty"`
	Expected    string        `json:"expected,omitempty"`
	HasExpected bool          `json:"hasExpected,omitempty"`
	Section     string        `json:"section,omitempty"`
	Images      []HarvestFile `json:"images,omitempty"`
}

// Done is the job's outcome.
type Done struct {
	State string `json:"state"`
	Line  string `json:"line,omitempty"`
}

// Control kinds. ShipPreview, Diff, Ship and Discard are carried by the
// remote transport only (worker protocol v1 §6.3); local shipping stays in
// internal/jobs and internal/api.
const (
	ControlCancel      = "cancel"
	ControlResume      = "resume"
	ControlShipPreview = "ship_preview"
	ControlDiff        = "diff"
	ControlShip        = "ship"
	ControlDiscard     = "discard"
)

// ShipRepo is one repo of a ship control: the files the operator confirmed
// and the commit message and pull request text. Author, when set, is the
// commit's author (the committer stays Defect Drainer): a deploy check such
// as Vercel's admits a commit only when its author is a known account.
type ShipRepo struct {
	Repo    string        `json:"repo"`
	Files   []string      `json:"files"`
	Message string        `json:"message"`
	Title   string        `json:"title"`
	Body    string        `json:"body"`
	Author  *CommitAuthor `json:"author,omitempty"`
}

// CommitAuthor is a git identity: a display name and an email.
type CommitAuthor struct {
	Name  string `json:"name"`
	Email string `json:"email"`
}

// Control is an instruction for a running job. Resume carries the spec of
// the next run; Cancel stops the current one.
type Control struct {
	Kind  string   `json:"kind"`
	JobID string   `json:"jobId"`
	Spec  *JobSpec `json:"spec,omitempty"`
	// RequestID makes a remote control idempotent; SpecURL replaces Spec for
	// a remote resume; Repo is the diff target; Repos the ship list (worker
	// protocol v1 §6.3, §8.5). The local transport uses none of them.
	RequestID string     `json:"requestId,omitempty"`
	SpecURL   string     `json:"specUrl,omitempty"`
	Repo      string     `json:"repo,omitempty"`
	Repos     []ShipRepo `json:"repos,omitempty"`
}

// Answers a Sink may give. ErrInactive: the job was cancelled or deleted,
// stop quietly. ErrParked: the job waits for a host lease and will be
// started again.
var (
	ErrInactive = errors.New("job is no longer active")
	ErrParked   = errors.New("job parked")
)
