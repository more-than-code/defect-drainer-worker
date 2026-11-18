package git

import (
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"testing"
)

func TestAddGitConfigEnvExtendsHooksList(t *testing.T) {
	cmd := HostGit("--version")
	AddGitConfigEnv(cmd, "credential.helper", "", "http.https://example.test/.extraheader", "X-Test: 1")
	got := map[string]string{}
	for _, e := range cmd.Env {
		if k, v, ok := strings.Cut(e, "="); ok && strings.HasPrefix(k, "GIT_CONFIG_") {
			if _, dup := got[k]; dup {
				t.Fatalf("%s set twice", k)
			}
			got[k] = v
		}
	}
	n := 0
	for k := range got {
		if strings.HasPrefix(k, "GIT_CONFIG_KEY_") {
			n++
		}
	}
	if got["GIT_CONFIG_COUNT"] == "" || got["GIT_CONFIG_COUNT"] != strconv.Itoa(n) {
		t.Fatalf("count %q for %d keys", got["GIT_CONFIG_COUNT"], n)
	}
	last := n - 1
	if got["GIT_CONFIG_KEY_"+strconv.Itoa(last-1)] != "credential.helper" || got["GIT_CONFIG_VALUE_"+strconv.Itoa(last-1)] != "" {
		t.Errorf("credential.helper entry: %v", got)
	}
	if got["GIT_CONFIG_KEY_"+strconv.Itoa(last)] != "http.https://example.test/.extraheader" || got["GIT_CONFIG_VALUE_"+strconv.Itoa(last)] != "X-Test: 1" {
		t.Errorf("extraheader entry: %v", got)
	}
	if !slices.Contains(cmd.Env, "GIT_CONFIG_VALUE_"+strconv.Itoa(last-2)+"=/dev/null") {
		t.Errorf("hooks entry lost: %v", got)
	}
}

// git itself reads the added entries. (That the empty credential.helper
// resets a host helper is proven by behaviour, against a real HTTPS git
// server, in internal/worker TestTokenGitAgainstHTTPBackend: how git lists
// an empty entry differs between versions.)
func TestAddGitConfigEnvReachesGit(t *testing.T) {
	t.Setenv("GIT_CONFIG_GLOBAL", "/dev/null")
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	cmd := HostGit("config", "--get", "dd.test")
	cmd.Dir = t.TempDir() // outside any repository (CI's checkout is another uid's)
	AddGitConfigEnv(cmd, "dd.test", "v1")
	if out, err := cmd.Output(); err != nil || strings.TrimSpace(string(out)) != "v1" {
		t.Fatalf("dd.test = %q, %v", out, err)
	}
}

func TestDropEnv(t *testing.T) {
	cmd := exec.Command("true")
	cmd.Env = []string{"A=1", "GH_TOKEN=x", "B=2", "SSH_AUTH_SOCK=/s"}
	DropEnv(cmd, "GH_TOKEN", "SSH_AUTH_SOCK")
	if want := []string{"A=1", "B=2"}; !slices.Equal(cmd.Env, want) {
		t.Fatalf("env %v, want %v", cmd.Env, want)
	}
}
