package git

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/more-than-code/defect-drainer-worker/agentadapter"
	"github.com/more-than-code/defect-drainer-worker/agentadapter/antigravity"
	"github.com/more-than-code/defect-drainer-worker/agentadapter/claude"
	"github.com/more-than-code/defect-drainer-worker/agentadapter/grok"
)

// hostEnv is what serve's own environment might hold: DD's secrets and every
// coding agent's credential.
func setHostEnv(t *testing.T) {
	t.Helper()
	for k, v := range map[string]string{
		"DEFECT_DRAINER_DATABASE_URL":   "postgres://dd:secret@db/dd",
		"DEFECT_DRAINER_CLAUDE_API_KEY": "sk-dd-claude",
		"DEFECT_CHANNEL_TOKEN":          "legacy-token",
		"DEFECTS_ROOT":                  "/srv/defects",
		"ANTHROPIC_API_KEY":             "sk-host-anthropic",
		"CLAUDE_CODE_OAUTH_TOKEN":       "host-oauth",
		"XAI_API_KEY":                   "xai-host",
		"GEMINI_API_KEY":                "gemini-host",
		"OPENAI_API_KEY":                "openai-host",
	} {
		t.Setenv(k, v)
	}
	// The test runner's own Postgres DSNs (README → Run) are not serve's
	// env; left in, the "no postgres:// value" check below would flag them.
	for _, k := range []string{"DD_MIGRATE_TEST_DSN", "DD_ADVISORY_LOCK_TEST_DSN"} {
		t.Setenv(k, "") // restores the value after the test
		os.Unsetenv(k)
	}
}

// envDumpBin writes a fake agent that dumps its environment to proof.
func envDumpBin(t *testing.T, proof string) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "fake-agent")
	script := "#!/bin/sh\nenv > '" + proof + "'\nexit 0\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return bin
}

func readEnvDump(t *testing.T, proof string) map[string]string {
	t.Helper()
	b, err := os.ReadFile(proof)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	for _, line := range strings.Split(string(b), "\n") {
		if k, v, ok := strings.Cut(line, "="); ok {
			out[k] = v
		}
	}
	return out
}

type childCase struct {
	name    string
	adapter func(bin string, t *testing.T) agentadapter.Adapter
	cred    func(t *testing.T) agentadapter.Credential
	// keep is the credential name and value this adapter's child may hold.
	keep map[string]string
}

func childCases() []childCase {
	return []childCase{
		{
			name: "grok",
			adapter: func(bin string, t *testing.T) agentadapter.Adapter {
				return grok.New(grok.Config{Bin: bin, Home: t.TempDir()})
			},
			cred: func(*testing.T) agentadapter.Credential { return agentadapter.Credential{} },
			keep: map[string]string{"XAI_API_KEY": "xai-host"},
		},
		{
			name: "claude",
			adapter: func(bin string, t *testing.T) agentadapter.Adapter {
				return claude.New(claude.Config{Bin: bin})
			},
			cred: func(t *testing.T) agentadapter.Credential {
				return agentadapter.Credential{Mode: agentadapter.APIKey, Key: "sk-dd-claude", ConfigDir: filepath.Join(t.TempDir(), "cfg")}
			},
			// The configured key under its native name, never the host's.
			keep: map[string]string{"ANTHROPIC_API_KEY": "sk-dd-claude"},
		},
		{
			name: "antigravity",
			adapter: func(bin string, t *testing.T) agentadapter.Adapter {
				return antigravity.New(antigravity.Config{Bin: bin, Home: t.TempDir()})
			},
			cred: func(*testing.T) agentadapter.Credential {
				return agentadapter.Credential{Mode: agentadapter.Subscription}
			},
			keep: map[string]string{},
		},
	}
}

// assertHygiene checks one child's env (criterion 7).
func assertHygiene(t *testing.T, env map[string]string, keep map[string]string, ddAllowed []string) {
	t.Helper()
	var dd []string
	for k := range env {
		if strings.HasPrefix(k, "DEFECT_DRAINER_") {
			dd = append(dd, k)
		}
		if strings.HasPrefix(k, "DEFECT_CHANNEL_") || k == "DEFECTS_ROOT" {
			t.Errorf("child has %s", k)
		}
	}
	sort.Strings(dd)
	if strings.Join(dd, ",") != strings.Join(ddAllowed, ",") {
		t.Errorf("DD names in child = %v, want %v", dd, ddAllowed)
	}
	for _, name := range agentadapter.CredentialEnvNames() {
		want, ok := keep[name]
		got, has := env[name]
		switch {
		case ok && got != want:
			t.Errorf("%s = %q, want %q", name, got, want)
		case !ok && has && name != "CLAUDE_CONFIG_DIR":
			t.Errorf("child has foreign credential %s", name)
		}
	}
	for _, v := range env {
		if strings.Contains(v, "postgres://") || v == "sk-dd-claude" && keep["ANTHROPIC_API_KEY"] == "" {
			t.Errorf("child env carries a DD secret value")
		}
	}
}

func TestChildEnvImplementerPerAdapter(t *testing.T) {
	for _, c := range childCases() {
		t.Run(c.name, func(t *testing.T) {
			setHostEnv(t)
			proof := filepath.Join(t.TempDir(), "env")
			a := c.adapter(envDumpBin(t, proof), t)
			wts := []WorktreeBinding{{Repo: "demo", WorktreeAbs: t.TempDir(), PrimaryAbs: t.TempDir(), Branch: "b"}}
			cmd, flush, err := StartCodingAgent(t.TempDir(), "BATCH-env", t.TempDir(), wts, "strict", nil, nil, &CodingAgentOpts{
				Adapter: a, Credential: c.cred(t), StateDir: t.TempDir(), Timeout: time.Minute,
			})
			if err != nil {
				t.Fatal(err)
			}
			_ = cmd.Wait()
			flush()
			var dd []string
			if c.name == "grok" {
				// The one deliberate, non-secret DD name, grok only (Main Dev,
				// 2026-11-14).
				dd = []string{"DEFECT_DRAINER_WORKTREES"}
			}
			assertHygiene(t, readEnvDump(t, proof), c.keep, dd)
		})
	}
}

func TestChildEnvReviewerPerAdapter(t *testing.T) {
	for _, c := range childCases() {
		t.Run(c.name, func(t *testing.T) {
			setHostEnv(t)
			proof := filepath.Join(t.TempDir(), "env")
			a := c.adapter(envDumpBin(t, proof), t)
			_, err := StartReviewAgent(ReviewAgentOpts{
				Cwd: t.TempDir(), Adapter: a, Credential: c.cred(t), StateDir: t.TempDir(),
				Worktrees: []string{t.TempDir()}, Wall: time.Minute,
			})
			if err != nil {
				t.Fatal(err)
			}
			assertHygiene(t, readEnvDump(t, proof), c.keep, nil)
		})
	}
}

// Verify commands and the toolchain probe run with FilterInheritedEnv: no DD
// name and no coding-agent credential at all.
func TestChildEnvVerifyAndToolchainProbe(t *testing.T) {
	setHostEnv(t)
	env := map[string]string{}
	for _, e := range FilterInheritedEnv(os.Environ(), []string{"APP_SECRET"}) {
		k, v, _ := strings.Cut(e, "=")
		env[k] = v
	}
	assertHygiene(t, env, map[string]string{}, nil)
}

// The adapter's key never reaches agent.log, the job log or a decoded event,
// even when the CLI prints it.
func TestCredentialRedactedFromLogs(t *testing.T) {
	const key = "sk-ant-redact-me-0123456789"
	bin := filepath.Join(t.TempDir(), "fake-claude")
	script := "#!/bin/sh\n" +
		"printf '%s\\n' '{\"type\":\"assistant\",\"message\":{\"content\":[{\"type\":\"text\",\"text\":\"key is " + key + "\"}]}}'\n" +
		"printf 'leak %s\\n' \"$ANTHROPIC_API_KEY\" >&2\n" +
		"printf '%s\\n' '{\"type\":\"result\",\"subtype\":\"success\",\"is_error\":false,\"session_id\":\"023759b8-8496-44f8-9ef8-e6fb4e1e08e4\",\"result\":\"" + key + "\"}'\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	handoff := t.TempDir()
	// stdout and stderr forward on separate goroutines (production's
	// appendLog takes the job lock), so the collector locks too.
	var mu sync.Mutex
	var lines []string
	collect := func(l string) { mu.Lock(); lines = append(lines, l); mu.Unlock() }
	wts := []WorktreeBinding{{Repo: "demo", WorktreeAbs: t.TempDir(), PrimaryAbs: t.TempDir(), Branch: "b"}}
	var end AgentEnd
	cmd, flush, err := StartCodingAgent(handoff, "BATCH-red", t.TempDir(), wts, "strict", collect, collect, &CodingAgentOpts{
		Adapter:    claude.New(claude.Config{Bin: bin}),
		Credential: agentadapter.Credential{Mode: agentadapter.APIKey, Key: key, ConfigDir: filepath.Join(t.TempDir(), "cfg")},
		StateDir:   t.TempDir(),
		OnEnd:      func(e AgentEnd) { end = e },
	})
	if err != nil {
		t.Fatal(err)
	}
	_ = cmd.Wait()
	flush()
	log, _ := os.ReadFile(filepath.Join(handoff, "agent.log"))
	all := string(log) + strings.Join(lines, "\n")
	if strings.Contains(all, key) || strings.Contains(all, "sk-ant-redact") {
		t.Fatalf("key leaked:\n%s", all)
	}
	if !strings.Contains(string(log), "[redacted]") || !strings.Contains(strings.Join(lines, "\n"), "key is [redacted]") {
		t.Fatalf("expected redaction marks:\nlog=%s\nlines=%q", log, lines)
	}
	if end.SessionID != "023759b8-8496-44f8-9ef8-e6fb4e1e08e4" {
		t.Fatalf("end = %+v", end)
	}
}

// A subscription token the claude child keeps is redacted like the key.
func TestOAuthTokenRedactedFromLogs(t *testing.T) {
	const tok = "sk-ant-oat01-redact-me-0123456789"
	t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", tok)
	bin := filepath.Join(t.TempDir(), "fake-claude")
	script := "#!/bin/sh\nprintf 'tok %s\\n' \"$CLAUDE_CODE_OAUTH_TOKEN\" >&2\n" +
		"printf '%s\\n' '{\"type\":\"assistant\",\"message\":{\"content\":[{\"type\":\"text\",\"text\":\"t=" + tok + "\"}]}}'\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	handoff := t.TempDir()
	// stdout and stderr forward on separate goroutines (production's
	// appendLog takes the job lock), so the collector locks too.
	var mu sync.Mutex
	var lines []string
	collect := func(l string) { mu.Lock(); lines = append(lines, l); mu.Unlock() }
	wts := []WorktreeBinding{{Repo: "demo", WorktreeAbs: t.TempDir(), PrimaryAbs: t.TempDir(), Branch: "b"}}
	cmd, flush, err := StartCodingAgent(handoff, "BATCH-tok", t.TempDir(), wts, "strict", collect, collect, &CodingAgentOpts{
		Adapter: claude.New(claude.Config{Bin: bin}), Credential: agentadapter.Credential{Mode: agentadapter.Subscription}, StateDir: t.TempDir(),
	})
	if err != nil {
		t.Fatal(err)
	}
	_ = cmd.Wait()
	flush()
	log, _ := os.ReadFile(filepath.Join(handoff, "agent.log"))
	all := string(log) + strings.Join(lines, "\n")
	if strings.Contains(all, tok) || !strings.Contains(all, "[redacted]") {
		t.Fatalf("token handling:\n%s", all)
	}
}
