package config

// Adapted from bot-agent's config tests for DD's keys and origin rule.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const testCred = "dd_wrk_" + "abcdefghijklmnopqrstuvwxyz012345"

func TestLoadRefusesModeAndSymlink(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "env")
	body := "DD_WORKER_SERVER=https://example.invalid\nDD_WORKER_ID=wrk_1\nDD_WORKER_CREDENTIAL=" + testCred + "\n"
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil || !strings.Contains(err.Error(), "0600") {
		t.Fatalf("mode: %v", err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.WorkerID != "wrk_1" || cfg.Credential != testCred || cfg.ServerURL != "https://example.invalid" {
		t.Fatalf("%+v", cfg)
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink(path, link); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(link); err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("symlink: %v", err)
	}
}

func TestServerURL(t *testing.T) {
	for _, tc := range []struct {
		url      string
		insecure bool
		ok       bool
	}{
		{"https://dd.example.com", false, true},
		{"https://dd.example.com/", false, true},
		{"https://dd.example.com:8443", false, true},
		{"http://dd.example.com", false, false},
		{"http://127.0.0.1:8860", false, false},
		{"http://127.0.0.1:8860", true, true},
		{"http://localhost:8860", true, true},
		{"http://192.0.2.10:8860", true, false},
		{"wss://dd.example.com", false, false},
		{"https://dd.example.com/worker/v1/ws", false, false},
		{"https://dd.example.com?x=1", false, false},
		{"https://user:pw@dd.example.com", false, false},
		{"", false, false},
		{"dd.example.com", false, false},
	} {
		err := ValidateServerURL(tc.url, tc.insecure)
		if (err == nil) != tc.ok {
			t.Errorf("%q insecure=%v: %v", tc.url, tc.insecure, err)
		}
	}
}

func TestWriteThenLoad(t *testing.T) {
	path := filepath.Join(t.TempDir(), "etc", "env")
	c := Config{ServerURL: "http://127.0.0.1:8860", WorkerID: "wrk_a", OrgID: "org_b", Credential: testCred, InsecureDev: true}
	if err := Write(path, c); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(path)
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v %v", fi, err)
	}
	got, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	got.Path = ""
	if got != c {
		t.Fatalf("%+v", got)
	}
	bad := c
	bad.Credential = "not-a-credential"
	if err := Write(path, bad); err == nil || strings.Contains(err.Error(), "not-a-credential") {
		t.Fatalf("%v", err)
	}
	bad = c
	bad.WorkerID = "../x"
	if err := Write(path, bad); err == nil {
		t.Fatal("bad worker id written")
	}
}

func TestJoinTokenAndHostname(t *testing.T) {
	tok, err := ReadJoinToken(strings.NewReader("  dd_wjn_abcdefghijklmnop0123  \nignored\n"))
	if err != nil || tok != "dd_wjn_abcdefghijklmnop0123" || !JoinTokenOK(tok) {
		t.Fatalf("%q %v", tok, err)
	}
	for _, bad := range []string{"", "dd_wrk_abcdefghijklmnop0123", "dd_wjn_short", "dd_wjn_abcdefghijklmnop 0123"} {
		if JoinTokenOK(bad) {
			t.Fatalf("accepted %q", bad)
		}
	}
	if _, err := ReadJoinToken(strings.NewReader("")); err == nil {
		t.Fatal("empty stdin")
	}
	host, err := SanitizeHostname("Pi One")
	if err != nil || host != "pi-one" {
		t.Fatalf("%s %v", host, err)
	}
	if _, err := SanitizeHostname(""); err == nil {
		t.Fatal("empty")
	}
}
