package policy

// Adapted from bot-agent's policy tests: the file checks are the same; the
// key set comes from a test schema instead of bot-agent's fixed keys.

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"
)

var testSchema = Schema{
	"enabled":  Bool,
	"max_jobs": Int,
	"name":     String,
	"repos":    Strings,
}

func TestUnknownKeyIsInvalid(t *testing.T) {
	body := []byte("enabled = true\nnot_a_key = [\"ghcr.io/user/*\"]\n")
	p := loadBytes(t, body, 0o644, rootUID)
	if p.Valid || p.Values != nil || p.Reason != "unknown_key" {
		t.Fatalf("%+v", p)
	}
	if p.Hash != hexHash(body) {
		t.Fatalf("hash %s", p.Hash)
	}
}

func TestNonRootOwner(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "policy.toml")
	body := []byte("enabled = true\n")
	if err := os.WriteFile(path, body, 0o644); err != nil {
		t.Fatal(err)
	}
	p := Load(Files{Policy: path, Schema: testSchema, UID: func(os.FileInfo) (uint32, bool) { return 1000, true }})
	if !p.Exists || p.Valid || p.Reason != "not_root" {
		t.Fatalf("%+v", p)
	}
	if p.Hash != hexHash(body) {
		t.Fatalf("hash %s", p.Hash)
	}
	// A real file in a temp dir is owned by this (non-root) user.
	if os.Geteuid() != 0 {
		if p := Load(Files{Policy: path, Schema: testSchema}); p.Reason != "not_root" {
			t.Fatalf("stat uid: %+v", p)
		}
	}
}

func TestSkipOwnerChecksOnlySyntax(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("a temp file is root-owned when root runs the test: run as a non-root user")
	}
	path := filepath.Join(t.TempDir(), "policy.toml")
	if err := os.WriteFile(path, []byte("enabled = true\n"), 0o666); err != nil {
		t.Fatal(err)
	}
	if p := Load(Files{Policy: path, Schema: testSchema}); p.Valid {
		t.Fatalf("Load accepted a non-root file: %+v", p)
	}
	p := Load(Files{Policy: path, Schema: testSchema, SkipOwner: true})
	if !p.Valid || !p.Values["enabled"].Bool {
		t.Fatalf("%+v", p)
	}
}

func TestGroupAndWorldWritable(t *testing.T) {
	cases := []struct {
		mode os.FileMode
		want string
	}{
		{0o664, "group_writable"},
		{0o642, "world_writable"},
	}
	for _, tc := range cases {
		p := loadBytes(t, []byte("enabled = true\n"), tc.mode, rootUID)
		if p.Valid || p.Reason != tc.want {
			t.Fatalf("mode %o: %+v", tc.mode, p)
		}
	}
}

func TestMissingFileIsAbsent(t *testing.T) {
	p := Load(Files{Policy: filepath.Join(t.TempDir(), "no-such.toml"), Schema: testSchema})
	if p.Exists || p.Valid || p.Hash != "" || p.Reason != "" {
		t.Fatalf("%+v", p)
	}
}

func TestValuesAndHash(t *testing.T) {
	body := []byte("# comment\nenabled = false\nmax_jobs = 2\nname = \"pi \\\"one\\\"\"\nrepos = [\"https://github.com/a/*\", \"git@github.com:b/c\"]\n")
	p := loadBytes(t, body, 0o644, rootUID)
	if !p.Valid || p.Values["enabled"].Bool || p.Values["max_jobs"].Int != 2 || p.Values["name"].String != `pi "one"` {
		t.Fatalf("%+v", p)
	}
	if r := p.Values["repos"].Strings; len(r) != 2 || r[1] != "git@github.com:b/c" {
		t.Fatalf("repos %v", r)
	}
	if p.Hash != hexHash(body) {
		t.Fatalf("hash %s", p.Hash)
	}
}

func TestParseRejectsWrongTypes(t *testing.T) {
	for _, body := range []string{
		"enabled = yes\n",
		"max_jobs = -1\n",
		"repos = \"user\"\n",
		"name = user\n",
		"name = \"a\" \"b\"\n",
		"enabled = true\nenabled = false\n",
		"repos = [user]\n",
		"enabled = true\x00\n",
	} {
		if _, reason := parseTOML([]byte(body), testSchema); reason != "parse" {
			t.Fatalf("%q reason %s", body, reason)
		}
	}
}

func TestTooLarge(t *testing.T) {
	big := make([]byte, MaxBytes+1)
	for i := range big {
		big[i] = '#'
	}
	if p := loadBytes(t, big, 0o644, rootUID); p.Valid || p.Reason != "parse" {
		t.Fatalf("%+v", p)
	}
}

func rootUID(os.FileInfo) (uint32, bool) { return 0, true }

func loadBytes(t *testing.T, body []byte, mode os.FileMode, uid func(os.FileInfo) (uint32, bool)) File {
	t.Helper()
	path := filepath.Join(t.TempDir(), "policy.toml")
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
	return Load(Files{Policy: path, Schema: testSchema, UID: uid})
}

func hexHash(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}
