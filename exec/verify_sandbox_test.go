package exec

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/more-than-code/defect-drainer-worker/git"
	"github.com/more-than-code/defect-drainer-worker/internal/sandbox"
)

// V7: the verify environment is built, never inherited.
func TestVerifySandboxEnvBuilt(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "sk-inherited")
	t.Setenv("SOME_HOST_VAR", "inherited")
	env := verifySandboxEnv("/home/w", "/h/.verify/tmp", "/h/.verify/cache",
		map[string]string{"PATH": "/sdk/bin:/usr/bin", "FLUTTER_ROOT": "/sdk", "SECRET_X": "s", "DD_TEST_ACCOUNT_PASSWORD": "pw", "DD_OTHER": "x"},
		[]string{"SECRET_X"})
	got := strings.Join(env, "\n")
	for _, bad := range []string{"sk-inherited", "SOME_HOST_VAR", "SECRET_X", "DD_TEST_ACCOUNT_PASSWORD", "DD_OTHER"} {
		if strings.Contains(got, bad) {
			t.Errorf("env carries %s:\n%s", bad, got)
		}
	}
	for _, want := range []string{"PATH=/sdk/bin:/usr/bin", "HOME=/home/w", "TMPDIR=/h/.verify/tmp", "CI=1", "FLUTTER_ROOT=/sdk",
		"HTTPS_PROXY=" + sandbox.ProxyURL, "http_proxy=" + sandbox.ProxyURL, "XDG_CACHE_HOME=/h/.verify/cache", "GOMODCACHE=/h/.verify/cache/go-mod"} {
		if !strings.Contains(got+"\n", want+"\n") {
			t.Errorf("env lacks %s:\n%s", want, got)
		}
	}
}

// V2: a host whose sandbox cannot start skips the checks; it never runs
// them some other way.
func TestHostRunVerificationUnavailableSkips(t *testing.T) {
	h := Host{Verify: &VerifySandbox{Unavailable: errors.New("verify sandbox unavailable: bwrap: not found")}}
	handoff := t.TempDir()
	var lines []string
	run := h.RunVerification(context.Background(), []VerifyCommand{{Repo: "app", Command: "true"}}, nil, handoff, nil, nil, nil, func(l, _ string) { lines = append(lines, l) }, nil)
	if run.Ran || run.Ok || run.SkippedReason != VerifySkippedSandboxUnavailable || len(run.Results) != 0 {
		t.Fatalf("%+v", run)
	}
	if len(lines) != 1 || !strings.Contains(lines[0], "sandbox unavailable") {
		t.Fatalf("log %v", lines)
	}
	// No commands: nothing to skip.
	if run := h.RunVerification(context.Background(), nil, nil, handoff, nil, nil, nil, nil, nil); run.SkippedReason != "" || run.Ran {
		t.Fatalf("empty: %+v", run)
	}
}

// L10: a host without a verify sandbox skips the checks; only the
// dev-only local runner's explicit opt-in runs them unsandboxed (V10).
func TestHostRunVerificationNilSandbox(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "ran")
	cmds := []VerifyCommand{{Repo: "app", Command: "touch " + marker}}
	run := Host{}.RunVerification(context.Background(), cmds, nil, t.TempDir(), nil, nil, nil, nil, nil)
	if run.Ran || run.SkippedReason != VerifySkippedSandboxUnavailable {
		t.Fatalf("nil Verify: %+v", run)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("a check ran without a sandbox or the opt-in")
	}

	wt := gitWorktreeForTest(t)
	cmds[0].Repo = wt.Repo
	run = Host{UnsandboxedVerify: true}.RunVerification(context.Background(), cmds, []git.WorktreeBinding{wt}, t.TempDir(), nil, nil, SnapshotWorktreeReals([]git.WorktreeBinding{wt}), nil, nil)
	if !run.Ran || !run.Ok || run.SkippedReason != "" {
		t.Fatalf("opt-in: %+v", run)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatal("the opted-in local runner did not run the check")
	}
}

// gitWorktreeForTest is a plain directory bound as a worktree: enough for
// the unsandboxed runner, which only needs it to exist.
func gitWorktreeForTest(t *testing.T) git.WorktreeBinding {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return git.WorktreeBinding{Repo: "app", WorktreeAbs: dir}
}
