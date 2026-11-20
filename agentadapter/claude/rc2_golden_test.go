package claude

import (
	"os"
	"path/filepath"
	"testing"

	aa "github.com/more-than-code/defect-drainer-worker/agentadapter"
)

// checkGolden compares got with testdata/rc2/<name>. DD_UPDATE_GOLDEN=1
// rewrites the file; the goldens were captured from v0.2.0-rc2 (0728df1)
// before the worker-browser change, so a job without Browser must still
// produce them byte for byte (worker-browser AC6).
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

// goldenTask is a worker job's implementer task with every rule kind set.
func goldenTask() aa.Task {
	tk := task(aa.Implementer, aa.APIKey)
	tk.Handoff = "/srv/dd/jobs/j1/handoff"
	tk.Worktrees = []string{"/srv/dd/jobs/j1/handoff/worktrees/web"}
	tk.DenyRead = []string{"/etc/dd-worker", "/srv/dd/repos", "/srv/dd/jobs", "/home/dd/.claude"}
	tk.ReadOnly = []string{"/srv/dd/repos/clone-a/.git", "/srv/dd/jobs/j1/evidence"}
	tk.DenyWrite = []string{"/srv/dd/status.json", "/srv/dd/readiness", "/home/dd"}
	return tk
}

func TestSettingsMatchRC2(t *testing.T) {
	tk := goldenTask()
	for _, c := range []struct {
		name    string
		role    aa.Role
		domains []string
	}{
		{"settings-implementer-no-network.json", aa.Implementer, nil},
		{"settings-implementer-domains.json", aa.Implementer, []string{"registry.npmjs.org"}},
		{"settings-reviewer.json", aa.Reviewer, []string{"registry.npmjs.org"}},
	} {
		tk.Role, tk.AllowedDomains = c.role, c.domains
		body, err := Settings(tk)
		if err != nil {
			t.Fatal(err)
		}
		checkGolden(t, c.name, body)
	}
}
