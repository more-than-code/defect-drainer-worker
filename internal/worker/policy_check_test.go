package worker

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/more-than-code/defect-drainer-worker/workerproto"
)

// TestPolicyWireHashAndManagedURL: the wire policy carries the file's hash,
// valid or not, and managed_url only with a valid policy.
func TestPolicyWireHashAndManagedURL(t *testing.T) {
	path := filepath.Join(t.TempDir(), "policy.toml")
	writeFile(t, path, "enabled = true\norgs = [\"org_a\"]\nmanaged_url = \"https://bot.example/h/1\"\n", 0o644)
	p := LoadPolicy(path, false, rootUID)
	w := p.Wire()
	if !p.Valid || len(p.Hash) != 64 || w.Hash != p.Hash || w.ManagedURL != "https://bot.example/h/1" {
		t.Fatalf("valid: %+v %+v", p, w)
	}
	writeFile(t, path, "enabled = true\norgs = []\nmanaged_url = \"https://bot.example/h/1\"\n", 0o644)
	p = LoadPolicy(path, false, rootUID)
	if w := p.Wire(); p.Valid || w.Hash == "" || w.Hash != p.Hash || w.ManagedURL != "" {
		t.Fatalf("invalid: %+v %+v", p, w)
	}
	if w := LoadPolicy(filepath.Join(t.TempDir(), "none"), false, rootUID).Wire(); w.Hash != "" {
		t.Fatalf("missing: %+v", w)
	}
}

// TestCheckPolicyDataMatchesTheFile: the same bytes give the same verdict
// and hash from stdin as from an installed file.
func TestCheckPolicyDataMatchesTheFile(t *testing.T) {
	body := "enabled = false\norgs = [\"org_a\"]\nmax_jobs = 2\n"
	path := filepath.Join(t.TempDir(), "policy.toml")
	writeFile(t, path, body, 0o644)
	f, d := LoadPolicy(path, false, rootUID), CheckPolicyData([]byte(body))
	if !d.Valid || d.Hash != f.Hash || d.MaxJobs != 2 || d.Enabled {
		t.Fatalf("file %+v data %+v", f, d)
	}
	if d := CheckPolicyData(nil); d.Valid || d.Reason != "orgs_empty" || !d.Exists {
		t.Fatalf("empty %+v", d)
	}
}

func TestValidManagedURL(t *testing.T) {
	for s, ok := range map[string]bool{
		"https://bot.example/hosts/h1/files/policy": true,
		"https://bot.example:8443/x?y=1#z":          true,
		"HTTPS://bot.example/x":                     true,
		"":                                          false,
		"http://bot.example/x":                      false,
		"https://":                                  false,
		"https://u@bot.example/x":                   false,
		"https://bot.example/a b":                   false,
		"https://bot.example/a\tb":                  false,
		"https://bot.example/\u0085":                false,
		"javascript:alert(1)":                       false,
		"//bot.example/x":                           false,
		"https:bot.example":                         false,
		"https://bot.example/" + strings.Repeat("a", workerproto.MaxManagedURL-20): true,
		"https://bot.example/" + strings.Repeat("a", workerproto.MaxManagedURL-19): false,
	} {
		if workerproto.ValidManagedURL(s) != ok {
			t.Errorf("%.40q: want %v", s, ok)
		}
	}
}

// TestCheckSecretsFileOwner: another user's file is bad_owner, as
// LoadSecrets refuses it.
func TestCheckSecretsFileOwner(t *testing.T) {
	path := filepath.Join(t.TempDir(), "secrets.env")
	writeFile(t, path, "A=1\n", 0o600)
	other := func(os.FileInfo) (uint32, bool) { return 12345, true }
	if r, err := CheckSecretsFile(path, other); err != nil || r != "bad_owner" {
		t.Fatalf("%q %v", r, err)
	}
	if _, err := LoadSecrets(path, other); err == nil || !strings.Contains(err.Error(), "owned by root or the worker user") {
		t.Fatalf("LoadSecrets: %v", err)
	}
	if r, err := CheckSecretsFile(path, rootUID); err != nil || r != "" {
		t.Fatalf("root: %q %v", r, err)
	}
}
