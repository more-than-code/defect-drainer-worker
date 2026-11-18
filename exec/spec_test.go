package exec

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/more-than-code/defect-drainer-worker/git"
)

// In W2 the JobSpec, every Event and every Control cross a WebSocket as
// JSON. These tests pin that each one survives the round trip unchanged,
// and that the fixtures set every field, so a field added later without a
// JSON-safe type fails here first.

func fullVerification() *VerificationRun {
	return &VerificationRun{Ran: true, Ok: true, StartedAt: "2026-11-15T00:00:00.000Z", FinishedAt: "2026-11-15T00:01:00.000Z", Results: []VerifyResult{{
		Repo: "web", Command: "make test", Cwd: "/w/web", ExitCode: IntPtr(1), Signal: strPtr("SIGKILL"),
		Ok: true, DurationMs: 42, OutputTail: "tail", SkippedReason: "skip",
	}}}
}

func fullReview() *ReviewRun {
	return &ReviewRun{OK: true, Required: true, SkippedReason: "s", Error: "e", TotalCostUSD: 0.5, Lenses: []ReviewLens{{
		Lens: "spec", OK: true, Error: "e", Turns: 3, CostUSD: 0.25, SessionID: "sess",
		Findings: []ReviewFinding{{Severity: "high", Lens: "spec", DefectID: "DEF-1", Location: "a.go:1", Finding: "f", Reasoning: "r", SuggestedFix: "s"}},
	}}}
}

func fullHygiene() *DiffHygieneReport {
	return &DiffHygieneReport{Noisy: true, Repos: []RepoDiffHygiene{{Repo: "web", BaseSha: "abc", FilesChanged: 1, ChangedLines: 2, EffectiveLines: 1, FormattingOnlyLines: 1,
		Hunks: 2, EffectiveHunks: 1, FormattingOnlyHunks: 1, FormattingRatio: 0.5, Error: "e"}}}
}

func fullAgentRun() *AgentRun {
	return &AgentRun{Iterations: 2, CompletedByArtifacts: true, StoppedBy: StoppedByArtifacts, StopReasons: []string{"end_turn"},
		Turns: 9, TotalCostUSD: 1.5, SessionID: "s1", Missing: []string{"NOTES.md"}, Resumes: 1, Runner: "claude", Tokens: 7}
}

func fullSpec() JobSpec {
	planID, planVersion, defID, defStatus := "TPLAN-1", 2, "DEF-1", "open"
	return JobSpec{
		JobID: "bjob_1", Kind: KindBatchFix, AppID: "app_0123456789abcdef", Label: "BATCH-1", BatchID: "BATCH-1",
		Mode: ModeResume, ResumeSession: "sess-1", Handoff: "/data/batch-jobs/bjob_1", DefectsRoot: "/data",
		Worktrees: []git.WorktreeBinding{{Repo: "web", PrimaryAbs: "/p/web", WorktreeAbs: "/w/web", Branch: "dd/b", RepoURL: "git@x:web.git", BaseRef: "main", BaseBranch: "main", BaseSource: "local"}},
		DefectIDs: []string{"DEF-1"}, HarvestIDs: []string{"DEF-1"},
		DiffBase: map[string]string{"web": "abc"}, Baseline: fullVerification(), PriorAgentRun: fullAgentRun(),
		CreatedAt: "2026-11-15T00:00:00Z", RunStartedAt: "2026-11-15T01:00:00Z",
		App: AppSettings{Sandbox: "strict", Toolchain: "flutter", SimulatorWrites: true, MaxTurns: 80,
			VerifyCommands: []VerifyCommand{{Repo: "web", Command: "make test"}},
			RepoEntries:    []RepoEntry{{Name: "web", URL: "/p/web", BaseSource: "local", BaseBranch: "main"}}},
		Fix: &FixSpec{
			Batch: Batch{ID: "BATCH-1", AppID: "app_0123456789abcdef", Title: "t", Goal: "g", Status: "in_progress", DefectIDs: []string{"DEF-1"}, Mode: "grok", Created: "c", UpdatedAt: "u", Path: "sqlite:batches/BATCH-1"},
			Defects: []Defect{{ID: "DEF-1", AppID: "app", Title: "t", Severity: "P1", Status: "open", Area: "a", Client: "c", Surface: "s",
				Repos: []string{"web"}, Labels: []string{"l"}, Related: []string{"DEF-2"}, Source: "src", Reporter: "r", KeyFiles: []string{"k"},
				Evidence: []string{"evidence/DEF-1/a.png"}, FixEvidence: []string{"evidence/DEF-1/b.png"}, Reported: "2026-11-15", Summary: "s",
				ResolvedDate: "d", Resolution: "r", DuplicateOf: "DEF-3", Body: "b", Bucket: "open", CreatedAt: "c", Path: "p"}},
			Evidence: []EvidenceRef{{Path: "evidence/DEF-1/a.png", SHA256: "e3b0"}},
		},
		TestRun: &TestRunSpec{RunID: "TRUN-1", SecretEnv: "QA_PASSWORD", Loaded: true, Run: TestRun{
			ID: "TRUN-1", AppID: "app", PlanID: &planID, PlanVersion: &planVersion, Title: "t", Status: "queued",
			Env:   map[string]any{"base_url": "https://x", "account": map[string]any{"label": "qa", "secret_ref": "env:QA_PASSWORD"}, "retries": float64(2)},
			Refs:  []RunRef{{Repo: "web", Ref: "main", SHA: "abc"}},
			Steps: []RunStep{{Key: "login", Position: 1, Section: "s", Action: "a", Expected: "e", Exploratory: true, Status: "pass", Actual: "x", Evidence: []string{"e"}, DefectID: &defID, DefectStatus: &defStatus, Review: "r", ReviewNote: "n", UpdatedAt: "u"}},
		}},
		StripEnv: []string{"QA_PASSWORD"},
		Caps:     Caps{AgentMaxIters: 8, AgentMaxWallSec: 2700, ReviewOff: true, ReviewMaxTurns: 8, ReviewMaxWallSec: 900, AgentMaxTurnsArg: "120", GrokBypassPermissions: true, GrokTools: "bash"},
		Adapter:  "claude",
		Model:    "sonnet",
	}
}

func fullEvents() []Event {
	lines := []LogLine{{Text: "line", Level: "info", Source: "DefectDrainer", Guarded: true}}
	return []Event{
		{Kind: EventLog, JobID: "bjob_1", Lines: lines},
		{Kind: EventStatus, JobID: "bjob_1", Lines: lines, Status: &Status{Phase: PhaseAgentExit, DiffBase: map[string]string{"web": "abc"}, Failed: true, Error: "Grok exited code=1 signal=null"}},
		{Kind: EventAgentRun, JobID: "bjob_1", Lines: lines, AgentRun: fullAgentRun()},
		{Kind: EventBaseline, JobID: "bjob_1", Lines: lines, Baseline: fullVerification()},
		{Kind: EventVerification, JobID: "bjob_1", Lines: lines, Verification: fullVerification(), DiffHygiene: fullHygiene(), Baseline: fullVerification()},
		{Kind: EventDiffHygiene, JobID: "bjob_1", Lines: lines, DiffHygiene: fullHygiene()},
		{Kind: EventReview, JobID: "bjob_1", Lines: lines, Review: fullReview()},
		{Kind: EventHarvest, JobID: "bjob_1", Lines: lines, Harvest: &FixHarvest{
			Defects: []HarvestedDefect{{DefectID: "DEF-1", Images: []HarvestFile{{Filename: "a.png", Data: []byte{0x89, 'P', 'N', 'G', 0}, SHA256: "0f1e2d3c4b5a69788796a5b4c3d2e1f00f1e2d3c4b5a69788796a5b4c3d2e1f0", Size: 5}},
				OversizedImages: []string{"big.png"}, Note: "fixed", OversizedNotes: []string{"/h/fix-notes/DEF-1.md"}, NoteSHA256: "0f1e2d3c4b5a69788796a5b4c3d2e1f00f1e2d3c4b5a69788796a5b4c3d2e1f0", NoteSize: 5}},
			Unfixed: map[string]bool{"DEF-2": true}, NotesUnreadable: true, AllowResolve: true,
			Verification: fullVerification(), Review: fullReview(),
		}},
		{Kind: EventTestResults, JobID: "bjob_1", Lines: lines, TestResults: &TestHarvest{
			Steps: []TestStepResult{{Key: "login", Status: "pass", Actual: "ok", Exploratory: true, Action: "a", Expected: "e", HasExpected: true, Section: "s",
				Images: []HarvestFile{{Filename: "shot.png", Data: []byte("png"), SHA256: "0f1e2d3c4b5a69788796a5b4c3d2e1f00f1e2d3c4b5a69788796a5b4c3d2e1f0", Size: 3}}}},
			Warns: []string{"harvest: ignore results/x.md (unknown step)"}, Ignored: 1, Finish: true,
		}},
		{Kind: EventDone, JobID: "bjob_1", Lines: lines, Done: &Done{State: "completed", Line: "batch complete"}},
	}
}

func roundTrip[T any](t *testing.T, in T) {
	t.Helper()
	raw, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("marshal %T: %v", in, err)
	}
	var out T
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("unmarshal %T: %v\n%s", in, err, raw)
	}
	if !reflect.DeepEqual(in, out) {
		t.Fatalf("%T changed in a JSON round trip\nin:  %#v\nout: %#v\njson: %s", in, in, out, raw)
	}
	again, err := json.Marshal(out)
	if err != nil {
		t.Fatal(err)
	}
	if string(again) != string(raw) {
		t.Fatalf("%T re-encodes differently\nfirst:  %s\nsecond: %s", in, raw, again)
	}
}

// requireSet fails on any zero exported field reachable from v, so every
// field is in the round trip. Kinds lists which Event payloads a fixture
// set covers; the Event check runs over the union of fixtures.
func requireSet(t *testing.T, path string, v reflect.Value) {
	t.Helper()
	switch v.Kind() {
	case reflect.Pointer:
		if v.IsNil() {
			t.Errorf("%s is nil in the fixture", path)
			return
		}
		requireSet(t, path, v.Elem())
	case reflect.Struct:
		for i := 0; i < v.NumField(); i++ {
			f := v.Type().Field(i)
			if !f.IsExported() {
				continue
			}
			requireSet(t, path+"."+f.Name, v.Field(i))
		}
	case reflect.Slice:
		if v.Len() == 0 {
			t.Errorf("%s is empty in the fixture", path)
			return
		}
		requireSet(t, path+"[0]", v.Index(0))
	case reflect.Map:
		if v.Len() == 0 {
			t.Errorf("%s is empty in the fixture", path)
		}
	default:
		if v.IsZero() {
			t.Errorf("%s is zero in the fixture", path)
		}
	}
}

func TestJobSpecRoundTrip(t *testing.T) {
	spec := fullSpec()
	requireSet(t, "JobSpec", reflect.ValueOf(spec))
	roundTrip(t, spec)
}

func TestEventRoundTrip(t *testing.T) {
	events := fullEvents()
	// Every payload field is set by some event of the matching kind.
	covered := map[string]bool{}
	for _, e := range events {
		roundTrip(t, e)
		v := reflect.ValueOf(e)
		for i := 0; i < v.NumField(); i++ {
			f := v.Type().Field(i)
			if !v.Field(i).IsZero() {
				covered[f.Name] = true
				requireSet(t, "Event("+e.Kind+")."+f.Name, v.Field(i))
			}
		}
	}
	et := reflect.TypeOf(Event{})
	for i := 0; i < et.NumField(); i++ {
		if !covered[et.Field(i).Name] {
			t.Errorf("no fixture event sets Event.%s", et.Field(i).Name)
		}
	}
	kinds := map[string]bool{}
	for _, e := range events {
		kinds[e.Kind] = true
	}
	for _, k := range []string{EventLog, EventStatus, EventAgentRun, EventBaseline, EventVerification, EventDiffHygiene, EventReview, EventHarvest, EventTestResults, EventDone} {
		if !kinds[k] {
			t.Errorf("no fixture for event kind %s", k)
		}
	}
}

func TestControlRoundTrip(t *testing.T) {
	spec := fullSpec()
	for _, c := range []Control{
		{Kind: ControlCancel, JobID: "bjob_1"},
		{Kind: ControlResume, JobID: "bjob_1", Spec: &spec},
		{Kind: ControlResume, JobID: "bjob_1", RequestID: "rq_1", SpecURL: "/worker/v1/jobs/bjob_1/spec"},
		{Kind: ControlDiff, JobID: "bjob_1", RequestID: "rq_2", Repo: "webapp"},
		{Kind: ControlShip, JobID: "bjob_1", RequestID: "rq_3", Repos: []ShipRepo{{Repo: "webapp", Files: []string{"src/a.ts"}, Message: "fix: a", Title: "Fix a", Body: "Fixes DEF-1"}}},
		{Kind: ControlShipPreview, JobID: "bjob_1", RequestID: "rq_4"},
		{Kind: ControlDiscard, JobID: "bjob_1", RequestID: "rq_5"},
	} {
		roundTrip(t, c)
	}
}

// A Kind the reader does not know, and keys it does not know, decode without
// error (W2 decodes leniently, §3.2).
func TestEventDecodesUnknownKeys(t *testing.T) {
	var e Event
	raw := `{"kind":"future_kind","jobId":"bjob_1","lines":[{"text":"x","level":"info","source":"DefectDrainer","color":"red"}],"newPayload":{"a":1}}`
	if err := json.Unmarshal([]byte(raw), &e); err != nil {
		t.Fatal(err)
	}
	if e.Kind != "future_kind" || len(e.Lines) != 1 || e.Lines[0].Text != "x" {
		t.Fatalf("decoded %+v", e)
	}
}
