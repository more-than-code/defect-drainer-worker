package worker

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/more-than-code/defect-drainer-worker/internal/botagent/config"
	"github.com/more-than-code/defect-drainer-worker/workerproto"
)

func legacyPaths(t *testing.T) (Paths, *strings.Builder, func(string, ...any)) {
	t.Helper()
	tmp := t.TempDir()
	p := Paths{ConfigDir: filepath.Join(tmp, "etc"), StateDir: filepath.Join(tmp, "state")}
	for _, f := range []string{p.LegacyEnv(), p.LegacyEnvPrev()} {
		if err := config.Write(f, config.Config{ServerURL: "https://dd.example.com", WorkerID: "wrk_1", OrgID: "org_1", Credential: testCred}); err != nil {
			t.Fatal(err)
		}
	}
	var log strings.Builder
	return p, &log, func(f string, a ...any) { fmt.Fprintf(&log, f+"\n", a...) }
}

// §3.2: the old {config}/env and env.prev move once to {state}/credential/.
func TestMigrateCredentialMovesOnce(t *testing.T) {
	p, log, logf := legacyPaths(t)
	if err := MigrateCredential(p, logf); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{p.Env(), p.EnvPrev()} {
		if _, err := config.Load(f); err != nil {
			t.Fatalf("%s: %v", f, err)
		}
	}
	for _, f := range []string{p.LegacyEnv(), p.LegacyEnvPrev()} {
		if _, err := os.Lstat(f); !os.IsNotExist(err) {
			t.Fatalf("%s still there", f)
		}
	}
	if fi, err := os.Stat(p.CredentialDir()); err != nil || fi.Mode().Perm() != 0o700 {
		t.Fatalf("credential dir %v %v", fi, err)
	}
	if err := MigrateCredential(p, logf); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(log.String(), testCred) {
		t.Fatal("the credential reached the log")
	}
}

// A config dir the worker cannot write: no move, the exact mv commands are
// logged, and the error stops the worker (dd-worker run exits 1).
func TestMigrateCredentialReadOnlyConfigDir(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory modes")
	}
	p, log, logf := legacyPaths(t)
	if err := os.Chmod(p.ConfigDir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(p.ConfigDir, 0o700) })
	err := MigrateCredential(p, logf)
	if err == nil {
		t.Fatal("migration with a read-only config dir succeeded")
	}
	for _, want := range []string{"mv " + p.LegacyEnvPrev() + " " + p.EnvPrev(), "mv " + p.LegacyEnv() + " " + p.Env()} {
		if !strings.Contains(log.String(), want) {
			t.Fatalf("log lacks %q:\n%s", want, log.String())
		}
	}
	if _, err := New(Options{Paths: p, Log: &strings.Builder{}}); err == nil {
		t.Fatal("New ran with the credential still in the config dir")
	}
}

// §3.2 and criterion 3: the worker never writes the config dir. With it
// read-only, a rotation still works (env and env.prev in {state}/credential/).
func TestRotationWithReadOnlyConfigDir(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory modes")
	}
	h := newHarness(t, harnessOpts{noStart: true})
	if err := os.Chmod(h.paths.ConfigDir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(h.paths.ConfigDir, 0o700) })
	h.start(Timing{})
	h.fs.mu.Lock()
	h.fs.creds[testCredB] = true
	h.fs.mu.Unlock()
	h.fs.send(workerproto.Rotated{Header: workerproto.NewHeader(workerproto.TypeRotated), Credential: testCredB})
	eventually(t, 5*time.Second, "rotate_ack", func() bool { return len(h.fs.frames(workerproto.TypeRotateAck)) == 1 })
	cur, _ := config.Load(h.paths.Env())
	prev, _ := config.Load(h.paths.EnvPrev())
	if cur.Credential != testCredB || prev.Credential != testCred {
		t.Fatal("env/env.prev not rotated in the credential dir")
	}
	if filepath.Dir(h.paths.Env()) != filepath.Join(h.paths.StateDir, "credential") {
		t.Fatalf("credential at %s", h.paths.Env())
	}
}

// The agent may not read the credential dir (§3.2).
func TestCredentialDirDeniedToAgent(t *testing.T) {
	p := Paths{ConfigDir: "/etc/dd-worker", StateDir: "/var/lib/dd-worker"}
	h := &hostAdapter{}
	host := h.host(p, "bjob_x", "", nil, nil)
	found := false
	for _, f := range host.EnvFiles {
		if f == "/var/lib/dd-worker/credential" {
			found = true
		}
	}
	if !found {
		t.Fatalf("credential dir not denied: %v", host.EnvFiles)
	}
}

// install.sh pre-creates an empty {state}/credential (dd-worker, 0700) before
// init: init accepts it. A credential dir that is a symlink is refused by
// init, by the migration and by run.
func TestCredentialDirPrecreatedAndSymlink(t *testing.T) {
	fs := newFakeServer(t)
	tmp := t.TempDir()
	p := Paths{ConfigDir: filepath.Join(tmp, "etc"), StateDir: filepath.Join(tmp, "state")}
	if err := os.MkdirAll(p.CredentialDir(), 0o700); err != nil {
		t.Fatal(err)
	}
	mid := filepath.Join(tmp, "machine-id")
	writeFile(t, mid, "abc123machine\n", 0o644)
	join := "dd_wjn_" + "jointokenjointoken01"
	fs.joinTokens[join] = true
	fs.enrollCred = testCred
	if _, err := Enroll(context.Background(), EnrollOptions{Paths: p, Server: fs.URL(), JoinToken: join, InsecureDev: true, MachineIDFile: mid}); err != nil {
		t.Fatalf("init into a pre-created empty credential dir: %v", err)
	}
	if _, err := config.Load(p.Env()); err != nil {
		t.Fatal(err)
	}

	// The credential dir is a symlink to somewhere else.
	tmp2 := t.TempDir()
	q := Paths{ConfigDir: filepath.Join(tmp2, "etc"), StateDir: filepath.Join(tmp2, "state")}
	elsewhere := filepath.Join(tmp2, "elsewhere")
	if err := os.MkdirAll(elsewhere, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(q.StateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(elsewhere, q.CredentialDir()); err != nil {
		t.Fatal(err)
	}
	fs.joinTokens[join] = true
	if _, err := Enroll(context.Background(), EnrollOptions{Paths: q, Server: fs.URL(), JoinToken: join, InsecureDev: true, MachineIDFile: mid}); err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("init through a symlinked credential dir: %v", err)
	}
	if entries, _ := os.ReadDir(elsewhere); len(entries) != 0 {
		t.Fatal("init wrote through the symlink")
	}
	if err := MigrateCredential(q, func(string, ...any) {}); err == nil {
		t.Fatal("migration accepted a symlinked credential dir")
	}
	if _, err := New(Options{Paths: q, Log: &strings.Builder{}}); err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("run with a symlinked credential dir: %v", err)
	}
}

func TestCredentialDirOpenToOthersRefused(t *testing.T) {
	tmp := t.TempDir()
	p := Paths{ConfigDir: filepath.Join(tmp, "etc"), StateDir: filepath.Join(tmp, "state")}
	if err := os.MkdirAll(p.CredentialDir(), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(p.CredentialDir(), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := CheckCredentialDir(p); err == nil || !strings.Contains(err.Error(), "0700") {
		t.Fatalf("0750 credential dir: %v", err)
	}
}
