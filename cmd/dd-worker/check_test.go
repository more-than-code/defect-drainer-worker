package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/more-than-code/defect-drainer-worker/internal/worker"
	"github.com/more-than-code/defect-drainer-worker/workerproto"
)

const validPolicy = "enabled = true\norgs = [\"org_a\"]\n"

var reasonLine = regexp.MustCompile(`^bad_[a-z0-9_]{1,64}\n$`)

// TestPolicyCheckStdinReasons runs every refusal through --file -: exit 2
// and exactly one bad_* line on stderr, nothing on stdout, and nothing from
// the candidate echoed back.
func TestPolicyCheckStdinReasons(t *testing.T) {
	long := "https://bot.example/" + strings.Repeat("a", workerproto.MaxManagedURL)
	for _, c := range []struct{ body, reason string }{
		{"enabled = true\norgs = []\n", "bad_orgs_empty"},
		{"enabled = true\n", "bad_orgs_empty"},
		{"enabled = true\norgs = [\"org.a\"]\n", "bad_org"},
		{validPolicy + "adapters = [\"vim\"]\n", "bad_adapters"},
		{validPolicy + "adapters = [\"claude\", \"claude\"]\n", "bad_adapters"},
		{validPolicy + "min_sandbox = \"open\"\n", "bad_min_sandbox"},
		{validPolicy + "max_turns = 0\n", "bad_max_turns"},
		{validPolicy + "max_wall_sec = 59\n", "bad_max_wall_sec"},
		{validPolicy + "max_jobs = 17\n", "bad_max_jobs"},
		{validPolicy + "agent_allowed_domains = [\"*.example.com\"]\n", "bad_agent_allowed_domains"},
		{validPolicy + "max_cost_usd = 0\n", "bad_max_cost_usd"},
		{validPolicy + "max_cost_usd = 101\n", "bad_max_cost_usd"},
		{validPolicy + "agent_browser = \"chromium\"\n", "bad_agent_browser"},
		{validPolicy + "agent_deny_sockets = [\"/\"]\n", "bad_agent_deny_sockets"},
		{validPolicy + "allowed_models = []\n", "bad_allowed_models"},
		{validPolicy + "allowed_models = [\"-x\"]\n", "bad_allowed_models"},
		{validPolicy + "repos = [\"file:///x\"]\n", "bad_repos"},
		{validPolicy + "managed_url = \"http://bot.example/h1\"\n", "bad_managed_url"},
		{validPolicy + "managed_url = \"https://user:pw@bot.example/h1\"\n", "bad_managed_url"},
		{validPolicy + "managed_url = \"https:///nohost\"\n", "bad_managed_url"},
		{validPolicy + "managed_url = \"javascript:alert(1)\"\n", "bad_managed_url"},
		{validPolicy + "managed_url = \"" + long + "\"\n", "bad_managed_url"},
		{validPolicy + "runners = [\"claude\"]\n", "bad_unknown_key"},
		{"enabled = \n", "bad_parse"},
		{validPolicy + strings.Repeat("#", 64*1024), "bad_parse"},
		{"", "bad_orgs_empty"},
	} {
		code, out, errOut := runCmd([]string{"policy", "check", "--file", "-"}, c.body)
		if code != exitInvalid || errOut != c.reason+"\n" || !reasonLine.MatchString(errOut) || out != "" {
			t.Errorf("%.60q: %d out %q err %q, want %s", c.body, code, out, errOut, c.reason)
		}
	}
}

func TestPolicyCheckStdinValid(t *testing.T) {
	body := validPolicy + "managed_url = \"https://bot.example/hosts/h1/files/policy\"\n"
	code, out, errOut := runCmd([]string{"policy", "check", "--file", "-"}, body)
	sum := sha256.Sum256([]byte(body))
	if code != exitOK || errOut != "" || out != "stdin: valid (enabled=true, sha256 "+hex.EncodeToString(sum[:])+")\nverify=sandbox\n" {
		t.Fatalf("%d %q %q", code, out, errOut)
	}
	if strings.Contains(out, "bot.example") || strings.Contains(out, "org_a") {
		t.Fatal("check printed the candidate's values")
	}
}

// Sandboxed verify (V2, M4): the verify line, and the deprecated
// allow_verify_commands: an explicit false is "off" unless verify_commands
// is set.
func TestPolicyCheckVerifyLines(t *testing.T) {
	for _, c := range []struct{ extra, want string }{
		{`verify_commands = "off"` + "\n", "verify=off\n"},
		{"allow_verify_commands = false\n", "verify=off\ndeprecated: allow_verify_commands (use verify_commands)\n"},
		{"allow_verify_commands = true\n", "verify=sandbox\ndeprecated: allow_verify_commands (use verify_commands)\n"},
		{"allow_verify_commands = false\nverify_commands = \"sandbox\"\n", "verify=sandbox\ndeprecated: allow_verify_commands (use verify_commands)\n"},
		{"allow_verify_commands = true\nverify_commands = \"off\"\n", "verify=off\ndeprecated: allow_verify_commands (use verify_commands)\n"},
	} {
		code, out, errOut := runCmd([]string{"policy", "check", "--file", "-"}, validPolicy+c.extra)
		if _, rest, _ := strings.Cut(out, ")\n"); code != exitOK || errOut != "" || rest != c.want {
			t.Errorf("%q: %d %q %q", c.extra, code, out, errOut)
		}
	}
	code, _, errOut := runCmd([]string{"policy", "check", "--file", "-"}, validPolicy+`verify_commands = "unsandboxed"`+"\n")
	if code != exitInvalid || errOut != "bad_verify_commands\n" {
		t.Errorf("bad value: %d %q", code, errOut)
	}
}

// TestPolicyCheckFile is --file <path>: the live owner and mode rules, a
// missing file exit 1, and usage errors exit 1.
func TestPolicyCheckFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "candidate.toml")
	if err := os.WriteFile(path, []byte(validPolicy), 0o644); err != nil {
		t.Fatal(err)
	}
	if os.Geteuid() != 0 {
		if code, _, errOut := runCmd([]string{"policy", "check", "--file", path}, ""); code != exitInvalid || errOut != "bad_not_root\n" {
			t.Fatalf("non-root: %d %q", code, errOut)
		}
	}
	if code, out, _ := runCmd([]string{"policy", "check", "--syntax-only", "--file", path}, ""); code != exitOK || !strings.Contains(out, "valid") {
		t.Fatalf("syntax-only: %d %q", code, out)
	}
	if code, out, errOut := runCmd([]string{"policy", "check", "--file", filepath.Join(dir, "none.toml")}, ""); code != exitFail || out != "" || !strings.Contains(errOut, "cannot read") {
		t.Fatalf("missing: %d %q %q", code, out, errOut)
	}
	for _, args := range [][]string{
		{"policy", "check", "--file", "-", "extra"},
		{"policy", "check", "--file", "-", "--bogus"},
		{"policy", "check", "--file"},
	} {
		if code, _, _ := runCmd(args, validPolicy); code != exitFail {
			t.Errorf("%v: %d, want %d", args, code, exitFail)
		}
	}
	// Without --file, a bad flag keeps policy check's own usage code.
	if code, _, _ := runCmd([]string{"policy", "check", "--bogus"}, ""); code != exitUsage {
		t.Fatalf("plain usage: %d", code)
	}
}

func TestSecretsCheck(t *testing.T) {
	const value = "hunter2-very-secret"
	for _, c := range []struct{ body, reason string }{
		{"ANTHROPIC_API_KEY=" + value + "\n# note\nexport TEST_PW=\"" + value + "\"\n", ""},
		{"", ""},
		{"1BAD=" + value + "\n", "bad_name"},
		{"BAD-NAME=" + value + "\n", "bad_name"},
		{"A=" + value + "\nA=" + value + "\n", "bad_duplicate"},
		{"NOEQUALS " + value + "\n", "bad_format"},
		{"A=\"" + value + "\\\"\n", "bad_format"},
		{"A=" + strings.Repeat("x", worker.MaxSecretsBytes) + "\n", "bad_format"},
	} {
		code, out, errOut := runCmd([]string{"secrets", "check", "--file", "-"}, c.body)
		if strings.Contains(out+errOut, value) {
			t.Fatalf("%.40q: printed a value", c.body)
		}
		if c.reason == "" {
			if code != exitOK || errOut != "" {
				t.Errorf("%.40q: %d %q", c.body, code, errOut)
			}
			continue
		}
		if code != exitInvalid || errOut != c.reason+"\n" || out != "" {
			t.Errorf("%.40q: %d out %q err %q, want %s", c.body, code, out, errOut, c.reason)
		}
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "secrets.env")
	if err := os.WriteFile(path, []byte("A="+value+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if code, _, errOut := runCmd([]string{"secrets", "check", "--file", path}, ""); code != exitOK {
		t.Fatalf("0600 own file: %d %q", code, errOut)
	}
	link := filepath.Join(dir, "link.env")
	if err := os.Symlink(path, link); err != nil {
		t.Fatal(err)
	}
	if code, _, errOut := runCmd([]string{"secrets", "check", "--file", link}, ""); code != exitInvalid || errOut != "bad_symlink\n" {
		t.Fatalf("symlink: %d %q", code, errOut)
	}
	if err := os.Chmod(path, 0o640); err != nil {
		t.Fatal(err)
	}
	if code, _, errOut := runCmd([]string{"secrets", "check", "--file", path}, ""); code != exitInvalid || errOut != "bad_mode\n" {
		t.Fatalf("mode: %d %q", code, errOut)
	}
	if code, _, _ := runCmd([]string{"secrets", "check", "--file", filepath.Join(dir, "none")}, ""); code != exitFail {
		t.Fatalf("missing: %d", code)
	}
	for _, args := range [][]string{{"secrets"}, {"secrets", "check"}, {"secrets", "check", "--file", "-", "x"}} {
		if code, _, _ := runCmd(args, ""); code != exitFail {
			t.Errorf("%v: %d", args, code)
		}
	}
}

// healthDirs writes policy.toml and a status.json as run would.
func healthDirs(t *testing.T, policy string, snap *worker.Snapshot) []string {
	t.Helper()
	cfg, st := dirs(t)
	for _, d := range []string{cfg, st} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(cfg, "policy.toml"), []byte(policy), 0o644); err != nil {
		t.Fatal(err)
	}
	if snap != nil {
		b, _ := json.Marshal(snap)
		if err := os.WriteFile(filepath.Join(st, "status.json"), b, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return []string{"--config-dir", cfg, "--state-dir", st}
}

func policyHash(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

func TestHealthWait(t *testing.T) {
	defer func(d time.Duration) { healthPoll = d }(healthPoll)
	healthPoll = 10 * time.Millisecond
	good := workerproto.Policy{Valid: true, Enabled: true, Hash: policyHash(validPolicy)}
	for _, c := range []struct {
		name   string
		snap   *worker.Snapshot
		reason string
	}{
		{"healthy", &worker.Snapshot{Connected: true, Policy: good}, ""},
		{"no status file", nil, "not_running"},
		{"old file loaded", &worker.Snapshot{Connected: true, Policy: workerproto.Policy{Valid: true, Enabled: true, Hash: policyHash("old")}}, "not_running"},
		{"policy invalid", &worker.Snapshot{Connected: true, Policy: workerproto.Policy{Hash: good.Hash}}, "policy_invalid"},
		{"policy disabled", &worker.Snapshot{Connected: true, Policy: workerproto.Policy{Valid: true, Hash: good.Hash}}, "policy_invalid"},
		{"not connected", &worker.Snapshot{Policy: good}, "not_connected"},
	} {
		common := healthDirs(t, validPolicy, c.snap)
		start := time.Now()
		code, out, errOut := runCmd(append([]string{"health", "--wait", "100ms"}, common...), "")
		if c.reason == "" {
			if code != exitOK || errOut != "" || out != "" {
				t.Errorf("%s: %d %q", c.name, code, errOut)
			}
			continue
		}
		if code != exitUnhealthy || errOut != c.reason+"\n" || out != "" {
			t.Errorf("%s: %d %q, want %s", c.name, code, errOut, c.reason)
		}
		if el := time.Since(start); el < 80*time.Millisecond || el > 5*time.Second {
			t.Errorf("%s: waited %s", c.name, el)
		}
	}
}

// TestHealthWaitSeesTheWorkerComeUp writes a healthy status while health
// waits, as a restarted worker does once it connects.
func TestHealthWaitSeesTheWorkerComeUp(t *testing.T) {
	defer func(d time.Duration) { healthPoll = d }(healthPoll)
	healthPoll = 10 * time.Millisecond
	common := healthDirs(t, validPolicy, &worker.Snapshot{Policy: workerproto.Policy{Valid: true, Enabled: true, Hash: policyHash(validPolicy)}})
	status := filepath.Join(common[3], "status.json")
	go func() {
		time.Sleep(50 * time.Millisecond)
		b, _ := json.Marshal(worker.Snapshot{Connected: true, Policy: workerproto.Policy{Valid: true, Enabled: true, Hash: policyHash(validPolicy)}})
		_ = os.WriteFile(status+".tmp", b, 0o600)
		_ = os.Rename(status+".tmp", status)
	}()
	if code, _, errOut := runCmd(append([]string{"health", "--wait", "5s"}, common...), ""); code != exitOK {
		t.Fatalf("%d %q", code, errOut)
	}
}

func TestHealthIdleAndUsage(t *testing.T) {
	idle := healthDirs(t, validPolicy, &worker.Snapshot{Connected: true})
	if code, _, errOut := runCmd(append([]string{"health", "--idle"}, idle...), ""); code != exitOK || errOut != "" {
		t.Fatalf("idle: %d %q", code, errOut)
	}
	busy := healthDirs(t, validPolicy, &worker.Snapshot{Connected: true, Busy: []string{"job_1"}})
	if code, _, errOut := runCmd(append([]string{"health", "--idle"}, busy...), ""); code != exitBusy || errOut != "busy\n" {
		t.Fatalf("busy: %d %q", code, errOut)
	}
	none := healthDirs(t, validPolicy, nil)
	if code, _, _ := runCmd(append([]string{"health", "--idle"}, none...), ""); code != exitOK {
		t.Fatalf("no status file: %d", code)
	}
	for _, args := range [][]string{
		{"health"},
		{"health", "--idle", "--wait", "1s"},
		{"health", "--wait", "11m"},
		{"health", "--wait", "-1s"},
		{"health", "--wait", "soon"},
		{"health", "--idle", "x"},
	} {
		if code, _, _ := runCmd(args, ""); code != exitFail {
			t.Errorf("%v: %d", args, code)
		}
	}
}
