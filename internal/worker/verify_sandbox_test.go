package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	dexec "github.com/more-than-code/defect-drainer-worker/exec"
	"github.com/more-than-code/defect-drainer-worker/internal/sandbox"
	"github.com/more-than-code/defect-drainer-worker/workerproto"
)

func lastHello(t *testing.T, h *harness) workerproto.Hello {
	t.Helper()
	frames := h.fs.frames(workerproto.TypeHello)
	var f workerproto.Hello
	if err := json.Unmarshal(frames[len(frames)-1].raw, &f); err != nil {
		t.Fatal(err)
	}
	return f
}

// V9: verify_sandbox.v1 only while the self-test passes; the hello policy
// carries verify_commands.
func TestVerifySandboxCapability(t *testing.T) {
	h := newHarness(t, harnessOpts{})
	f := lastHello(t, h)
	if !slices.Contains(f.Capabilities, workerproto.CapVerifySandboxV1) || f.Policy.VerifyCommands != "sandbox" || !f.Policy.AllowVerifyCommands {
		t.Fatalf("ready: caps %v policy %+v", f.Capabilities, f.Policy)
	}
	if f.VerifySandbox == nil || !f.VerifySandbox.Ready || f.VerifySandbox.Reason != "" {
		t.Fatalf("ready: verify_sandbox %+v", f.VerifySandbox)
	}
	if !strings.Contains(h.log.String(), "verify sandbox: ready") {
		t.Fatalf("log:\n%s", h.log.String())
	}

	down := newHarness(t, harnessOpts{verify: func(context.Context) (sandbox.Launcher, error) {
		return sandbox.Launcher{}, fmt.Errorf("%w: bwrap: not found", sandbox.ErrUnavailable)
	}})
	f = lastHello(t, down)
	if slices.Contains(f.Capabilities, workerproto.CapVerifySandboxV1) || f.Policy.VerifyCommands != "sandbox" {
		t.Fatalf("unavailable: caps %v policy %+v", f.Capabilities, f.Policy)
	}
	if f.VerifySandbox == nil || f.VerifySandbox.Ready || f.VerifySandbox.Reason != "bwrap: not found" {
		t.Fatalf("unavailable: verify_sandbox %+v", f.VerifySandbox)
	}
	if !strings.Contains(down.log.String(), "verify sandbox: unavailable (bwrap: not found)") {
		t.Fatalf("log:\n%s", down.log.String())
	}

	off := newHarness(t, harnessOpts{policy: strings.Replace(defaultPolicy, "allow_verify_commands = true", `verify_commands = "off"`, 1)})
	if f := lastHello(t, off); f.Policy.VerifyCommands != "off" || f.Policy.AllowVerifyCommands {
		t.Fatalf("off: policy %+v", f.Policy)
	}
}

// V2/acceptance 5: with the sandbox unavailable the checks are skipped with
// verify_sandbox_unavailable, none of them runs, and nothing resolves.
func TestVerifySkippedSandboxUnavailable(t *testing.T) {
	h := newHarness(t, harnessOpts{verify: func(context.Context) (sandbox.Launcher, error) {
		return sandbox.Launcher{}, errors.New("self-test: egress: a refused CONNECT was not denied")
	}})
	spec := fixSpec("bjob_sv_unavail")
	marker := h.tmp + "/should-not-run"
	spec.Project.VerifyCommands = []dexec.VerifyCommand{{Repo: "app", Command: "touch " + marker}}
	h.offerJob(spec, nil)
	h.waitDone(spec.JobID, 60*time.Second)
	events := h.fs.jobEvents(spec.JobID)
	log := joinLines(events)
	for _, want := range []string{
		"verify skipped: sandbox unavailable (self-test: egress: a refused CONNECT was not denied)",
		"verify: skipped by verify_sandbox_unavailable (1 command(s) configured)",
	} {
		if !strings.Contains(log, want) {
			t.Errorf("log lacks %q:\n%s", want, log)
		}
	}
	hv := lastHarvest(events)
	if hv == nil || hv.AllowResolve || hv.Verification == nil || hv.Verification.Ran || hv.Verification.SkippedReason != workerproto.VerifySkippedSandboxUnavailable {
		t.Fatalf("harvest %+v", hv)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("a verify command ran without the sandbox")
	}
}

// V6/V7: every check, baseline included, goes through bwrap with the built
// environment: no secrets.env name, no DD name, the proxy variables and
// the caches set.
func TestVerifyRunsThroughSandbox(t *testing.T) {
	h := newHarness(t, harnessOpts{secrets: "ANTHROPIC_API_KEY=sk-test-secret\nOTHER_SECRET=hunter2\n"})
	spec := fixSpec("bjob_sv_through")
	envOut := h.tmp + "/env.txt"
	spec.Project.VerifyCommands = []dexec.VerifyCommand{{Repo: "app", Command: "env > " + envOut + "; pwd >> " + envOut}}
	h.offerJob(spec, nil)
	h.waitDone(spec.JobID, 60*time.Second)
	hv := lastHarvest(h.fs.jobEvents(spec.JobID))
	if hv == nil || hv.Verification == nil || !hv.Verification.Ran || !hv.Verification.Ok {
		t.Fatalf("harvest %+v\n%s", hv, joinLines(h.fs.jobEvents(spec.JobID)))
	}
	runs, err := os.ReadFile(h.bwrapLog)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSuffix(string(runs), "\x1e"), "\x1e")
	if len(lines) != 2 { // baseline and after the fix
		t.Fatalf("%d bwrap runs, want 2:\n%s", len(lines), runs)
	}
	args := strings.Split(lines[1], "\x1f")
	joined := strings.Join(args, " ")
	for _, want := range []string{"--unshare-all --unshare-net --die-with-parent --new-session --cap-drop ALL --ro-bind / /", "--clearenv", "/run/dd-verify"} {
		if !strings.Contains(joined, want) {
			t.Errorf("bwrap args lack %q:\n%s", want, joined)
		}
	}
	for _, p := range []string{h.paths.StateDir, h.paths.ConfigDir} {
		if !hiddenIn(args, realPath(t, p)) {
			t.Errorf("%s not hidden:\n%s", p, joined)
		}
	}
	env, err := os.ReadFile(envOut)
	if err != nil {
		t.Fatal(err)
	}
	got := string(env)
	for _, bad := range []string{"sk-test-secret", "hunter2", "DEFECT_DRAINER_", "DD_WORKER_", "DD_TEST_ACCOUNT_PASSWORD"} {
		if strings.Contains(got, bad) {
			t.Errorf("verify env carries %q:\n%s", bad, got)
		}
	}
	for _, want := range []string{"CI=1", "HTTPS_PROXY=" + sandbox.ProxyURL, "https_proxy=" + sandbox.ProxyURL, "HTTP_PROXY=", "TMPDIR=" + h.paths.JobDir(spec.JobID) + "/verify-", "XDG_CACHE_HOME=", "GOMODCACHE=", "npm_config_cache="} {
		if !strings.Contains(got, want) {
			t.Errorf("verify env lacks %q:\n%s", want, got)
		}
	}
}

func realPath(t *testing.T, p string) string {
	t.Helper()
	r, err := filepath.EvalSymlinks(p)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// The shared name with the control plane (exec cannot import workerproto).
func TestVerifySkipReasonParity(t *testing.T) {
	if dexec.VerifySkippedSandboxUnavailable != workerproto.VerifySkippedSandboxUnavailable {
		t.Fatal("exec and workerproto disagree on verify_sandbox_unavailable")
	}
}

func TestVerifyCommandsPolicy(t *testing.T) {
	base := "enabled = true\norgs = [\"org_a\"]\n"
	for _, c := range []struct {
		body, want, reason string
		set                bool
	}{
		{base, "sandbox", "", false},
		{base + "verify_commands = \"sandbox\"\n", "sandbox", "", false},
		{base + "verify_commands = \"off\"\n", "off", "", false},
		{base + "allow_verify_commands = false\n", "off", "", true},
		{base + "allow_verify_commands = true\n", "sandbox", "", true},
		{base + "allow_verify_commands = false\nverify_commands = \"sandbox\"\n", "sandbox", "", true},
		{base + "allow_verify_commands = true\nverify_commands = \"off\"\n", "off", "", true},
		{base + "verify_commands = \"unsandboxed\"\n", "", "bad_verify_commands", false},
		{base + "verify_commands = \"\"\n", "", "bad_verify_commands", false},
	} {
		p := CheckPolicyData([]byte(c.body))
		if c.reason != "" {
			if p.Valid || p.Reason != c.reason {
				t.Errorf("%q: valid=%v reason %q, want %q", c.body, p.Valid, p.Reason, c.reason)
			}
			continue
		}
		if !p.Valid || p.VerifyCommands != c.want || p.AllowVerifySet != c.set {
			t.Errorf("%q: %+v", c.body, p)
		}
	}
}

// hiddenIn reports whether args (from Args) hide p: a tmpfs on p or on one
// of its parents, or /dev/null over p.
func hiddenIn(args []string, p string) bool {
	for i := 0; i+1 < len(args); i++ {
		switch {
		case args[i] == "--tmpfs" && (p == args[i+1] || strings.HasPrefix(p, args[i+1]+"/")):
			return true
		case args[i] == "--ro-bind" && args[i+1] == "/dev/null" && i+2 < len(args) && args[i+2] == p:
			return true
		}
	}
	return false
}
