//go:build linux

package worker

import (
	"net"
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

// TestBrowserSandboxBwrap is worker-browser AC3 and AC6 on Linux, the way
// TestSandboxLayoutBwrap does it: the real scan and the real settings file,
// applied with bwrap. It proves the masks and the layout. Claude's own
// seccomp unix-socket switch needs the real CLI; probe P2 covered that.
//   - a planted 0666 socket under a fake /run, owned by someone else, is
//     found by the scan and cannot be reached from inside;
//   - without the mask (the negative control) it can;
//   - a socket made in $TMPDIR inside works;
//   - a job without Browser gets exactly the settings it got before.
//
// Run by packaging/test/sandbox_test.sh as root in a container (socat,
// bwrap); skipped elsewhere.
func TestBrowserSandboxBwrap(t *testing.T) {
	if os.Getenv("DD_SANDBOX_E2E") != "1" {
		t.Skip("set DD_SANDBOX_E2E=1 (packaging/test/sandbox_test.sh)")
	}
	for _, bin := range []string{"bwrap", "socat"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Fatalf("%s not installed", bin)
		}
	}
	if os.Geteuid() != 0 {
		t.Fatal("needs root, to plant a socket owned by another user")
	}
	tmp := t.TempDir()
	home := filepath.Join(tmp, "home")
	t.Setenv("HOME", home)
	if err := os.MkdirAll(home, 0o755); err != nil {
		t.Fatal(err)
	}
	p := Paths{ConfigDir: filepath.Join(tmp, "etc"), StateDir: filepath.Join(tmp, "state")}
	fakeRun := filepath.Join(tmp, "run")
	sock := filepath.Join(fakeRun, "example", "agent.sock")
	if err := os.MkdirAll(filepath.Dir(sock), 0o755); err != nil {
		t.Fatal(err)
	}
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			_ = c.Close()
		}
	}()
	if err := os.Chmod(sock, 0o666); err != nil {
		t.Fatal(err)
	}
	// The daemon's dir and socket belong to another user (the test runs as
	// root, which is "the worker" here).
	for _, f := range []string{sock, filepath.Dir(sock)} {
		if err := os.Chown(f, 65534, 65534); err != nil {
			t.Fatal(err)
		}
	}

	scan := newSocketScan(p, nil)
	scan.roots = []string{fakeRun}
	masked, dropped, complete := scan.run()
	if !complete || len(dropped) != 0 || strings.Join(masked, ",") != filepath.Dir(sock) {
		t.Fatalf("scan: masked %v dropped %v complete %v", masked, dropped, complete)
	}

	handoff := p.Handoff("bjob_wb1")
	wt := filepath.Join(p.WorktreeRoot("bjob_wb1"), "app")
	for _, d := range []string{handoff, wt, filepath.Join(handoff, "tmp")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	host := (&hostAdapter{}).host(p, "bjob_wb1", "", []git.WorktreeBinding{{Repo: "app", WorktreeAbs: wt}}, nil)
	plain := aa.Task{Role: aa.Implementer, Handoff: handoff, Worktrees: []string{wt}, ReadOnly: host.ReadOnly, DenyRead: host.DenyRead(), DenyWrite: host.DenyWrite}
	browser := plain
	browser.UnixSockets = true
	browser.DenyRead = append(append([]string(nil), plain.DenyRead...), masked...)

	// AC6: without Browser nothing about unix sockets reaches the file.
	plainBody, err := claude.Settings(plain)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(plainBody), "allowAllUnixSockets") || strings.Contains(string(plainBody), fakeRun) {
		t.Fatalf("a job without Browser got browser settings:\n%s", plainBody)
	}
	browserBody, err := claude.Settings(browser)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(browserBody), `"allowAllUnixSockets": true`) {
		t.Fatalf("browser settings:\n%s", browserBody)
	}

	connect := "socat -u OPEN:/dev/null UNIX-CONNECT:" + sock
	tmpSock := "T=" + handoff + "/tmp; socat UNIX-LISTEN:$T/s.sock OPEN:/dev/null & pid=$!; i=0; " +
		"while [ ! -S $T/s.sock ] && [ $i -lt 50 ]; do sleep 0.1; i=$((i+1)); done; " +
		"socat -u OPEN:/dev/null UNIX-CONNECT:$T/s.sock; rc=$?; kill $pid 2>/dev/null; exit $rc"
	for _, c := range []struct {
		what string
		body []byte
		sh   string
		ok   bool
	}{
		{"no mask: the host socket is reachable (control)", plainBody, connect, true},
		{"browser job: the host socket is masked", browserBody, connect, false},
		{"browser job: the socket's dir is empty", browserBody, "[ -z \"$(ls -A " + filepath.Dir(sock) + ")\" ]", true},
		{"browser job: a socket in $TMPDIR works", browserBody, tmpSock, true},
		{"browser job: still writes its handoff", browserBody, "echo x > " + handoff + "/w.txt", true},
		{"browser job: still cannot write $HOME", browserBody, "echo x > " + home + "/w.txt", false},
	} {
		cmd := exec.Command("bwrap", append(bwrapArgs(t, c.body), "sh", "-c", c.sh)...)
		out, err := cmd.CombinedOutput()
		if (err == nil) != c.ok {
			t.Errorf("%s: ok=%v, want %v\n%s", c.what, err == nil, c.ok, out)
		} else {
			t.Logf("%s: ok=%v as expected", c.what, err == nil)
		}
	}
}

// running reports whether a process's command line contains s.
func running(s string) bool {
	ps, _ := filepath.Glob("/proc/[0-9]*/cmdline")
	for _, p := range ps {
		b, _ := os.ReadFile(p)
		if strings.Contains(strings.ReplaceAll(string(b), "\x00", " "), s) {
			return true
		}
	}
	return false
}

// TestDDShotChromium runs dd-shot with Debian's Chromium against a static
// page, inside bwrap with a browser job's filesystem rules: a PNG is
// written. Run by packaging/test/sandbox_test.sh when the browser image
// built; skipped elsewhere.
func TestDDShotChromium(t *testing.T) {
	if os.Getenv("DD_BROWSER_E2E") != "1" {
		t.Skip("set DD_BROWSER_E2E=1 (packaging/test/sandbox_test.sh, browser image)")
	}
	chromium, err := exec.LookPath("chromium")
	if err != nil {
		t.Fatal("chromium not installed")
	}
	tmp := t.TempDir()
	t.Setenv("HOME", filepath.Join(tmp, "home"))
	p := Paths{ConfigDir: filepath.Join(tmp, "etc"), StateDir: filepath.Join(tmp, "state")}
	handoff := p.Handoff("bjob_wb2")
	wt := filepath.Join(p.WorktreeRoot("bjob_wb2"), "app")
	writeFile(t, filepath.Join(wt, "site", "index.html"), "<!doctype html><title>dd</title><body style=\"background:#4a8\">dd-shot</body>\n", 0o644)
	if err := os.MkdirAll(filepath.Join(handoff, "tmp"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := dexec.WriteDDShot(handoff, dexec.DDShot{Browser: chromium, Dir: wt,
		PreviewCmd: "python3 -m http.server 5173 --bind 127.0.0.1 --directory site", PreviewURL: "http://127.0.0.1:5173"}); err != nil {
		t.Fatal(err)
	}
	host := (&hostAdapter{}).host(p, "bjob_wb2", "", []git.WorktreeBinding{{Repo: "app", WorktreeAbs: wt}}, nil)
	task := aa.Task{Role: aa.Implementer, Handoff: handoff, Worktrees: []string{wt}, ReadOnly: host.ReadOnly,
		DenyRead: host.DenyRead(), DenyWrite: host.DenyWrite, UnixSockets: true}
	body, err := claude.Settings(task)
	if err != nil {
		t.Fatal(err)
	}
	args := append(bwrapArgs(t, body), "--chdir", handoff, "--setenv", "TMPDIR", filepath.Join(handoff, "tmp"), "sh", "dd-shot", "DEF-1", "/")
	start := time.Now()
	out, err := exec.Command("bwrap", args...).Output()
	t.Logf("dd-shot in %s:\n%s", time.Since(start).Round(time.Millisecond), out)
	if err != nil || !strings.HasPrefix(string(out), "DD-SHOT OK DEF-1 fix-evidence/DEF-1/fix-01.png ") {
		log, _ := os.ReadFile(filepath.Join(handoff, ".dd-shot", "browser.log"))
		t.Fatalf("dd-shot: %v\n%s\nbrowser log:\n%s", err, out, log)
	}
	png, err := os.ReadFile(filepath.Join(handoff, "fix-evidence", "DEF-1", "fix-01.png"))
	if err != nil || len(png) < 100 || string(png[:8]) != "\x89PNG\r\n\x1a\n" {
		t.Fatalf("not a PNG (%d bytes): %v", len(png), err)
	}
	if running("http.server 5173") {
		t.Fatal("preview server left running")
	}
}
