package exec

import (
	"os"
	osexec "os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// ddShotEnv is a handoff with dd-shot written for a fake browser, and a bin
// dir with fake curl (up once the server touched $DD_TEST_MARK) and timeout.
type ddShotEnv struct {
	handoff, wt, bin, mark, pidFile string
}

func newDDShotEnv(t *testing.T, previewCmd string) ddShotEnv {
	t.Helper()
	dir := t.TempDir()
	e := ddShotEnv{handoff: filepath.Join(dir, "handoff"), wt: filepath.Join(dir, "wt"), bin: filepath.Join(dir, "bin"),
		mark: filepath.Join(dir, "up"), pidFile: filepath.Join(dir, "server.pid")}
	for _, d := range []string{e.handoff, e.wt, e.bin} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	write := func(p, body string) {
		if err := os.WriteFile(p, []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join(e.bin, "curl"), "#!/bin/sh\n[ -e \"$DD_TEST_MARK\" ]\n")
	write(filepath.Join(e.bin, "timeout"), "#!/bin/sh\nshift\nexec \"$@\"\n")
	// The browser writes a PNG for every URL but one containing /broken.
	browser := filepath.Join(e.bin, "chromium")
	write(browser, "#!/bin/sh\nout=\nfor a in \"$@\"; do case $a in --screenshot=*) out=${a#--screenshot=};; esac; url=$a; done\n"+
		"case $url in */broken*) echo 'FATAL: crashed' >&2; exit 134;; esac\nprintf '\\211PNG\\r\\n' > \"$out\"\n")
	if err := WriteDDShot(e.handoff, DDShot{Browser: browser, Dir: e.wt, PreviewCmd: previewCmd, PreviewURL: "http://127.0.0.1:5173/"}); err != nil {
		t.Fatal(err)
	}
	return e
}

func (e ddShotEnv) run(t *testing.T, env []string, args ...string) (string, int) {
	t.Helper()
	cmd := osexec.Command("sh", append([]string{DDShotName}, args...)...)
	cmd.Dir = e.handoff
	cmd.Env = append(os.Environ(), "PATH="+e.bin+":"+os.Getenv("PATH"), "DD_TEST_MARK="+e.mark, "DD_SHOT_WAIT=3",
		"DD_PREVIEW_CMD=", "DD_PREVIEW_URL=")
	cmd.Env = append(cmd.Env, env...)
	out, err := cmd.Output()
	code := 0
	if ee, ok := err.(*osexec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatal(err)
	}
	return string(out), code
}

// B3: dd-shot serves, waits, screenshots each path after the existing
// files, reports one status line each, and stops the server.
func TestDDShot(t *testing.T) {
	// The baked command, with a quote to prove the quoting; it records its
	// pid and cwd, marks itself up and stays up.
	e := newDDShotEnv(t, `echo $$ > "$DD_TEST_PID"; pwd > cwd.txt; touch "$DD_TEST_MARK"; echo 'it'"'"'s up'; exec sleep 60`)
	if err := os.MkdirAll(filepath.Join(e.handoff, "fix-evidence", "DEF-1"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(e.handoff, "fix-evidence", "DEF-1", "fix-01.png"), []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	out, code := e.run(t, []string{"DD_TEST_PID=" + e.pidFile}, "DEF-1", "/", "/broken", "/settings?tab=a")
	want := "DD-SHOT OK DEF-1 fix-evidence/DEF-1/fix-02.png 6B /\n" +
		"DD-SHOT FAIL DEF-1 /broken no-png rc=134 log=.dd-shot/browser.log\n" +
		"DD-SHOT OK DEF-1 fix-evidence/DEF-1/fix-04.png 6B /settings?tab=a\n"
	if out != want || code != 1 {
		t.Fatalf("exit %d, output:\n%s\nwant:\n%s", code, out, want)
	}
	if b, _ := os.ReadFile(filepath.Join(e.handoff, "fix-evidence", "DEF-1", "fix-01.png")); string(b) != "old" {
		t.Fatal("an existing screenshot was overwritten")
	}
	if _, err := os.Stat(filepath.Join(e.handoff, "fix-evidence", "DEF-1", "fix-03.png")); err == nil {
		t.Fatal("a failed screenshot left a file")
	}
	if cwd, _ := os.ReadFile(filepath.Join(e.wt, "cwd.txt")); !strings.HasSuffix(strings.TrimSpace(string(cwd)), "/wt") {
		t.Fatalf("preview ran in %q", cwd)
	}
	if log, _ := os.ReadFile(filepath.Join(e.handoff, ".dd-shot", "server.log")); string(log) != "it's up\n" {
		t.Fatalf("server log %q", log)
	}
	pidB, _ := os.ReadFile(e.pidFile)
	pid, err := strconv.Atoi(strings.TrimSpace(string(pidB)))
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for syscall.Kill(pid, 0) == nil {
		if time.Now().After(deadline) {
			t.Fatalf("preview server %d still running", pid)
		}
		time.Sleep(50 * time.Millisecond)
	}
	// Env wins over the baked values.
	os.Remove(e.mark)
	out, code = e.run(t, []string{"DD_PREVIEW_CMD=touch \"$DD_TEST_MARK\"; exec sleep 60", "DD_PREVIEW_URL=http://127.0.0.1:9/"}, "DEF-1", "/")
	if code != 0 || !strings.HasPrefix(out, "DD-SHOT OK DEF-1 fix-evidence/DEF-1/fix-05.png") {
		t.Fatalf("env preview: exit %d %q", code, out)
	}
}

func TestDDShotFailures(t *testing.T) {
	cases := []struct {
		name, cmd string
		args      []string
		want      string
		code      int
	}{
		{"no preview", "", []string{"DEF-1", "/"}, "DD-SHOT FAIL DEF-1 no-preview\n", 1},
		{"timeout", "exec sleep 60", []string{"DEF-1", "/"}, "DD-SHOT FAIL DEF-1 server-timeout 3s log=.dd-shot/server.log\n", 1},
		{"exited", "exit 3", []string{"DEF-1", "/"}, "DD-SHOT FAIL DEF-1 server-exited log=.dd-shot/server.log\n", 1},
		{"one arg", "x", []string{"DEF-1"}, "", 2},
		{"bad id", "x", []string{"DEF 1", "/"}, "", 2},
		{"dash id", "x", []string{"-rf", "/"}, "", 2},
		{"relative path", "x", []string{"DEF-1", "settings"}, "", 2},
		{"bad path", "x", []string{"DEF-1", "/a b"}, "", 2},
		{"quote path", "x", []string{"DEF-1", "/a'b"}, "", 2},
		{"nine paths", "x", []string{"DEF-1", "/", "/", "/", "/", "/", "/", "/", "/", "/"}, "", 2},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := newDDShotEnv(t, c.cmd)
			out, code := e.run(t, nil, c.args...)
			if code != c.code || (c.want != "" && out != c.want) || (c.code == 2 && !strings.HasPrefix(out, "DD-SHOT FAIL usage:")) {
				t.Fatalf("exit %d, output %q", code, out)
			}
		})
	}
}

// The tracked script is the template: exactly one line per baked value.
func TestWriteDDShotQuotes(t *testing.T) {
	dir := t.TempDir()
	if err := WriteDDShot(dir, DDShot{Browser: "/usr/bin/chromium", Dir: "/w", PreviewCmd: `a 'b' $(c)`, PreviewURL: "http://127.0.0.1:1"}); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(filepath.Join(dir, DDShotName))
	if !strings.Contains(string(b), `dd_cmd='a '\''b'\'' $(c)'`+"\n") || !strings.Contains(string(b), "dd_browser='/usr/bin/chromium'\n") {
		t.Fatalf("dd-shot header:\n%s", b[:600])
	}
	if fi, _ := os.Stat(filepath.Join(dir, DDShotName)); fi.Mode().Perm() != 0o755 {
		t.Fatalf("mode %v", fi.Mode())
	}
}

// A preview that forks (pnpm dev starting vite) is stopped as a whole
// process group. Needs setsid (Linux); dash's kill takes "-PGID", not "--".
func TestDDShotStopsProcessGroup(t *testing.T) {
	if _, err := osexec.LookPath("setsid"); err != nil {
		t.Skip("no setsid on this host")
	}
	e := newDDShotEnv(t, `sleep 60 & echo $! > "$DD_TEST_PID"; touch "$DD_TEST_MARK"; wait`)
	out, code := e.run(t, []string{"DD_TEST_PID=" + e.pidFile}, "DEF-1", "/")
	if code != 0 || !strings.HasPrefix(out, "DD-SHOT OK DEF-1 ") {
		t.Fatalf("exit %d %q", code, out)
	}
	pidB, _ := os.ReadFile(e.pidFile)
	pid, err := strconv.Atoi(strings.TrimSpace(string(pidB)))
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for syscall.Kill(pid, 0) == nil {
		if time.Now().After(deadline) {
			_ = syscall.Kill(pid, syscall.SIGKILL)
			t.Fatalf("the preview's child %d survived", pid)
		}
		time.Sleep(50 * time.Millisecond)
	}
}
