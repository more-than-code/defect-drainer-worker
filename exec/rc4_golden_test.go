package exec

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/more-than-code/defect-drainer-worker/git"
)

// A batch fix that is not verify_submitted keeps v0.2.0-rc4's (79c6cc1) wire:
// the running status a worker reports is byte-identical, and the spec a
// server sent rc4 (old keys, appId and app) still decodes to the same value.
// testdata/rc4 was captured on the rc4 tree and is never regenerated; this
// file uses no field added after rc4. testdata/rc7 is the same spec with the
// project keys (§18), regenerated with DD_UPDATE_GOLDEN=1.
func TestBatchFixWireIsRC4(t *testing.T) {
	spec := JobSpec{
		JobID: "bjob_rc4", Kind: KindBatchFix, ProjectID: "app1", Label: "BATCH-1", BatchID: "BATCH-1",
		Mode: ModeFresh, DefectIDs: []string{"DEF-1"}, HarvestIDs: []string{"DEF-1"},
		CreatedAt: "2026-11-21T00:00:00Z", RunStartedAt: "2026-11-21T00:00:00Z",
		Worktrees: []git.WorktreeBinding{{Repo: "app", Branch: "dd/bjob_rc4/app", RepoURL: "https://github.com/acme/app", BaseSource: "origin", BaseBranch: "main"}},
		Project: ProjectSettings{Sandbox: "strict", RepoEntries: []RepoEntry{{Name: "app", URL: "https://github.com/acme/app", BaseSource: "origin", BaseBranch: "main"}},
			VerifyCommands: []VerifyCommand{{Repo: "app", Command: "make test"}}},
		Fix: &FixSpec{
			Batch:   Batch{ID: "BATCH-1", ProjectID: "app1", Title: "t", DefectIDs: []string{"DEF-1"}},
			Defects: []Defect{{ID: "DEF-1", ProjectID: "app1", Title: "Button broken", Body: "## Acceptance\n- [ ] works\n"}},
		},
		Caps:    Caps{AgentMaxIters: 8, AgentMaxWallSec: 2700, ReviewMaxTurns: 8, ReviewMaxWallSec: 900, AgentMaxCostUSD: 5},
		Adapter: "claude", Model: "sonnet",
	}
	running := Event{Kind: EventStatus, JobID: "bjob_rc4", Status: &Status{Phase: PhaseRunning, DiffBase: map[string]string{"app": "0123456789abcdef0123456789abcdef01234567"}},
		Lines: []LogLine{{Text: "spawning batch fix", Level: "info", Source: "DefectDrainer", Guarded: true}}}
	if got := mustMarshal(t, running); got != readGolden(t, "rc4", "status-running.json") {
		t.Fatalf("status-running.json differs from rc4:\n--- got\n%s", got)
	}
	var old JobSpec
	if err := json.Unmarshal([]byte(readGolden(t, "rc4", "spec-fresh.json")), &old); err != nil || !reflect.DeepEqual(old, spec) {
		t.Fatalf("rc4 spec decodes to %+v, %v; want %+v", old, err, spec)
	}
	got := mustMarshal(t, spec)
	path := filepath.Join("testdata", "rc7", "spec-fresh.json")
	if os.Getenv("DD_UPDATE_GOLDEN") == "1" {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if want := readGolden(t, "rc7", "spec-fresh.json"); got != want {
		t.Fatalf("spec-fresh.json differs from rc7:\n--- got\n%s\n--- want\n%s", got, want)
	}
}

func mustMarshal(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func readGolden(t *testing.T, dir, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", dir, name))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
