package git

import (
	"strings"
	"testing"

	"github.com/more-than-code/defect-drainer-worker/agentadapter/antigravity"
	"github.com/more-than-code/defect-drainer-worker/agentadapter/claude"
)

// Non-grok adapters get their own sandbox paragraph: no --sandbox flag text
// and no ~/.grok write grant. Grok's text is covered by the existing tests.
func TestPromptSandboxNotesPerAdapter(t *testing.T) {
	wts := []WorktreeBinding{{Repo: "demo", WorktreeAbs: "/wt/demo", PrimaryAbs: "/src/demo", Branch: "b"}}
	for _, opts := range []*CodingAgentOpts{
		{Adapter: claude.New(claude.Config{})},
		{Adapter: antigravity.New(antigravity.Config{})},
	} {
		name := string(opts.Adapter.Name())
		p := buildBatchFixCLIPrompt("/h/job", "BATCH-1", "/defects", "strict", wts, opts)
		if !strings.Contains(p, "SANDBOX: the "+name+" CLI's own sandbox, strict") {
			t.Fatalf("%s prompt lacks its sandbox headline:\n%s", name, p)
		}
		if strings.Contains(p, "~/.grok") || strings.Contains(p, "--sandbox") {
			t.Fatalf("%s prompt names grok's sandbox:\n%s", name, p)
		}
		if got := SandboxHeadline(opts.Adapter, "workspace"); !strings.Contains(got, name) {
			t.Fatalf("headline = %q", got)
		}
	}
	if got := SandboxHeadline(nil, "workspace"); got != "SANDBOX: --sandbox workspace (App Settings)." {
		t.Fatalf("grok headline changed: %q", got)
	}
}
