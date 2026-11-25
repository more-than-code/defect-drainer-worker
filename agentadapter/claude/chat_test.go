package claude

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"

	aa "github.com/more-than-code/defect-drainer-worker/agentadapter"
)

// §21: a chat task resumes the session with no shell, no edits and no
// subagents, and its settings allow reads only.
func TestChatTaskIsReadOnlyAndResumes(t *testing.T) {
	a := New(Config{Bin: "/usr/bin/claude"})
	tk := task(aa.Chat, aa.APIKey)
	tk.ResumeID = "023759b8-8496-44f8-9ef8-e6fb4e1e08e4"
	c, err := a.Command(tk)
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(c.Args, " ")
	if !strings.Contains(joined, "--disallowedTools "+ChatDisallowedTools+" --resume "+tk.ResumeID) {
		t.Fatalf("argv = %q", c.Args)
	}
	if !slices.Contains(c.Args, "/data/agent-state/j1/claude-chat-settings.json") {
		t.Fatalf("settings path: %q", c.Args)
	}
	b, err := Settings(tk)
	if err != nil {
		t.Fatal(err)
	}
	var s settings
	if err := json.Unmarshal(b, &s); err != nil {
		t.Fatal(err)
	}
	for _, rule := range s.Permissions.Allow {
		if rule == "Bash" || strings.HasPrefix(rule, "Edit(") {
			t.Fatalf("chat settings allow %q", rule)
		}
	}
	if !slices.Contains(s.Permissions.Deny, "Bash") {
		t.Fatalf("chat settings do not deny Bash: %v", s.Permissions.Deny)
	}
	tk.ResumeID = ""
	if _, err := a.Command(tk); err == nil {
		t.Fatal("a chat task without a session must be refused")
	}
}
