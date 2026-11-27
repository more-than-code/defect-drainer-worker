package selfupdate

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/more-than-code/defect-drainer-worker/internal/release"
	"github.com/more-than-code/defect-drainer-worker/workerproto"
)

const installer = "#!/bin/sh\necho fake install.sh \"$@\"\n"

func tgz(t *testing.T, files map[string]string, extra ...*tar.Header) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(zw)
	for name, body := range files {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(body)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		tw.Write([]byte(body))
	}
	for _, h := range extra {
		tw.WriteHeader(h)
	}
	tw.Close()
	zw.Close()
	return buf.Bytes()
}

// release is one fake signed release served by fakeFetch.
type fakeRelease struct {
	files map[string][]byte
	err   map[string]error
}

func (f fakeRelease) Fetch(_ context.Context, version, name string) ([]byte, error) {
	if e := f.err[name]; e != nil {
		return nil, e
	}
	b, ok := f.files[version+"/"+name]
	if !ok {
		return nil, errors.New("404")
	}
	return b, nil
}

type fixture struct {
	t        *testing.T
	env      Env
	rel      fakeRelease
	priv     ed25519.PrivateKey
	target   string
	tarball  string
	bundle   map[string]string
	install  func() (int, string, error)
	healthy  error
	rollback error
	rolled   bool
	busy     bool
	policy   HostPolicy
	version  string
}

func newFixture(t *testing.T) *fixture {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	f := &fixture{t: t, priv: priv, target: "0.2.1", policy: HostPolicy{Valid: true, Mode: workerproto.SelfUpdateServer}}
	f.tarball = "dd-worker_0.2.1_linux_arm64.tar.gz"
	f.bundle = map[string]string{"dd-worker": "binary", "install.sh": installer, "dd-worker.service": "[Unit]\n"}
	f.install = func() (int, string, error) { return 0, "upgraded", nil }
	f.version = "0.2.1\n"
	state := t.TempDir()
	root := t.TempDir()
	f.env = Env{
		StateDir: state, RootDir: root, Arch: "arm64", Keys: []ed25519.PublicKey{pub},
		Installed: func() (string, error) { return "0.2.0", nil },
		Policy:    func() HostPolicy { return f.policy },
		Busy:      func(context.Context) bool { return f.busy },
		VersionOf: func(_ context.Context, bin string) (string, error) {
			if _, err := os.Stat(bin); err != nil {
				return "", err
			}
			return f.version, nil
		},
		Install: func(_ context.Context, script, version, base string) (int, string, error) {
			if !strings.HasPrefix(base, "file://"+root) {
				t.Errorf("base %q", base)
			}
			if _, err := os.Stat(filepath.Join(strings.TrimPrefix(base, "file://"), "v"+version, "SHA256SUMS")); err != nil {
				t.Errorf("staged sums: %v", err)
			}
			return f.install()
		},
		Healthy:  func(context.Context) error { return f.healthy },
		Rollback: func(context.Context) error { f.rolled = true; return f.rollback },
		Now:      func() time.Time { return time.Date(2026, 11, 27, 3, 0, 0, 0, time.UTC) },
	}
	return f
}

// build signs the release and serves it.
func (f *fixture) build() {
	tb := tgz(f.t, f.bundle)
	sums := fmt.Sprintf("%s  %s\n%s  install.sh\n", sha256Hex(tb), f.tarball, sha256Hex([]byte(installer)))
	f.rel.files = map[string][]byte{
		f.target + "/SHA256SUMS":     []byte(sums),
		f.target + "/SHA256SUMS.sig": release.EncodeSig(ed25519.Sign(f.priv, []byte(sums))),
		f.target + "/" + f.tarball:   tb,
	}
	f.env.Fetch = &f.rel
}

func (f *fixture) run(request string) *workerproto.SelfUpdateResult {
	f.t.Helper()
	if request != "" {
		if err := os.MkdirAll(filepath.Join(f.env.StateDir, RequestDirName), 0o700); err != nil {
			f.t.Fatal(err)
		}
		os.WriteFile(filepath.Join(f.env.StateDir, RequestDirName, RequestName), []byte(request), 0o600)
	}
	res, err := Run(context.Background(), f.env)
	if err != nil {
		f.t.Fatal(err)
	}
	if res != nil {
		if got := ReadResult(f.env.RootDir); got == nil || *got != *res {
			f.t.Fatalf("result.json %+v, want %+v", got, res)
		}
		if RequestPending(f.env.StateDir) {
			f.t.Fatal("request not deleted")
		}
		if _, err := os.Stat(filepath.Join(f.env.RootDir, "stage")); err == nil {
			f.t.Fatal("stage left behind")
		}
	}
	return res
}

func expect(t *testing.T, name string, res *workerproto.SelfUpdateResult, result, reason string) {
	t.Helper()
	if res == nil {
		t.Fatalf("%s: no result", name)
	}
	if res.Result != result || res.Reason != reason {
		t.Errorf("%s: got %s %s, want %s %s", name, res.Result, res.Reason, result, reason)
	}
}

func TestRunSucceeds(t *testing.T) {
	f := newFixture(t)
	f.build()
	res := f.run("0.2.1\n")
	expect(t, "success", res, workerproto.SelfUpdateSucceeded, "")
	if res.From != "0.2.0" || res.To != "0.2.1" || res.At != "2026-11-27T03:00:00Z" {
		t.Errorf("result %+v", res)
	}
}

func TestRunNoRequest(t *testing.T) {
	f := newFixture(t)
	f.build()
	if res := f.run(""); res != nil {
		t.Fatalf("no request: %+v", res)
	}
}

func TestRunRefusals(t *testing.T) {
	cases := []struct {
		name    string
		request string
		mod     func(*fixture)
		reason  string
	}{
		{"bad request", "0.2.1; rm -rf /\n", nil, ReasonBadRequest},
		{"bad request rc0", "0.2.1-rc0\n", nil, ReasonBadRequest},
		{"policy invalid", "0.2.1\n", func(f *fixture) { f.policy.Valid = false }, ReasonPolicyInvalid},
		{"off", "0.2.1\n", func(f *fixture) { f.policy.Mode = workerproto.SelfUpdateOff }, ReasonOff},
		{"not newer", "0.2.0\n", nil, release.ReasonNotNewer},
		{"downgrade", "0.1.9\n", nil, release.ReasonNotNewer},
		{"cross minor", "0.3.0\n", nil, release.ReasonCrossMinor},
		{"outside window", "0.2.1\n", func(f *fixture) { f.policy.Window, _ = ParseWindow("04:00-05:00") }, ReasonOutsideWindow},
		{"busy", "0.2.1\n", func(f *fixture) { f.busy = true }, ReasonBusy},
	}
	for _, c := range cases {
		f := newFixture(t)
		f.build()
		if c.mod != nil {
			c.mod(f)
		}
		installed := false
		f.env.Install = func(context.Context, string, string, string) (int, string, error) {
			installed = true
			return 0, "", nil
		}
		expect(t, c.name, f.run(c.request), workerproto.SelfUpdateRefused, c.reason)
		if installed {
			t.Errorf("%s: installed", c.name)
		}
	}
}

func TestRunRefusesALinkedRequest(t *testing.T) {
	f := newFixture(t)
	f.build()
	secret := filepath.Join(t.TempDir(), "secret")
	os.WriteFile(secret, []byte("0.2.1\n"), 0o600)
	os.MkdirAll(filepath.Join(f.env.StateDir, RequestDirName), 0o700)
	if err := os.Symlink(secret, filepath.Join(f.env.StateDir, RequestDirName, RequestName)); err != nil {
		t.Fatal(err)
	}
	res, err := Run(context.Background(), f.env)
	if err != nil {
		t.Fatal(err)
	}
	expect(t, "linked request", res, workerproto.SelfUpdateRefused, ReasonBadRequest)
	// A linked update dir is not followed either.
	f2 := newFixture(t)
	f2.build()
	elsewhere := t.TempDir()
	os.WriteFile(filepath.Join(elsewhere, RequestName), []byte("0.2.1\n"), 0o600)
	os.Symlink(elsewhere, filepath.Join(f2.env.StateDir, RequestDirName))
	res, err = Run(context.Background(), f2.env)
	if err != nil {
		t.Fatal(err)
	}
	expect(t, "linked dir", res, workerproto.SelfUpdateRefused, ReasonBadRequest)
	if _, err := os.Stat(filepath.Join(elsewhere, RequestName)); err != nil {
		t.Error("followed the link and deleted the target")
	}
}

func TestRunFailures(t *testing.T) {
	cases := []struct {
		name   string
		mod    func(*fixture)
		result string
		reason string
	}{
		{"placeholder key", func(f *fixture) { f.env.Keys = []ed25519.PublicKey{make([]byte, 32)} }, workerproto.SelfUpdateFailed, ReasonKeyPlaceholder},
		{"sums download", func(f *fixture) { f.rel.err = map[string]error{"SHA256SUMS": errors.New("timeout")} }, workerproto.SelfUpdateFailed, ReasonDownloadFailed},
		{"sig download", func(f *fixture) { f.rel.err = map[string]error{"SHA256SUMS.sig": errors.New("404")} }, workerproto.SelfUpdateFailed, ReasonDownloadFailed},
		{"tarball download", func(f *fixture) { f.rel.err = map[string]error{f.tarball: errors.New("reset")} }, workerproto.SelfUpdateFailed, ReasonDownloadFailed},
		{"wrong key", func(f *fixture) {
			pub, _, _ := ed25519.GenerateKey(rand.Reader)
			f.env.Keys = []ed25519.PublicKey{pub}
		}, workerproto.SelfUpdateFailed, ReasonSignatureInvalid},
		{"swapped tarball", func(f *fixture) {
			f.rel.files[f.target+"/"+f.tarball] = tgz(f.t, map[string]string{"dd-worker": "evil", "install.sh": installer})
		}, workerproto.SelfUpdateFailed, ReasonSumMismatch},
		{"no entry for arch", func(f *fixture) { f.env.Arch = "amd64" }, workerproto.SelfUpdateFailed, ReasonSumMismatch},
		{"version mismatch", func(f *fixture) { f.version = "0.2.0\n" }, workerproto.SelfUpdateFailed, ReasonVersionMismatch},
		{"not connected", func(f *fixture) {
			f.install = func() (int, string, error) {
				return 1, "install.sh: 0.2.1 did not connect within 60s: rolling back to 0.2.0\ninstall.sh: error: upgrade to 0.2.1 failed; back on 0.2.0\n", nil
			}
		}, workerproto.SelfUpdateRolledBack, ReasonNotConnected},
		{"install failed", func(f *fixture) { f.install = func() (int, string, error) { return 1, "prerequisites missing", nil } }, workerproto.SelfUpdateFailed, ReasonInstallFailed},
		{"unhealthy", func(f *fixture) { f.healthy = errors.New("not_connected") }, workerproto.SelfUpdateRolledBack, ReasonHealth},
		{"rollback failed", func(f *fixture) { f.healthy = errors.New("x"); f.rollback = errors.New("y") }, workerproto.SelfUpdateFailed, ReasonRollbackFailed},
	}
	for _, c := range cases {
		f := newFixture(t)
		f.build()
		c.mod(f)
		expect(t, c.name, f.run("0.2.1\n"), c.result, c.reason)
	}
}

func TestRunBundleChecks(t *testing.T) {
	for name, files := range map[string]map[string]string{
		"extra file":       {"dd-worker": "b", "install.sh": installer, "evil.sh": "x"},
		"no binary":        {"install.sh": installer},
		"other install.sh": {"dd-worker": "b", "install.sh": "#!/bin/sh\nevil\n"},
		"path in name":     {"dd-worker": "b", "install.sh": installer, "../dd-worker.service": "x"},
		"nested name":      {"dd-worker": "b", "install.sh": installer, "x/LICENSE": "x"},
	} {
		f := newFixture(t)
		f.bundle = files
		f.build()
		expect(t, name, f.run("0.2.1\n"), workerproto.SelfUpdateFailed, ReasonBundleInvalid)
	}
	// A link entry.
	f := newFixture(t)
	f.build()
	tb := tgz(t, map[string]string{"dd-worker": "b", "install.sh": installer}, &tar.Header{Name: "LICENSE", Typeflag: tar.TypeSymlink, Linkname: "/etc/shadow"})
	sums := fmt.Sprintf("%s  %s\n%s  install.sh\n", sha256Hex(tb), f.tarball, sha256Hex([]byte(installer)))
	f.rel.files[f.target+"/SHA256SUMS"] = []byte(sums)
	f.rel.files[f.target+"/SHA256SUMS.sig"] = release.EncodeSig(ed25519.Sign(f.priv, []byte(sums)))
	f.rel.files[f.target+"/"+f.tarball] = tb
	expect(t, "link entry", f.run("0.2.1\n"), workerproto.SelfUpdateFailed, ReasonBundleInvalid)
}

func TestRunLock(t *testing.T) {
	f := newFixture(t)
	f.build()
	f.env.LockPath = filepath.Join(t.TempDir(), "lock")
	unlock, err := holdLock(f.env.LockPath)
	if err != nil {
		t.Fatal(err)
	}
	os.MkdirAll(filepath.Join(f.env.StateDir, RequestDirName), 0o700)
	os.WriteFile(filepath.Join(f.env.StateDir, RequestDirName, RequestName), []byte("0.2.1\n"), 0o600)
	if res, err := Run(context.Background(), f.env); res != nil || err != nil {
		t.Fatalf("locked: %+v %v", res, err)
	}
	if !RequestPending(f.env.StateDir) {
		t.Fatal("a locked-out run took the request")
	}
	unlock()
	expect(t, "after unlock", f.run(""), workerproto.SelfUpdateSucceeded, "")
}

func TestWriteRequest(t *testing.T) {
	dir := t.TempDir()
	if err := WriteRequest(dir, "0.2.1"); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(filepath.Join(dir, RequestDirName, RequestName))
	if string(b) != "0.2.1\n" || !RequestPending(dir) {
		t.Fatalf("request %q", b)
	}
	fi, _ := os.Stat(filepath.Join(dir, RequestDirName))
	if fi.Mode().Perm() != 0o700 {
		t.Errorf("dir mode %v", fi.Mode())
	}
	if err := WriteRequest(dir, "0.2.1\nx"); err == nil {
		t.Error("bad version written")
	}
}

func TestCheckURL(t *testing.T) {
	for _, ok := range []string{
		"https://github.com/more-than-code/defect-drainer-worker/releases/download/v0.2.1/SHA256SUMS",
		"https://objects.githubusercontent.com/x", "https://release-assets.githubusercontent.com:443/x",
	} {
		u, _ := url.Parse(ok)
		if err := checkURL(u); err != nil {
			t.Errorf("%s: %v", ok, err)
		}
	}
	for _, bad := range []string{"http://github.com/x", "https://github.com:8443/x", "https://evil.example/x", "https://u@github.com/x", "file:///etc/passwd"} {
		u, _ := url.Parse(bad)
		if checkURL(u) == nil {
			t.Errorf("%s allowed", bad)
		}
	}
}

func TestHTTPFetcher(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v0.2.1/SHA256SUMS":
			w.Write([]byte("sums"))
		case "/v0.2.1/big":
			w.Write(bytes.Repeat([]byte("x"), 1024))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	f := HTTPFetcher{Client: srv.Client(), Base: srv.URL}
	b, err := f.Fetch(context.Background(), "0.2.1", "SHA256SUMS")
	if err != nil || string(b) != "sums" {
		t.Fatalf("fetch %q %v", b, err)
	}
	if _, err := f.Fetch(context.Background(), "0.2.1", "missing"); err == nil {
		t.Error("404 accepted")
	}
	// The production client refuses a base that is not an allowed host.
	if _, err := (HTTPFetcher{Base: srv.URL}).Fetch(context.Background(), "0.2.1", "SHA256SUMS"); err == nil {
		t.Error("non-GitHub base allowed")
	}
	// A redirect off the allowlist is refused by the production client.
	c, _ := HTTPFetcher{}.client()
	req, _ := http.NewRequest(http.MethodGet, "https://github.com/x", nil)
	evil, _ := url.Parse("https://evil.example/x")
	if err := c.CheckRedirect(&http.Request{URL: evil}, []*http.Request{req}); err == nil {
		t.Error("redirect to another host allowed")
	}
	ok, _ := url.Parse("https://objects.githubusercontent.com/x")
	if err := c.CheckRedirect(&http.Request{URL: ok}, []*http.Request{req}); err != nil {
		t.Errorf("redirect to the asset host refused: %v", err)
	}
	via := make([]*http.Request, MaxRedirects)
	for i := range via {
		via[i] = req
	}
	if err := c.CheckRedirect(&http.Request{URL: ok}, via); err == nil {
		t.Error("redirect past MaxRedirects allowed")
	}
}

// install.sh stopped after it switched (no "back on"): the root step
// switches back itself.
func TestRunRollsBackAHalfInstall(t *testing.T) {
	f := newFixture(t)
	f.build()
	installed := "0.2.0"
	f.env.Installed = func() (string, error) { return installed, nil }
	f.install = func() (int, string, error) { installed = "0.2.1"; return 1, "systemctl restart dd-worker failed", nil }
	expect(t, "half install", f.run("0.2.1\n"), workerproto.SelfUpdateRolledBack, ReasonInstallFailed)
	if !f.rolled {
		t.Fatal("not rolled back")
	}
	f2 := newFixture(t)
	f2.build()
	f2.install = func() (int, string, error) { return 1, "prerequisites missing", nil }
	expect(t, "nothing switched", f2.run("0.2.1\n"), workerproto.SelfUpdateFailed, ReasonInstallFailed)
	if f2.rolled {
		t.Fatal("rolled back a host that never switched")
	}
}

// The root step holds a failed target back for Cooldown on its own, and
// keeps the result that says why; timing refusals do not count.
func TestRunCoolsDown(t *testing.T) {
	f := newFixture(t)
	f.build()
	f.healthy = errors.New("x")
	expect(t, "first", f.run("0.2.1\n"), workerproto.SelfUpdateRolledBack, ReasonHealth)
	f.healthy, f.rolled = nil, false
	os.WriteFile(filepath.Join(f.env.StateDir, RequestDirName, RequestName), []byte("0.2.1\n"), 0o600)
	res, err := Run(context.Background(), f.env)
	if err != nil || res != nil {
		t.Fatalf("in the cool-down: %+v %v", res, err)
	}
	if got := ReadResult(f.env.RootDir); got == nil || got.Reason != ReasonHealth {
		t.Fatalf("result.json replaced: %+v", got)
	}
	if RequestPending(f.env.StateDir) {
		t.Fatal("request not taken")
	}
	f.env.Now = func() time.Time { return time.Date(2026, 11, 27, 9, 1, 0, 0, time.UTC) }
	expect(t, "after", f.run("0.2.1\n"), workerproto.SelfUpdateSucceeded, "")

	g := newFixture(t)
	g.build()
	g.busy = true
	expect(t, "busy", g.run("0.2.1\n"), workerproto.SelfUpdateRefused, ReasonBusy)
	g.busy = false
	expect(t, "busy is retried", g.run("0.2.1\n"), workerproto.SelfUpdateSucceeded, "")
}

// An installed version dir that is not the signed release is replaced
// before install.sh, which would reuse it as it is.
func TestRunReplacesAStaleVersionDir(t *testing.T) {
	f := newFixture(t)
	f.build()
	f.env.LibDir = t.TempDir()
	stale := filepath.Join(f.env.LibDir, "0.2.1")
	os.MkdirAll(stale, 0o755)
	os.WriteFile(filepath.Join(stale, "dd-worker"), []byte("old unsigned build"), 0o755)
	expect(t, "stale", f.run("0.2.1\n"), workerproto.SelfUpdateSucceeded, "")
	if _, err := os.Stat(stale); err == nil {
		t.Fatal("stale dir kept")
	}
	same := filepath.Join(f.env.LibDir, "0.2.1")
	os.MkdirAll(same, 0o755)
	os.WriteFile(filepath.Join(same, "dd-worker"), []byte("binary"), 0o755) // the bundle's
	os.Remove(filepath.Join(f.env.RootDir, ResultName))
	expect(t, "same", f.run("0.2.1\n"), workerproto.SelfUpdateSucceeded, "")
	if _, err := os.Stat(same); err != nil {
		t.Fatal("identical dir removed")
	}
}

// Whatever the worker puts at request is removed, so the path unit does
// not fire on it again: a directory, a link.
func TestRunClearsAnyRequest(t *testing.T) {
	f := newFixture(t)
	f.build()
	os.MkdirAll(filepath.Join(f.env.StateDir, RequestDirName, RequestName), 0o700)
	res, err := Run(context.Background(), f.env)
	if err != nil {
		t.Fatal(err)
	}
	expect(t, "dir", res, workerproto.SelfUpdateRefused, ReasonBadRequest)
	if _, err := os.Lstat(filepath.Join(f.env.StateDir, RequestDirName, RequestName)); err == nil {
		t.Fatal("directory left at request")
	}
	os.Symlink("/nonexistent", filepath.Join(f.env.StateDir, RequestDirName, RequestName))
	Run(context.Background(), f.env)
	if _, err := os.Lstat(filepath.Join(f.env.StateDir, RequestDirName, RequestName)); err == nil {
		t.Fatal("link left at request")
	}
}

// A lock that can't be taken for another reason than a running run still
// answers the request.
func TestRunAnswersWhenTheLockFails(t *testing.T) {
	f := newFixture(t)
	f.build()
	f.env.LockPath = filepath.Join(t.TempDir(), "missing", "lock")
	os.MkdirAll(filepath.Join(f.env.StateDir, RequestDirName), 0o700)
	os.WriteFile(filepath.Join(f.env.StateDir, RequestDirName, RequestName), []byte("0.2.1\n"), 0o600)
	res, err := Run(context.Background(), f.env)
	if err != nil {
		t.Fatal(err)
	}
	expect(t, "lock", res, workerproto.SelfUpdateFailed, ReasonInternal)
	if RequestPending(f.env.StateDir) {
		t.Fatal("request left")
	}
}

// Runs are spaced MinInterval apart.
func TestRunPaces(t *testing.T) {
	f := newFixture(t)
	f.build()
	f.env.MinInterval = 300 * time.Millisecond
	f.env.Now = time.Now
	f.run("")
	start := time.Now()
	f.run("")
	if d := time.Since(start); d < 250*time.Millisecond {
		t.Fatalf("second run after %s", d)
	}
}
