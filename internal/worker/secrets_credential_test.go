package worker

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// TestLoadSecretsCredential: the LoadCredential copy follows the file and
// mode rules; a missing copy is an empty set.
func TestLoadSecretsCredential(t *testing.T) {
	for _, c := range []struct {
		name    string
		mode    os.FileMode
		link    bool
		missing bool
		ok      bool
	}{
		{name: "0400 copy", mode: 0o400, ok: true},
		{name: "0600 copy", mode: 0o600, ok: true},
		{name: "group-readable copy", mode: 0o440},
		{name: "world-readable copy", mode: 0o404},
		{name: "symlink", mode: 0o400, link: true},
		{name: "missing", missing: true, ok: true},
	} {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, SecretsCredential)
			switch {
			case c.missing:
			case c.link:
				target := filepath.Join(t.TempDir(), "real")
				writeFile(t, target, "ANTHROPIC_API_KEY=sk-not-real-1\n", c.mode)
				if err := os.Symlink(target, path); err != nil {
					t.Fatal(err)
				}
			default:
				writeFile(t, path, "ANTHROPIC_API_KEY=sk-not-real-1\n", c.mode)
			}
			s, err := LoadSecretsCredential(dir)
			if c.ok != (err == nil) {
				t.Fatalf("err %v, want ok=%v", err, c.ok)
			}
			if !c.ok {
				return
			}
			_, has := s.Get("ANTHROPIC_API_KEY")
			if has == c.missing {
				t.Fatalf("names %v (missing=%v)", s.Names(), c.missing)
			}
		})
	}
}

// TestSecretsOwnerRuleOnlyForPathFile: the owner rule applies to the file
// on disk and is skipped for the credential copy, which systemd owns.
func TestSecretsOwnerRuleOnlyForPathFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "secrets.env")
	writeFile(t, path, "A=1\n", 0o400)
	fi, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	other := func(os.FileInfo) (uint32, bool) { return 4242, true }
	if os.Geteuid() == 4242 {
		t.Skip("test uid collides")
	}
	if r := secretsFileReason(fi, other, true); r != "bad_owner" {
		t.Fatalf("path file owned by another user: %q", r)
	}
	if r := secretsFileReason(fi, rootUID, true); r != "" {
		t.Fatalf("root-owned path file: %q", r)
	}
	if r := secretsFileReason(fi, other, false); r != "" {
		t.Fatalf("credential copy owned by another user: %q", r)
	}
}

// TestSecretsSourceFollowsCredentialsDir: with $CREDENTIALS_DIRECTORY set the
// worker reads the copy there, not the file on disk; unset, the file.
func TestSecretsSourceFollowsCredentialsDir(t *testing.T) {
	cfg, creds := t.TempDir(), t.TempDir()
	writeFile(t, filepath.Join(cfg, "secrets.env"), "ANTHROPIC_API_KEY=sk-from-path-000\n", 0o600)
	writeFile(t, filepath.Join(creds, SecretsCredential), "ANTHROPIC_API_KEY=sk-from-copy-000\n", 0o400)

	w := &Worker{opts: Options{Paths: Paths{ConfigDir: cfg, CredentialsDir: creds}, OwnerUID: rootUID}}
	s, err := w.loadSecrets()
	if v, _ := s.Get("ANTHROPIC_API_KEY"); err != nil || v != "sk-from-copy-000" {
		t.Fatalf("credentials dir set: %q %v", v, err)
	}
	w.opts.Paths.CredentialsDir = ""
	s, err = w.loadSecrets()
	if v, _ := s.Get("ANTHROPIC_API_KEY"); err != nil || v != "sk-from-path-000" {
		t.Fatalf("credentials dir unset: %q %v", v, err)
	}
}

// TestCredentialsDirDeniedToAgent: the LoadCredential copy is as hidden from
// the agent as /etc/dd-worker is.
func TestCredentialsDirDeniedToAgent(t *testing.T) {
	p := Paths{ConfigDir: "/etc/dd-worker", StateDir: "/var/lib/dd-worker"}
	h := &hostAdapter{}
	if host := h.host(p, "bjob_x", "", nil, nil); slices.Contains(host.EnvFiles, "") {
		t.Fatalf("empty deny path with no credentials dir: %v", host.EnvFiles)
	}
	p.CredentialsDir = "/run/credentials/dd-worker.service"
	host := h.host(p, "bjob_x", "", nil, nil)
	for _, want := range []string{"/etc/dd-worker", "/run/credentials/dd-worker.service"} {
		if !slices.Contains(host.EnvFiles, want) {
			t.Fatalf("%s not denied: %v", want, host.EnvFiles)
		}
	}
}
