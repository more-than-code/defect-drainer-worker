package worker

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/more-than-code/defect-drainer-worker/workerproto"
)

// Tests clone from https://git.test/acme/… (setupGit maps it to a bare
// repo with insteadOf), so that is the "GitHub" origin of this package's
// tests: the token header is scoped to it as it is to github.com in
// production (§14).
// Their bare origins are file paths, so tests allow file as well as https.
func init() {
	gitHubOrigin = "https://git.test/"
	gitAllowProtocol = "https:file"
}

// fakeGhOnPath puts a gh in dir that only records that it ran.
func fakeGhOnPath(t *testing.T, dir string) (ranMarker string) {
	t.Helper()
	ranMarker = filepath.Join(t.TempDir(), "gh-ran")
	writeFile(t, filepath.Join(dir, "gh"), "#!/bin/sh\necho \"$*\" >> '"+ranMarker+"'\nexit 0\n", 0o755)
	return ranMarker
}

// fakeGitOnPath puts a git on PATH, and a gh next to it that records any
// run, and nothing else. The git prints version and fails anything else.
func fakeGitOnPath(t *testing.T, version, envDump string) (ghMarker string) {
	t.Helper()
	dir := t.TempDir()
	dump := ""
	if envDump != "" {
		dump = "/usr/bin/env > '" + envDump + "'\n"
	}
	writeFile(t, filepath.Join(dir, "git"), "#!/bin/sh\n"+dump+
		"case \"$*\" in\n"+
		"  *--version*) echo 'git version "+version+" (Apple Git-154)';;\n"+
		"  *) exit 2;;\n"+
		"esac\n", 0o755)
	ghMarker = fakeGhOnPath(t, dir)
	t.Setenv("PATH", dir)
	return ghMarker
}

func toolByName(t *testing.T, tools []workerproto.ToolStatus, name string) workerproto.ToolStatus {
	t.Helper()
	for _, x := range tools {
		if x.Name == name {
			return x
		}
	}
	t.Fatalf("no %s in %+v", name, tools)
	return workerproto.ToolStatus{}
}

// Under gittoken.v1 tools is git only (§14): version, too_old, missing.
// gh is never probed, and no helper or ssh-agent is needed.
func TestCheckToolsReasons(t *testing.T) {
	cases := []struct {
		name       string
		gitVersion string // "" = no git on PATH
		want       workerproto.ToolStatus
	}{
		{name: "ready", gitVersion: "2.47.1", want: workerproto.ToolStatus{Name: "git", Version: "2.47.1", Ready: true}},
		{name: "too old", gitVersion: "2.31.9", want: workerproto.ToolStatus{Name: "git", Version: "2.31.9", Reason: workerproto.ReasonTooOld}},
		{name: "missing", want: workerproto.ToolStatus{Name: "git", Reason: workerproto.ReasonMissing}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Setenv("SSH_AUTH_SOCK", "")
			marker := ""
			if c.gitVersion != "" {
				marker = fakeGitOnPath(t, c.gitVersion, "")
			} else {
				dir := t.TempDir()
				marker = fakeGhOnPath(t, dir)
				t.Setenv("PATH", dir)
			}
			tools := checkTools(context.Background())
			if len(tools) != 1 || tools[0] != c.want {
				t.Fatalf("tools = %+v, want [%+v]", tools, c.want)
			}
			if err := workerproto.ValidateTools(tools); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(marker); err == nil {
				t.Fatal("checkTools ran gh")
			}
		})
	}
}

// The probe runs under git.HostToolEnv: no DD setting or agent key reaches
// git.
func TestCheckToolsEnv(t *testing.T) {
	t.Setenv("DEFECT_DRAINER_DATABASE_URL", "postgres://leak")
	t.Setenv("DEFECT_CHANNEL_SECRET", "leak")
	t.Setenv("ANTHROPIC_API_KEY", "sk-ant-leak")
	t.Setenv("XAI_API_KEY", "xai-leak")
	t.Setenv("DD_TEST_ACCOUNT_PASSWORD", "leak")
	gitEnv := filepath.Join(t.TempDir(), "git.env")
	fakeGitOnPath(t, "2.47.1", gitEnv)

	if g := toolByName(t, checkTools(context.Background()), "git"); !g.Ready {
		t.Fatalf("git = %+v", g)
	}
	b, err := os.ReadFile(gitEnv)
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(string(b), "\n") {
		key, _, _ := strings.Cut(line, "=")
		if strings.HasPrefix(key, "DEFECT_DRAINER_") || strings.HasPrefix(key, "DEFECT_CHANNEL_") ||
			key == "ANTHROPIC_API_KEY" || key == "XAI_API_KEY" || key == "DD_TEST_ACCOUNT_PASSWORD" {
			t.Errorf("%s reached git", key)
		}
	}
}

func TestVersionLess(t *testing.T) {
	for _, c := range []struct {
		a, b string
		want bool
	}{{"2.31.9", "2.32.0", true}, {"2.32", "2.32.0", false}, {"2.47.1", "2.32.0", false}, {"1.14.0", "2.0.0", true}, {"10.0", "9.9.9", false}} {
		if got := versionLess(c.a, c.b); got != c.want {
			t.Errorf("versionLess(%s, %s) = %v", c.a, c.b, got)
		}
	}
}

// hello carries gittoken.v1, and hello and status carry git only (§4.1,
// §4.2, §14).
func TestHelloAndStatusCarryTools(t *testing.T) {
	h := newHarness(t, harnessOpts{})
	defer h.stop()
	hello := h.fs.hellos[0]
	if !slices.Contains(hello.Capabilities, workerproto.CapGitTokenV1) {
		t.Fatalf("hello capabilities = %v", hello.Capabilities)
	}
	if len(hello.Tools) != 1 || hello.Tools[0].Name != workerproto.ToolGit {
		t.Fatalf("hello tools = %+v", hello.Tools)
	}
	if st := h.w.statusFrame(); len(st.Tools) != 1 || st.Tools[0].Name != workerproto.ToolGit {
		t.Fatalf("status tools = %+v", st.Tools)
	}
}
