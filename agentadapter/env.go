package agentadapter

import (
	"sort"
	"strings"
)

// credentialEnv is every adapter's native credential variable. A child gets
// only the names its own adapter keeps (ChildEnv's keep), and only when the
// caller passes them on.
var credentialEnv = []string{
	// Claude Code: credentials, and the routing/provider switches that would
	// send a key elsewhere or bill another account.
	"ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN", "CLAUDE_CODE_OAUTH_TOKEN", "CLAUDE_CONFIG_DIR",
	"ANTHROPIC_BASE_URL", "ANTHROPIC_CUSTOM_HEADERS", "ANTHROPIC_FOUNDRY_API_KEY",
	"CLAUDE_CODE_USE_BEDROCK", "CLAUDE_CODE_USE_VERTEX", "AWS_BEARER_TOKEN_BEDROCK",
	// Grok Build
	"XAI_API_KEY", "GROK_API_KEY",
	// Antigravity / Gemini
	"GEMINI_API_KEY", "GOOGLE_API_KEY",
	// Codex (later slice) and other gateways; denied to every child today.
	"OPENAI_API_KEY", "CODEX_API_KEY", "OPENROUTER_API_KEY",
}

// CredentialEnvNames is every adapter credential name, sorted.
func CredentialEnvNames() []string {
	out := append([]string(nil), credentialEnv...)
	sort.Strings(out)
	return out
}

// ChildEnv is the environment hygiene every child gets (agent, reviewer,
// verify, toolchain probe). From environ, which the caller read, it drops:
//   - every DEFECT_DRAINER_* and DEFECT_CHANNEL_* name, and DEFECTS_ROOT;
//   - DD_TEST_ACCOUNT_PASSWORD (a test job sets it again on purpose);
//   - every adapter credential name not in keep;
//   - the names in extra (the app's env: secret names).
//
// The caller adds its own deliberate variables after this.
func ChildEnv(environ, keep, extra []string) []string {
	keepSet := map[string]bool{}
	for _, k := range keep {
		keepSet[k] = true
	}
	drop := map[string]bool{"DD_TEST_ACCOUNT_PASSWORD": true, "DEFECTS_ROOT": true}
	for _, k := range credentialEnv {
		if !keepSet[k] {
			drop[k] = true
		}
	}
	for _, k := range extra {
		if k != "" {
			drop[k] = true
		}
	}
	out := make([]string, 0, len(environ))
	for _, e := range environ {
		key, _, ok := strings.Cut(e, "=")
		if !ok || drop[key] || ddName(key) {
			continue
		}
		out = append(out, e)
	}
	return out
}

func ddName(key string) bool {
	return strings.HasPrefix(key, "DEFECT_DRAINER_") || strings.HasPrefix(key, "DEFECT_CHANNEL_")
}

// GetEnv reads key from an environ slice (last one wins), never os.Getenv.
func GetEnv(environ []string, key string) string {
	v := ""
	for _, e := range environ {
		if k, val, ok := strings.Cut(e, "="); ok && k == key {
			v = val
		}
	}
	return v
}
