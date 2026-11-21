package exec

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/more-than-code/defect-drainer-worker/git"
)

// A batch fix that is not verify_submitted is byte-identical to v0.2.0-rc4
// (79c6cc1) on the wire: the spec a server sends and the running status a
// worker reports. testdata/rc4 was captured by running this test on the rc4
// tree with DD_UPDATE_GOLDEN=1; this file uses no field added after rc4.
func TestBatchFixWireIsRC4(t *testing.T) {
	spec := JobSpec{
		JobID: "bjob_rc4", Kind: KindBatchFix, AppID: "app1", Label: "BATCH-1", BatchID: "BATCH-1",
		Mode: ModeFresh, DefectIDs: []string{"DEF-1"}, HarvestIDs: []string{"DEF-1"},
		CreatedAt: "2026-11-21T00:00:00Z", RunStartedAt: "2026-11-21T00:00:00Z",
		Worktrees: []git.WorktreeBinding{{Repo: "app", Branch: "dd/bjob_rc4/app", RepoURL: "https://github.com/acme/app", BaseSource: "origin", BaseBranch: "main"}},
		App: AppSettings{Sandbox: "strict", RepoEntries: []RepoEntry{{Name: "app", URL: "https://github.com/acme/app", BaseSource: "origin", BaseBranch: "main"}},
			VerifyCommands: []VerifyCommand{{Repo: "app", Command: "make test"}}},
		Fix: &FixSpec{
			Batch:   Batch{ID: "BATCH-1", AppID: "app1", Title: "t", DefectIDs: []string{"DEF-1"}},
			Defects: []Defect{{ID: "DEF-1", AppID: "app1", Title: "Button broken", Body: "## Acceptance\n- [ ] works\n"}},
		},
		Caps:    Caps{AgentMaxIters: 8, AgentMaxWallSec: 2700, ReviewMaxTurns: 8, ReviewMaxWallSec: 900, AgentMaxCostUSD: 5},
		Adapter: "claude", Model: "sonnet",
	}
	running := Event{Kind: EventStatus, JobID: "bjob_rc4", Status: &Status{Phase: PhaseRunning, DiffBase: map[string]string{"app": "0123456789abcdef0123456789abcdef01234567"}},
		Lines: []LogLine{{Text: "spawning batch fix", Level: "info", Source: "DefectDrainer", Guarded: true}}}
	for name, v := range map[string]any{"spec-fresh.json": spec, "status-running.json": running} {
		got, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		path := filepath.Join("testdata", "rc4", name)
		if os.Getenv("DD_UPDATE_GOLDEN") == "1" {
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, got, 0o644); err != nil {
				t.Fatal(err)
			}
			continue
		}
		want, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != string(want) {
			t.Fatalf("%s differs from rc4:\n--- got\n%s\n--- want\n%s", name, got, want)
		}
	}
}
