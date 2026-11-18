package exec

import (
	"os"
	osexec "os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestHostGitEnvDumpExec covers the internal/exec git children: GitCAbs
// (diff hygiene, test-run status) and execReviewGit (the reviewer's diff).
// A git wrapper first on PATH dumps its env, then runs the real git.
func TestHostGitEnvDumpExec(t *testing.T) {
	secrets := map[string]string{
		"DEFECT_DRAINER_DATABASE_URL":   "postgres://dd:secret@db/dd",
		"DEFECT_DRAINER_CLAUDE_API_KEY": "sk-dd-claude",
		"ANTHROPIC_API_KEY":             "sk-ant-secret",
		"DD_TEST_ACCOUNT_PASSWORD":      "pw-test-account",
		"QA_PASSWORD":                   "pw-app-env-secret",
	}
	for k, v := range secrets {
		t.Setenv(k, v)
	}
	t.Setenv("GH_TOKEN", "gh-token-kept")
	for _, k := range []string{"GIT_AUTHOR_NAME", "GIT_COMMITTER_NAME"} {
		t.Setenv(k, "t")
	}
	for _, k := range []string{"GIT_AUTHOR_EMAIL", "GIT_COMMITTER_EMAIL"} {
		t.Setenv(k, "t@t")
	}
	realGit, err := osexec.LookPath("git")
	if err != nil {
		t.Skip("git not on PATH")
	}
	root := t.TempDir()
	repo := filepath.Join(root, "repo")
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"init", "-q", "-b", "main"},
		{"commit", "-q", "--allow-empty", "-m", "base"},
	} {
		if out, err := osexec.Command(realGit, append([]string{"-C", repo}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	bin := filepath.Join(root, "bin")
	dumps := filepath.Join(root, "dumps")
	for _, d := range []string{bin, dumps} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	wrapper := "#!/bin/sh\nenv > \"" + dumps + "/git.$$\"\nexec \"" + realGit + "\" \"$@\"\n"
	if err := os.WriteFile(filepath.Join(bin, "git"), []byte(wrapper), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))

	check := func(label string, wantGitAuthor bool) {
		t.Helper()
		matches, _ := filepath.Glob(filepath.Join(dumps, "git.*"))
		if len(matches) == 0 {
			t.Fatalf("%s: no git child ran through the wrapper", label)
		}
		for _, m := range matches {
			raw, err := os.ReadFile(m)
			if err != nil {
				t.Fatal(err)
			}
			env := "\n" + string(raw)
			for k, v := range secrets {
				if strings.Contains(env, "\n"+k+"=") || strings.Contains(env, v) {
					t.Errorf("%s: %s reached git", label, k)
				}
			}
			for _, want := range []string{"\nPATH=", "\nHOME=", "\nGH_TOKEN=gh-token-kept\n"} {
				if !strings.Contains(env, want) {
					t.Errorf("%s: %q missing", label, strings.TrimSpace(want))
				}
			}
			if got := strings.Contains(env, "\nGIT_AUTHOR_NAME="); got != wantGitAuthor {
				t.Errorf("%s: GIT_AUTHOR_NAME present=%v, want %v", label, got, wantGitAuthor)
			}
			_ = os.Remove(m)
		}
	}

	if _, err := GitCAbs(repo, "rev-parse", "HEAD"); err != nil {
		t.Fatal(err)
	}
	check("GitCAbs", true)

	if _, err := execReviewGit(repo, "", nil, "status", "--porcelain"); err != nil {
		t.Fatal(err)
	}
	// The reviewer's git also drops every inherited GIT_* name.
	check("execReviewGit", false)
}
