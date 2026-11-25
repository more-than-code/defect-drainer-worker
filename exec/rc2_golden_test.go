package exec

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/more-than-code/defect-drainer-worker/git"
)

// checkGolden compares got with testdata/rc2/<name>. DD_UPDATE_GOLDEN=1
// rewrites the file; the goldens were captured from v0.2.0-rc2 (0728df1)
// before the worker-browser change (AC6: a job without Browser is rc2), and
// rewritten once on purpose for the data, never-yield and secrets rules (toyg).
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

func goldenBriefInput() BatchFixBriefInput {
	return BatchFixBriefInput{
		Batch:       Batch{ID: "BATCH-1", ProjectID: "app", Title: "Fix the header", Goal: "Header fits on phones"},
		Defects:     []Defect{{ID: "DEF-1", ProjectID: "app", Title: "Header overflows", Severity: "major", Status: "open"}},
		DefectsRoot: "/srv/dd/jobs/j1/evidence",
		Worktrees:   []git.WorktreeBinding{{Repo: "web", WorktreeAbs: "/srv/dd/jobs/j1/handoff/worktrees/web", PrimaryAbs: "/srv/dd/repos/web", Branch: "dd/BATCH-1"}},
	}
}

func TestBriefMatchesRC2(t *testing.T) {
	in := goldenBriefInput()
	in.AllowedDomains = []string{"registry.npmjs.org"}
	checkGolden(t, "brief-domains.md", []byte(BuildBatchFixBrief(in)))
	in.AllowedDomains, in.NoNetwork = nil, true
	checkGolden(t, "brief-no-network.md", []byte(BuildBatchFixBrief(in)))
	checkGolden(t, "process.md", []byte(processMarkdown))
}

// Worker browser (B3, B6): the Screenshots section and the narrowed
// NO-SCREENSHOT rule; everything else is the rc2 brief.
func TestBriefBrowser(t *testing.T) {
	in := goldenBriefInput()
	in.AllowedDomains = []string{"registry.npmjs.org"}
	rc2 := BuildBatchFixBrief(in)
	in.Browser = &BrowserBrief{PreviewCmd: "pnpm dev --port 5173 --strictPort", PreviewURL: "http://127.0.0.1:5173"}
	got := BuildBatchFixBrief(in)
	for _, s := range []string{"## Screenshots", "sh dd-shot <DEF-id> <path> [<path>...]", "`pnpm dev --port 5173 --strictPort`",
		"http://127.0.0.1:5173", "Only if `dd-shot` failed", "`DD-SHOT FAIL`"} {
		if !strings.Contains(got, s) {
			t.Fatalf("brief lacks %q:\n%s", s, got)
		}
	}
	if strings.Contains(got, "If the sandbox is **strict** and you cannot screenshot") {
		t.Fatalf("brief keeps the rc2 NO-SCREENSHOT rule:\n%s", got)
	}
	// Removing the browser lines gives rc2's brief back.
	before, _, _ := strings.Cut(got, "## Screenshots")
	if !strings.HasPrefix(rc2, before) {
		t.Fatal("the brief changed before the Screenshots section")
	}
	in.Browser = &BrowserBrief{}
	if got := BuildBatchFixBrief(in); !strings.Contains(got, "**no preview**") || strings.Contains(got, "sh dd-shot") {
		t.Fatalf("no-preview brief:\n%s", got)
	}
}
