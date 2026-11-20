package git

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/more-than-code/defect-drainer-worker/agentadapter/claude"
)

// checkGolden compares got with testdata/rc2/<name>. DD_UPDATE_GOLDEN=1
// rewrites the file; the goldens were captured from v0.2.0-rc2 (0728df1)
// before the worker-browser change (AC6: a job without Browser is rc2).
func checkGolden(t *testing.T, name string, got []byte) {
	t.Helper()
	path := filepath.Join("testdata", "rc2", name)
	if os.Getenv("DD_UPDATE_GOLDEN") == "1" {
		if err := os.WriteFile(path, got, 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(want) {
		t.Fatalf("%s differs from rc2:\n--- got\n%s\n--- want\n%s", name, got, want)
	}
}

func TestPromptMatchesRC2(t *testing.T) {
	wts := []WorktreeBinding{{Repo: "web", WorktreeAbs: "/srv/dd/jobs/j1/handoff/worktrees/web", PrimaryAbs: "/srv/dd/repos/web", Branch: "dd/BATCH-1"}}
	opts := &CodingAgentOpts{
		Adapter:        claude.New(claude.Config{}),
		ReadOnly:       []string{"/srv/dd/jobs/j1/evidence"},
		AllowedDomains: []string{"registry.npmjs.org"},
	}
	checkGolden(t, "prompt-claude.txt", []byte(buildBatchFixCLIPrompt("/srv/dd/jobs/j1/handoff", "BATCH-1", "/srv/dd/jobs/j1/evidence", "strict", wts, opts)))
	checkGolden(t, "continue-prompt.txt", []byte(ContinuePrompt))
}

// Worker browser (B3, B6): the prompt names dd-shot, the continue prompt
// narrows NO-SCREENSHOT, and the Task carries UnixSockets.
func TestBrowserPrompt(t *testing.T) {
	wts := []WorktreeBinding{{Repo: "web", WorktreeAbs: "/srv/dd/jobs/j1/handoff/worktrees/web", PrimaryAbs: "/srv/dd/repos/web", Branch: "dd/BATCH-1"}}
	opts := &CodingAgentOpts{Adapter: claude.New(claude.Config{}), Browser: true}
	p := buildBatchFixCLIPrompt("/srv/dd/jobs/j1/handoff", "BATCH-1", "/srv/dd/jobs/j1/evidence", "strict", wts, opts)
	if !strings.Contains(p, "sh dd-shot <DEF-id> <path>") || !strings.Contains(p, "quoted under it in NOTES.md") {
		t.Fatalf("prompt:\n%s", p)
	}
	opts.NoPreview = true
	p = buildBatchFixCLIPrompt("/srv/dd/jobs/j1/handoff", "BATCH-1", "/srv/dd/jobs/j1/evidence", "strict", wts, opts)
	if !strings.Contains(p, "no preview command") || strings.Contains(p, "sh dd-shot") {
		t.Fatalf("no-preview prompt:\n%s", p)
	}
	if !strings.Contains(ContinuePromptBrowser, "sh dd-shot") || !strings.Contains(ContinuePromptBrowser, "DD-SHOT FAIL") ||
		strings.Contains(ContinuePromptBrowser, "if the sandbox is strict and you cannot screenshot") {
		t.Fatalf("continue prompt:\n%s", ContinuePromptBrowser)
	}
}
