package workerproto

import (
	"testing"

	dexec "github.com/more-than-code/defect-drainer-worker/exec"
)

// A ship control lists files inside the app's repo, which follow the app's
// own naming: SvelteKit and Next.js route files must pass (the live Create
// PR that hung on src/routes/dictionary/+page.svelte).
func TestValidRepoFilePath(t *testing.T) {
	for _, p := range []string{"src/routes/dictionary/+page.svelte", "src/routes/[id]/+page.ts", "src/routes/(app)/+layout.svelte",
		"app/@modal/page.tsx", "src/routes/[...rest]/+server.ts", "a~b/c.txt", "README.md", "src/a.ts"} {
		if !ValidRepoFilePath(p) {
			t.Errorf("ValidRepoFilePath(%q) = false", p)
		}
	}
	for _, p := range []string{"", "../x", "/abs", "a//b", "-rf", "a/-rf", "a/./b", "a/../b", ".", "..", "a\\b", "a\x00b", "a b", "a:b", "a*b", "a?b", "a/"} {
		if ValidRepoFilePath(p) {
			t.Errorf("ValidRepoFilePath(%q) = true", p)
		}
	}
	// The stricter rule is unchanged for repo names, evidence and handoff paths.
	if ValidRelPath("src/routes/dictionary/+page.svelte") {
		t.Error("ValidRelPath widened")
	}
}

func TestControlShipRouteFiles(t *testing.T) {
	f := Control{Header: NewHeader(TypeControl), JobID: "bjob_1", LeaseID: "ls_1", RequestID: "rq_1",
		Control: dexec.Control{Kind: dexec.ControlShip, JobID: "bjob_1", RequestID: "rq_1", Repos: []dexec.ShipRepo{{Repo: "webapp",
			Files: []string{"src/routes/dictionary/+page.svelte", "src/routes/[id]/+page.ts"}, Message: "m", Title: "t", Body: "b"}}}}
	if err := f.Validate(); err != nil {
		t.Fatalf("ship of route files refused: %v", err)
	}
	f.Control.Repos[0].Files = []string{"src/../../x"}
	if f.Validate() == nil {
		t.Fatal("a traversal accepted")
	}
	f.Control.Repos[0].Files = []string{"ok.ts"}
	f.Control.Repos[0].Repo = "web+app"
	if f.Validate() == nil {
		t.Fatal("a repo name with + accepted")
	}
}
