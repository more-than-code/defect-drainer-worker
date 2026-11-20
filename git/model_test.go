package git

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/more-than-code/defect-drainer-worker/agentadapter"
	"github.com/more-than-code/defect-drainer-worker/agentadapter/claude"
)

// fakeClaude records its argv, one per line, in argvFile and prints a result
// with a two-model modelUsage.
func fakeClaude(t *testing.T, argvFile string) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "fake-claude")
	script := "#!/bin/sh\nfor a in \"$@\"; do printf '%s\\n' \"$a\"; done > '" + argvFile + "'\n" +
		"printf '%s\\n' '{\"type\":\"result\",\"subtype\":\"success\",\"is_error\":false,\"session_id\":\"023759b8-8496-44f8-9ef8-e6fb4e1e08e4\",\"result\":\"{}\",\"total_cost_usd\":0.3,\"modelUsage\":{\"a\":{\"costUSD\":0.2,\"canonicalModel\":\"sonnet\"},\"b\":{\"costUSD\":0.1,\"canonicalModel\":\"haiku\"}}}'\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return bin
}

func modelArg(t *testing.T, argvFile string) string {
	t.Helper()
	b, err := os.ReadFile(argvFile)
	if err != nil {
		t.Fatal(err)
	}
	args := strings.Split(strings.TrimSpace(string(b)), "\n")
	for i, a := range args {
		if a == "--model" && i+1 < len(args) {
			return args[i+1]
		}
	}
	return ""
}

// The job's model reaches the implementer's argv, and its cost by model
// reaches OnEnd.
func TestCodingAgentModel(t *testing.T) {
	for _, model := range []string{"", "opus"} {
		argv := filepath.Join(t.TempDir(), "argv")
		wts := []WorktreeBinding{{Repo: "demo", WorktreeAbs: t.TempDir(), PrimaryAbs: t.TempDir(), Branch: "b"}}
		var end AgentEnd
		cmd, flush, err := StartCodingAgent(t.TempDir(), "BATCH-m", t.TempDir(), wts, "strict", func(string) {}, func(string) {}, &CodingAgentOpts{
			Adapter: claude.New(claude.Config{Bin: fakeClaude(t, argv)}), Model: model,
			Credential: agentadapter.Credential{Mode: agentadapter.Subscription}, StateDir: t.TempDir(),
			OnEnd: func(e AgentEnd) { end = e },
		})
		if err != nil {
			t.Fatal(err)
		}
		_ = cmd.Wait()
		flush()
		if got := modelArg(t, argv); got != model {
			t.Errorf("model %q: argv --model = %q", model, got)
		}
		if end.CostByModel["sonnet"].CostUSD != 0.2 || end.CostByModel["haiku"].CostUSD != 0.1 {
			t.Errorf("end = %+v", end)
		}
	}
}

// M4: a reviewer runs on the job's model.
func TestReviewAgentModel(t *testing.T) {
	argv := filepath.Join(t.TempDir(), "argv")
	res, err := StartReviewAgent(ReviewAgentOpts{
		Cwd: t.TempDir(), Adapter: claude.New(claude.Config{Bin: fakeClaude(t, argv)}), Model: "haiku",
		Credential: agentadapter.Credential{Mode: agentadapter.Subscription}, StateDir: t.TempDir(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := modelArg(t, argv); got != "haiku" {
		t.Fatalf("reviewer --model = %q", got)
	}
	if len(res.End.CostByModel) != 2 {
		t.Fatalf("review end = %+v", res.End)
	}
}
