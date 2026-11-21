//go:build linux

package worker

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	aa "github.com/more-than-code/defect-drainer-worker/agentadapter"
	"github.com/more-than-code/defect-drainer-worker/agentadapter/claude"
	dexec "github.com/more-than-code/defect-drainer-worker/exec"
	"github.com/more-than-code/defect-drainer-worker/git"
)

// signInApp is a tiny app with a session cookie: a sign-in form posting to
// /session, a JSON sign-in at /api/login, and /account, which redirects to
// /login without the cookie.
const signInApp = `import json, sys
from http.server import BaseHTTPRequestHandler, HTTPServer
from urllib.parse import parse_qs
EMAIL, PASSWORD = "seed@example.com", "seed-Pass!word1"
FORM = b'<!doctype html><title>login</title><form method=post action=/session><input id=email name=email><input id=password name=password type=password><button id=go>Sign in</button></form>'
class H(BaseHTTPRequestHandler):
    def log_message(self, *a): pass
    def send(self, code, body=b"", headers=()):
        self.send_response(code)
        for k, v in headers: self.send_header(k, v)
        self.send_header("Content-Type", "text/html")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)
    def signed_in(self):
        return "sid=ok" in (self.headers.get("Cookie") or "")
    def do_GET(self):
        if self.path == "/health": return self.send(200, b"ok")
        if self.path == "/login": return self.send(200, FORM)
        if self.path == "/account":
            if not self.signed_in(): return self.send(302, b"", [("Location", "/login")])
            return self.send(200, b'<!doctype html><body style="background:#4a8">signed in</body>')
        return self.send(200, b'<!doctype html><body style="background:#48a">home</body>')
    def do_POST(self):
        body = self.rfile.read(int(self.headers.get("Content-Length") or 0)).decode()
        if self.path == "/session":
            q = parse_qs(body)
            ok = q.get("email") == [EMAIL] and q.get("password") == [PASSWORD]
            return self.send(303, b"", [("Location", "/account" if ok else "/login")] + ([("Set-Cookie", "sid=ok; Path=/")] if ok else []))
        if self.path == "/api/login":
            d = json.loads(body or "{}")
            if d.get("email") == EMAIL and d.get("password") == PASSWORD:
                return self.send(200, b"{}", [("Set-Cookie", "sid=ok; Path=/")])
            return self.send(401, b"{}")
        return self.send(404)
HTTPServer(("127.0.0.1", int(sys.argv[1])), H).serve_forever()
`

// TestDDShotSignInChromium is job stack S10 on Linux: dd-shot boots a
// one-service stack, then `dd-worker shot` drives Debian's Chromium over
// the DevTools protocol, inside bwrap with a browser job's settings. A form
// and an HTTP sign-in each screenshot the signed-in /account; a wrong
// password fails the sign-in with its reason and writes no PNG. Nothing is
// left running and the password is never printed. Run by
// packaging/test/sandbox_test.sh in the browser image, with the dd-worker
// binary at DD_WORKER_BIN; skipped elsewhere.
func TestDDShotSignInChromium(t *testing.T) {
	if os.Getenv("DD_BROWSER_E2E") != "1" {
		t.Skip("set DD_BROWSER_E2E=1 (packaging/test/sandbox_test.sh, browser image)")
	}
	chromium, err := exec.LookPath("chromium")
	if err != nil {
		t.Fatal("chromium not installed")
	}
	worker := os.Getenv("DD_WORKER_BIN")
	if worker == "" {
		t.Fatal("DD_WORKER_BIN not set")
	}
	const password = "seed-Pass!word1"
	form := &dexec.StackSignIn{URL: "/login", Email: "seed@example.com", Password: password, Method: dexec.SignInForm,
		Form:        &dexec.SignInFields{EmailSelector: "#email", PasswordSelector: "#password", SubmitSelector: "#go"},
		SuccessPath: "/account"}
	http := &dexec.StackSignIn{URL: "/login", Email: "seed@example.com", Password: password, Method: dexec.SignInHTTP,
		HTTP:        &dexec.SignInCall{Method: "POST", Path: "/api/login", BodyTemplate: `{"email":"{signin.email}","password":"{signin.password}"}`},
		SuccessPath: "/account"}
	wrong := func(si dexec.StackSignIn) *dexec.StackSignIn { si.Password = "not-the-password"; return &si }
	for _, c := range []struct {
		name string
		si   *dexec.StackSignIn
		want []string // lines dd-shot prints, in order (prefixes)
		pngs int
	}{
		{"form", form, []string{"DD-SHOT SIGNIN OK form", "DD-SHOT OK DEF-1 fix-evidence/DEF-1/fix-01.png ", "DD-SHOT OK DEF-1 fix-evidence/DEF-1/fix-02.png "}, 2},
		{"http", http, []string{"DD-SHOT SIGNIN OK http", "DD-SHOT OK DEF-1 fix-evidence/DEF-1/fix-01.png ", "DD-SHOT OK DEF-1 fix-evidence/DEF-1/fix-02.png "}, 2},
		{"form, wrong password", wrong(*form), []string{"DD-SHOT FAIL DEF-1 signin still-on-signin"}, 0},
		{"http, wrong password", wrong(*http), []string{"DD-SHOT FAIL DEF-1 signin http 401"}, 0},
	} {
		t.Run(c.name, func(t *testing.T) {
			tmp := t.TempDir()
			t.Setenv("HOME", filepath.Join(tmp, "home"))
			p := Paths{ConfigDir: filepath.Join(tmp, "etc"), StateDir: filepath.Join(tmp, "state")}
			handoff := p.Handoff("bjob_si1")
			wt := filepath.Join(p.WorktreeRoot("bjob_si1"), "app")
			writeFile(t, filepath.Join(wt, "app.py"), signInApp, 0o644)
			if err := os.MkdirAll(filepath.Join(handoff, "tmp"), 0o755); err != nil {
				t.Fatal(err)
			}
			app := dexec.AppSettings{PreviewCmd: "exec sleep 300", PreviewURL: "http://127.0.0.1:5174",
				JobStack:       []dexec.JobStackService{{Name: "web", Repo: "app", Start: "exec python3 app.py 5174", Port: 5174, Health: "/health"}},
				JobStackSignIn: c.si}
			st := dexec.DDStack{Services: app.JobStack, Dirs: map[string]string{"app": wt}, TimeoutSec: 60, Random: "r", SignIn: app.SignInValues()}
			if err := dexec.WriteDDStack(handoff, st); err != nil {
				t.Fatal(err)
			}
			vals := map[string]string{dexec.PlaceholderRandom: "r"}
			for k, v := range app.SignInValues() {
				vals[k] = v
			}
			if err := dexec.WriteShotSignIn(handoff, dexec.NewShotSignIn(app, vals)); err != nil {
				t.Fatal(err)
			}
			if err := dexec.WriteDDShot(handoff, dexec.DDShot{Browser: chromium, Dir: wt, PreviewCmd: app.PreviewCmd, PreviewURL: app.PreviewURL,
				Stack: true, TimeoutSec: 60, Worker: worker}); err != nil {
				t.Fatal(err)
			}
			host := (&hostAdapter{}).host(p, "bjob_si1", "", []git.WorktreeBinding{{Repo: "app", WorktreeAbs: wt}}, nil)
			task := aa.Task{Role: aa.Implementer, Handoff: handoff, Worktrees: []string{wt}, ReadOnly: host.ReadOnly,
				DenyRead: host.DenyRead(), DenyWrite: host.DenyWrite, UnixSockets: true}
			body, err := claude.Settings(task)
			if err != nil {
				t.Fatal(err)
			}
			args := append(bwrapArgs(t, body), "--chdir", handoff, "--setenv", "TMPDIR", filepath.Join(handoff, "tmp"),
				"sh", "dd-shot", "DEF-1", "/", "/account")
			start := time.Now()
			out, err := exec.Command("bwrap", args...).Output()
			t.Logf("dd-shot in %s (err %v):\n%s", time.Since(start).Round(time.Millisecond), err, out)
			lines := strings.Split(strings.TrimSpace(string(out)), "\n")
			if len(lines) != len(c.want) {
				log, _ := os.ReadFile(filepath.Join(handoff, ".dd-shot", "browser.log"))
				t.Fatalf("%d lines, want %d; browser log:\n%s", len(lines), len(c.want), log)
			}
			for i, w := range c.want {
				if !strings.HasPrefix(lines[i], w) {
					t.Fatalf("line %d %q, want prefix %q", i, lines[i], w)
				}
			}
			if (err == nil) != (c.pngs > 0) || strings.Contains(string(out), password) {
				t.Fatalf("exit %v for %d PNGs, or the password was printed", err, c.pngs)
			}
			pngs, _ := filepath.Glob(filepath.Join(handoff, "fix-evidence", "DEF-1", "*.png"))
			if len(pngs) != c.pngs {
				t.Fatalf("%d PNGs, want %d", len(pngs), c.pngs)
			}
			for _, f := range pngs {
				if b, _ := os.ReadFile(f); len(b) < 100 || string(b[:8]) != "\x89PNG\r\n\x1a\n" {
					t.Fatalf("%s is not a PNG", f)
				}
			}
			for _, s := range []string{"app.py 5174", "dd-shot-cdp-", "sleep 300"} {
				if running(s) {
					t.Fatalf("%q left running", s)
				}
			}
		})
	}
}
