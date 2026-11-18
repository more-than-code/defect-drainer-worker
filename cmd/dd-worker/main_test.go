package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/more-than-code/defect-drainer-worker/internal/botagent/config"
)

const cred = "dd_wrk_" + "statuscredentialstatuscredential"

func dirs(t *testing.T) (string, string) {
	t.Helper()
	tmp := t.TempDir()
	return filepath.Join(tmp, "etc"), filepath.Join(tmp, "state")
}

func runCmd(args []string, stdin string) (int, string, string) {
	var out, errb bytes.Buffer
	code := run(args, strings.NewReader(stdin), &out, &errb)
	return code, out.String(), errb.String()
}

func TestPolicyCheckExitCodes(t *testing.T) {
	cfg, st := dirs(t)
	common := []string{"--config-dir", cfg, "--state-dir", st}
	if code, out, _ := runCmd(append([]string{"policy", "check"}, common...), ""); code != exitFail || !strings.Contains(out, "missing") {
		t.Fatalf("missing policy: %d %q", code, out)
	}
	if err := os.MkdirAll(cfg, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(cfg, "policy.toml")
	if err := os.WriteFile(path, []byte("enabled = true\norgs = [\"org_a\"]\nallow_ship = false\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if code, out, _ := runCmd(append([]string{"policy", "check", "--syntax-only"}, common...), ""); code != exitOK || !strings.Contains(out, "valid") {
		t.Fatalf("syntax-only: %d %q", code, out)
	}
	if os.Geteuid() != 0 {
		if code, out, _ := runCmd(append([]string{"policy", "check"}, common...), ""); code != exitFail || !strings.Contains(out, "not_root") {
			t.Fatalf("non-root file: %d %q", code, out)
		}
	}
	if err := os.WriteFile(path, []byte("runners = [\"claude\"]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if code, out, _ := runCmd(append([]string{"policy", "check", "--syntax-only"}, common...), ""); code != exitFail || !strings.Contains(out, "unknown_key") {
		t.Fatalf("unknown key: %d %q", code, out)
	}
	if code, _, _ := runCmd([]string{"policy"}, ""); code != exitUsage {
		t.Fatalf("usage: %d", code)
	}
}

func TestStatusNeverPrintsTheCredential(t *testing.T) {
	cfg, st := dirs(t)
	if err := config.Write(filepath.Join(st, "credential", "env"), config.Config{ServerURL: "https://dd.example.com", WorkerID: "wrk_1", OrgID: "org_1", Credential: cred}); err != nil {
		t.Fatal(err)
	}
	code, out, errOut := runCmd([]string{"status", "--config-dir", cfg, "--state-dir", st}, "")
	if code != exitOK || !strings.Contains(out, "worker wrk_1, org org_1") || !strings.Contains(out, "NOT VALID") {
		t.Fatalf("%d %q %q", code, out, errOut)
	}
	if strings.Contains(out+errOut, cred) {
		t.Fatal("status printed the credential")
	}
	cfg2, st2 := dirs(t)
	if code, out, _ := runCmd([]string{"status", "--config-dir", cfg2, "--state-dir", st2}, ""); code != exitFail || !strings.Contains(out, "not enrolled") {
		t.Fatalf("unenrolled: %d %q", code, out)
	}
}

func TestInitRefusals(t *testing.T) {
	cfg, st := dirs(t)
	common := []string{"--config-dir", cfg, "--state-dir", st}
	// The join token is never an argument.
	if code, _, _ := runCmd(append([]string{"init", "--server", "https://dd.example.com"}, common...), "dd_wjn_x"); code != exitUsage {
		t.Fatalf("without --join-token-stdin: %d", code)
	}
	if code, _, _ := runCmd(append([]string{"init", "--server", "https://dd.example.com", "--join-token", "dd_wjn_abcdefghijklmnop01"}, common...), ""); code != exitUsage {
		t.Fatalf("--join-token accepted: %d", code)
	}
	// http without --insecure-dev, and a malformed token, fail before any dial.
	code, _, errOut := runCmd(append([]string{"init", "--server", "http://dd.example.com", "--join-token-stdin"}, common...), "dd_wjn_abcdefghijklmnop01\n")
	if code != exitFail || !strings.Contains(errOut, "https") {
		t.Fatalf("http origin: %d %q", code, errOut)
	}
	code, _, errOut = runCmd(append([]string{"init", "--server", "https://dd.example.com", "--join-token-stdin"}, common...), "not-a-token\n")
	if code != exitFail || strings.Contains(errOut, "not-a-token") {
		t.Fatalf("bad token: %d %q", code, errOut)
	}
	for _, f := range []string{filepath.Join(cfg, "env"), filepath.Join(st, "credential", "env")} {
		if _, err := os.Stat(f); !os.IsNotExist(err) {
			t.Fatal("a credential file was written")
		}
	}
}

// W3 §3.2: init as root would write a credential the worker user cannot
// read, so it is refused before anything is read or dialled.
func TestInitRefusesRoot(t *testing.T) {
	old := geteuid
	geteuid = func() int { return 0 }
	t.Cleanup(func() { geteuid = old })
	cfg, st := dirs(t)
	code, _, errOut := runCmd([]string{"init", "--server", "https://dd.example.com", "--join-token-stdin", "--config-dir", cfg, "--state-dir", st}, "dd_wjn_abcdefghijklmnop01\n")
	if code != exitFail || !strings.Contains(errOut, "sudo -u dd-worker dd-worker init --server https://dd.example.com --join-token-stdin") {
		t.Fatalf("root init: %d %q", code, errOut)
	}
	if _, err := os.Stat(st); !os.IsNotExist(err) {
		t.Fatal("root init wrote state")
	}
}

// A credential still at the pre-W3 path is named by status (run moves it:
// internal/worker TestMigrateCredential*).
func TestStatusNamesLegacyCredential(t *testing.T) {
	cfg, st := dirs(t)
	if err := config.Write(filepath.Join(cfg, "env"), config.Config{ServerURL: "https://dd.example.com", WorkerID: "wrk_1", OrgID: "org_1", Credential: cred}); err != nil {
		t.Fatal(err)
	}
	code, out, _ := runCmd([]string{"status", "--config-dir", cfg, "--state-dir", st}, "")
	if code != exitFail || !strings.Contains(out, "dd-worker run moves it") {
		t.Fatalf("status: %d %q", code, out)
	}
}

func TestRunNotEnrolled(t *testing.T) {
	cfg, st := dirs(t)
	if code, _, errOut := runCmd([]string{"run", "--config-dir", cfg, "--state-dir", st}, ""); code != exitFail || !strings.Contains(errOut, "not enrolled") {
		t.Fatalf("%d %q", code, errOut)
	}
	if code, _, _ := runCmd([]string{"bogus"}, ""); code != exitUsage {
		t.Fatalf("unknown command: %d", code)
	}
}
