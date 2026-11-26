package exec

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeHandoffFiles(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for rel, body := range files {
		p := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// toyg 4: a NO-VISUAL line claims the fix has nothing visible to show; the
// next non-blank line is its reason, and without one it claims nothing.
func TestNoVisualClaims(t *testing.T) {
	notes := strings.Join([]string{
		"plan",
		"NO-VISUAL: DEF-1",
		"",
		"  the API now refuses an empty id; TestRefuseEmpty proves it  ",
		"NO-VISUAL: DEF-2",
		"UNFIXED: DEF-2",
		"NO-VISUAL: DEF-3",
		"NO-VISUAL: DEF-10 trailing text",
		"reason for nothing",
		"NO-VISUAL: DEF-4",
	}, "\n")
	h := writeHandoffFiles(t, map[string]string{"NOTES.md": notes})
	got := NoVisualClaims(h, []string{"DEF-1", "DEF-2", "DEF-3", "DEF-4", "DEF-10"}, time.Time{})
	if got["DEF-1"] != "the API now refuses an empty id; TestRefuseEmpty proves it" {
		t.Errorf("DEF-1 = %q", got["DEF-1"])
	}
	for _, id := range []string{"DEF-2", "DEF-4", "DEF-10"} {
		if _, ok := got[id]; ok {
			t.Errorf("%s claimed: %q", id, got[id])
		}
	}
	// DEF-3's next line is a marker for another id: no reason, no claim.
	if _, ok := got["DEF-3"]; ok {
		t.Errorf("DEF-3 claimed: %q", got["DEF-3"])
	}
	long := writeHandoffFiles(t, map[string]string{"NOTES.md": "NO-VISUAL: DEF-1\n" + strings.Repeat("x", 900)})
	if r := NoVisualClaims(long, []string{"DEF-1"}, time.Time{})["DEF-1"]; len(r) > NoVisualReasonLimit+len("…") {
		t.Errorf("reason not cut: %d bytes", len(r))
	}
	if got := NoVisualClaims(t.TempDir(), []string{"DEF-1"}, time.Time{}); len(got) != 0 {
		t.Errorf("no NOTES.md claimed %v", got)
	}
}

// The claim releases the loop in any sandbox (it takes the image's place),
// still needs the fix notes, and harvest sends it to the control plane.
func TestNoVisualReleasesLoopAndHarvest(t *testing.T) {
	h := writeHandoffFiles(t, map[string]string{
		"NOTES.md":           "NO-VISUAL: DEF-1\nlog line only\n",
		"fix-notes/DEF-1.md": "Fixed. TestLogLine proves it.",
	})
	ok, missing := ArtifactsComplete(h, []string{"DEF-1"}, "workspace", time.Time{})
	if !ok {
		t.Fatalf("not complete: %v", missing)
	}
	noNotes := writeHandoffFiles(t, map[string]string{"NOTES.md": "NO-VISUAL: DEF-1\nlog line only\n"})
	if ok, missing := ArtifactsComplete(noNotes, []string{"DEF-1"}, "workspace", time.Time{}); ok || len(missing) != 1 || missing[0] != "fix-notes/DEF-1.md" {
		t.Fatalf("without fix notes: ok=%v missing=%v", ok, missing)
	}
	c := CollectFixHarvest(h, []string{"DEF-1"}, []string{"DEF-1"}, time.Time{}, time.Time{})
	if len(c.Defects) != 1 || c.Defects[0].NoVisual != "log line only" || len(c.Defects[0].Images) != 0 {
		t.Fatalf("harvest = %+v", c.Defects)
	}
}

func TestNoVisualInBriefPromptsAndReview(t *testing.T) {
	brief := BuildBatchFixBrief(goldenBriefInput())
	if !strings.Contains(brief, "`NO-VISUAL: <DEF-id>`") {
		t.Error("brief lacks NO-VISUAL")
	}
	md := BuildReviewMD("spec", "BRIEF.md", nil, nil, []string{"DEF-1"})
	if !strings.Contains(md, "## No visual change") || !strings.Contains(md, "`NO-VISUAL: <DEF-id>`") {
		t.Error("REVIEW.md lacks the NO-VISUAL check")
	}
}
