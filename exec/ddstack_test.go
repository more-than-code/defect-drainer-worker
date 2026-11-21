package exec

import (
	"encoding/json"
	"os"
	osexec "os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// ddStackEnv is a handoff with dd-stack, two worktrees, fake Postgres
// binaries and a fake curl: a health URL answers once $DD_TEST_MARKS/up-<port>
// exists. Every fake process records its pid in $DD_TEST_MARKS/pids.
type ddStackEnv struct {
	handoff, marks, bin, tmp string
	stack                    DDStack
}

const testRandom = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func newDDStackEnv(t *testing.T, mutate func(*DDStack)) ddStackEnv {
	t.Helper()
	dir := t.TempDir()
	e := ddStackEnv{handoff: filepath.Join(dir, "handoff"), marks: filepath.Join(dir, "marks"),
		bin: filepath.Join(dir, "bin"), tmp: filepath.Join(dir, "tmp")}
	pg := filepath.Join(dir, "pg")
	api, web := filepath.Join(dir, "wt-api"), filepath.Join(dir, "wt-web")
	for _, d := range []string{e.handoff, e.marks, e.bin, e.tmp, pg, api, web} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	write := func(p, body string) {
		if err := os.WriteFile(p, []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join(e.bin, "curl"), "#!/bin/sh\nfor a in \"$@\"; do url=$a; done\np=${url#http://127.0.0.1:}\np=${p%%/*}\n[ -e \"$DD_TEST_MARKS/up-$p\" ]\n")
	write(filepath.Join(pg, "initdb"), "#!/bin/sh\nwhile [ $# -gt 0 ]; do [ \"$1\" = -D ] && d=$2; shift; done\nmkdir -p \"$d\" && echo initdb ok\n")
	// pg_ctl start keeps a sleep as the "postmaster"; stop kills it.
	write(filepath.Join(pg, "pg_ctl"), `#!/bin/sh
while [ $# -gt 0 ]; do
	case $1 in
	-D) d=$2; shift ;;
	-o) o=$2; shift ;;
	start | stop) act=$1 ;;
	esac
	shift
done
case $act in
start)
	set -- $o
	echo "$2" >"$d/port"
	sleep 60 &
	echo $! >"$d/pid"
	echo $! >>"$DD_TEST_MARKS/pids"
	echo "server started" ;;
stop)
	kill "$(cat "$d/pid")" ;;
esac
`)
	write(filepath.Join(pg, "postgres"), "#!/bin/sh\necho 'postgres (PostgreSQL) 15.10'\n")
	rec := `echo "$0 $DATABASE_URL $SECRET $API_URL" >>"$DD_TEST_MARKS/steps"`
	e.stack = DDStack{
		Services: []JobStackService{
			{Name: "api", Repo: "api", Install: "echo api-install >>\"$DD_TEST_MARKS/order\"",
				Migrate: "echo api-migrate >>\"$DD_TEST_MARKS/order\"; " + rec,
				Seed:    []string{"echo api-seed-1 >>\"$DD_TEST_MARKS/order\"", "echo api-seed-2 >>\"$DD_TEST_MARKS/order\"; echo seeded"},
				Start:   "echo $$ >>\"$DD_TEST_MARKS/pids\"; pwd >\"$DD_TEST_MARKS/api-cwd\"; touch \"$DD_TEST_MARKS/up-3000\"; exec sleep 60",
				Port:    3000, Health: "/health", NeedsPostgres: true,
				Env: map[string]string{"DATABASE_URL": "{postgres.url}", "SECRET": "{random}", "API_URL": "{services.api.url}"}},
			{Name: "web", Repo: "web",
				Start: "echo web-start >>\"$DD_TEST_MARKS/order\"; echo $$ >>\"$DD_TEST_MARKS/pids\"; touch \"$DD_TEST_MARKS/up-5173\"; exec sleep 60",
				Port:  5173, Health: "/"},
		},
		PreviewEnv: map[string]string{"BACKEND_URL": "{services.api.url}/v1", "BFF_SHARED_SECRET": "{random}", "PLAIN": "it's {x}"},
		Dirs:       map[string]string{"api": api, "web": web},
		Postgres:   pg, TimeoutSec: 20, Random: testRandom,
	}
	if mutate != nil {
		mutate(&e.stack)
	}
	if err := WriteDDStack(e.handoff, e.stack); err != nil {
		t.Fatal(err)
	}
	return e
}

func (e ddStackEnv) run(t *testing.T, args ...string) (string, int) {
	t.Helper()
	cmd := osexec.Command("sh", append([]string{DDStackName}, args...)...)
	cmd.Dir = e.handoff
	cmd.Env = append(os.Environ(), "PATH="+e.bin+":"+os.Getenv("PATH"), "DD_TEST_MARKS="+e.marks, "TMPDIR="+e.tmp)
	out, err := cmd.Output()
	code := 0
	if ee, ok := err.(*osexec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatal(err)
	}
	return string(out), code
}

func (e ddStackEnv) read(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(e.marks, name))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// assertAllStopped checks every recorded process is gone and $TMPDIR is
// empty again.
func (e ddStackEnv) assertAllStopped(t *testing.T) {
	t.Helper()
	b, _ := os.ReadFile(filepath.Join(e.marks, "pids"))
	for _, f := range strings.Fields(string(b)) {
		pid, err := strconv.Atoi(f)
		if err != nil {
			t.Fatal(err)
		}
		deadline := time.Now().Add(3 * time.Second)
		for syscall.Kill(pid, 0) == nil {
			if time.Now().After(deadline) {
				_ = syscall.Kill(pid, syscall.SIGKILL)
				t.Fatalf("process %d still running", pid)
			}
			time.Sleep(50 * time.Millisecond)
		}
	}
	if left, _ := os.ReadDir(e.tmp); len(left) != 0 {
		t.Fatalf("$TMPDIR keeps %d entries", len(left))
	}
}

var stepTime = regexp.MustCompile(` \d+s\n`)

// S4: Postgres, then each service in order with its env, then the command
// with the preview env; status lines only; everything stopped after.
func TestDDStackRun(t *testing.T) {
	e := newDDStackEnv(t, nil)
	out, code := e.run(t, "run", "--", "sh", "-c", `echo "$BACKEND_URL|$BFF_SHARED_SECRET|$PLAIN|$SECRET" >"$DD_TEST_MARKS/cmd"; echo hello; exit 7`)
	if code != 7 {
		t.Fatalf("exit %d, want the command's 7; output:\n%s", code, out)
	}
	m := regexp.MustCompile(`DD-STACK UP postgres 127\.0\.0\.1:(\d+)\n`).FindStringSubmatch(out)
	if m == nil {
		t.Fatalf("no postgres line:\n%s", out)
	}
	want := "DD-STACK UP postgres 127.0.0.1:" + m[1] + "\n" +
		"DD-STACK OK api install\nDD-STACK OK api migrate\nDD-STACK OK api seed-1\nDD-STACK OK api seed-2\n" +
		"DD-STACK UP api http://127.0.0.1:3000\nDD-STACK UP web http://127.0.0.1:5173\n" +
		"DD-STACK RUN\nhello\nDD-STACK DOWN\n"
	if got := stepTime.ReplaceAllString(out, "\n"); got != want {
		t.Fatalf("output:\n%s\nwant:\n%s", got, want)
	}
	if strings.Contains(out, testRandom) {
		t.Fatal("the output shows the {random} value")
	}
	if got := e.read(t, "order"); got != "api-install\napi-migrate\napi-seed-1\napi-seed-2\nweb-start\n" {
		t.Fatalf("order %q", got)
	}
	if got, want := e.read(t, "steps"), "sh postgresql://postgres@127.0.0.1:"+m[1]+"/postgres "+testRandom+" http://127.0.0.1:3000\n"; got != want {
		t.Fatalf("step env %q, want %q", got, want)
	}
	if got, want := e.read(t, "cmd"), "http://127.0.0.1:3000/v1|"+testRandom+"|it's {x}|\n"; got != want {
		t.Fatalf("command env %q, want %q (no service env)", got, want)
	}
	if cwd := strings.TrimSpace(e.read(t, "api-cwd")); !strings.HasSuffix(cwd, "/wt-api") {
		t.Fatalf("api ran in %q", cwd)
	}
	if b, _ := os.ReadFile(filepath.Join(e.handoff, ".dd-stack", "api-seed-2.log")); string(b) != "seeded\n" {
		t.Fatalf("seed log %q", b)
	}
	e.assertAllStopped(t)
}

// S4, AC4: a failed step names the service and step with its log's tail,
// exits 125 without running the command, and stops what had started.
func TestDDStackStepFails(t *testing.T) {
	e := newDDStackEnv(t, func(s *DDStack) {
		s.Services[1].Install = "echo line-1; echo line-2; exit 3"
	})
	out, code := e.run(t, "run", "--", "touch", filepath.Join(e.marks, "ran"))
	want := "DD-STACK FAIL web install log=.dd-stack/web-install.log\nline-1\nline-2\nDD-STACK DOWN\n"
	if code != 125 || !strings.HasSuffix(out, want) {
		t.Fatalf("exit %d, output:\n%s\nwant suffix:\n%s", code, out, want)
	}
	if !strings.Contains(out, "DD-STACK UP api http://127.0.0.1:3000\n") {
		t.Fatalf("api did not come up first:\n%s", out)
	}
	if _, err := os.Stat(filepath.Join(e.marks, "ran")); err == nil {
		t.Fatal("the command ran on a failed stack")
	}
	if b, _ := os.ReadFile(filepath.Join(e.handoff, ".dd-stack", "failed")); string(b) != "web install log=.dd-stack/web-install.log\n" {
		t.Fatalf("failed file %q", b)
	}
	e.assertAllStopped(t)
}

// S6: a service that never passes health fails within the preview timeout,
// named, with its start log.
func TestDDStackHealthFails(t *testing.T) {
	e := newDDStackEnv(t, func(s *DDStack) {
		s.Services = s.Services[1:]
		s.Services[0].Start = "echo $$ >>\"$DD_TEST_MARKS/pids\"; echo listening-nowhere; exec sleep 60"
		s.TimeoutSec = 2
	})
	start := time.Now()
	out, code := e.run(t, "run", "--", "true")
	want := "DD-STACK FAIL web health log=.dd-stack/web-start.log\nlistening-nowhere\nDD-STACK DOWN\n"
	if code != 125 || !strings.HasSuffix(out, want) {
		t.Fatalf("exit %d, output:\n%s\nwant suffix:\n%s", code, out, want)
	}
	if d := time.Since(start); d > 15*time.Second {
		t.Fatalf("took %s", d)
	}
	e.assertAllStopped(t)
}

// A start command that exits is named at once.
func TestDDStackStartExits(t *testing.T) {
	e := newDDStackEnv(t, func(s *DDStack) {
		s.Services = s.Services[1:]
		s.Services[0].Start = "echo boom; exit 1"
	})
	out, code := e.run(t, "run", "--", "true")
	if code != 125 || !strings.HasSuffix(out, "DD-STACK FAIL web start log=.dd-stack/web-start.log\nboom\nDD-STACK DOWN\n") {
		t.Fatalf("exit %d, output:\n%s", code, out)
	}
	if strings.Contains(out, "postgres") {
		t.Fatalf("Postgres started though no service needs it:\n%s", out)
	}
	e.assertAllStopped(t)
}

func TestDDStackUsage(t *testing.T) {
	e := newDDStackEnv(t, nil)
	for _, args := range [][]string{{}, {"run"}, {"run", "--"}, {"up", "--", "true"}, {"run", "true", "x"}} {
		if out, code := e.run(t, args...); code != 2 || !strings.HasPrefix(out, "DD-STACK FAIL usage:") {
			t.Fatalf("%q: exit %d %q", args, code, out)
		}
	}
}

// The definition is quoted: a value with quotes and $ stays text.
func TestStackValueQuoting(t *testing.T) {
	vals := DDStack{Services: []JobStackService{{Name: "api", Port: 3000}}, Random: "R"}.placeholderValues()
	got := stackValue(`a'b $HOME {postgres.url}?x={random}&u={services.api.url} {services.nope.url}`, vals)
	want := `'a'\''b $HOME '"$dd_pg_url"'?x=R&u=http://127.0.0.1:3000 {services.nope.url}'`
	if got != want {
		t.Fatalf("got  %s\nwant %s", got, want)
	}
}

func TestWriteDDStackRefuses(t *testing.T) {
	dir := t.TempDir()
	s := DDStack{Services: []JobStackService{{Name: "api", Repo: "api", Start: "x", Port: 3000, Health: "/", NeedsPostgres: true}},
		Dirs: map[string]string{"api": dir}, TimeoutSec: 90}
	if err := WriteDDStack(dir, s); err == nil {
		t.Fatal("wrote a Postgres stack with no Postgres")
	}
	s.Postgres, s.Dirs = "/pg", map[string]string{}
	if err := WriteDDStack(dir, s); err == nil {
		t.Fatal("wrote a service with no worktree")
	}
}

// AC6: without services dd-shot is rc5's, byte for byte.
func TestDDShotNoStackIsRC5(t *testing.T) {
	rc5, err := os.ReadFile(filepath.Join("testdata", "ddshot-rc5.sh"))
	if err != nil {
		t.Fatal(err)
	}
	c := DDShot{Browser: "/usr/bin/chromium", Dir: "/w/web", PreviewCmd: "pnpm dev --port 5173", PreviewURL: "http://127.0.0.1:5173/", TimeoutSec: 200}
	want := string(rc5)
	for _, kv := range [][2]string{{"dd_browser", c.Browser}, {"dd_dir", c.Dir}, {"dd_cmd", c.PreviewCmd}, {"dd_url", c.PreviewURL}} {
		want = strings.Replace(want, kv[0]+"=''\n", kv[0]+"="+shQuote(kv[1])+"\n", 1)
	}
	dir := t.TempDir()
	if err := WriteDDShot(dir, c); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(filepath.Join(dir, DDShotName))
	if string(got) != want {
		t.Fatal("dd-shot without services differs from rc5")
	}
	c.Stack = true
	if err := WriteDDShot(dir, c); err != nil {
		t.Fatal(err)
	}
	got, _ = os.ReadFile(filepath.Join(dir, DDShotName))
	if !strings.Contains(string(got), "dd_wait=200 # dd-stack\n") || !strings.Contains(string(got), `run -- sh -c`) {
		t.Fatal("the stack variant lacks its lines")
	}
}

// withDDShot adds dd-shot's stack variant to e, previewing on 5174 with a
// fake browser and timeout.
func (e ddStackEnv) withDDShot(t *testing.T, previewCmd string) {
	t.Helper()
	write := func(p, body string) {
		if err := os.WriteFile(p, []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join(e.bin, "timeout"), "#!/bin/sh\nshift\nexec \"$@\"\n")
	browser := filepath.Join(e.bin, "chromium")
	write(browser, "#!/bin/sh\nfor a in \"$@\"; do case $a in --screenshot=*) out=${a#--screenshot=};; esac; done\nprintf '\\211PNG\\r\\n' > \"$out\"\n")
	if err := WriteDDShot(e.handoff, DDShot{Browser: browser, Dir: e.stack.Dirs["web"], PreviewCmd: previewCmd,
		PreviewURL: "http://127.0.0.1:5174/", Stack: true, TimeoutSec: e.stack.TimeoutSec}); err != nil {
		t.Fatal(err)
	}
}

// S4: with services, dd-shot's preview runs inside dd-stack with the
// preview env, and the stack is down afterwards.
func TestDDShotThroughStack(t *testing.T) {
	e := newDDStackEnv(t, nil)
	e.withDDShot(t, `echo "$BACKEND_URL" >"$DD_TEST_MARKS/preview-env"; echo $$ >>"$DD_TEST_MARKS/pids"; touch "$DD_TEST_MARKS/up-5174"; exec sleep 60`)
	cmd := osexec.Command("sh", DDShotName, "DEF-1", "/dictionary?q=narrow")
	cmd.Dir = e.handoff
	cmd.Env = append(os.Environ(), "PATH="+e.bin+":"+os.Getenv("PATH"), "DD_TEST_MARKS="+e.marks, "TMPDIR="+e.tmp,
		"DD_PREVIEW_CMD=", "DD_PREVIEW_URL=", "DD_SHOT_WAIT=")
	out, err := cmd.Output()
	if err != nil || string(out) != "DD-SHOT OK DEF-1 fix-evidence/DEF-1/fix-01.png 6B /dictionary?q=narrow\n" {
		t.Fatalf("%v %q", err, out)
	}
	if got := e.read(t, "preview-env"); got != "http://127.0.0.1:3000/v1\n" {
		t.Fatalf("preview env %q", got)
	}
	log, _ := os.ReadFile(filepath.Join(e.handoff, ".dd-shot", "server.log"))
	if !strings.Contains(string(log), "DD-STACK UP web http://127.0.0.1:5173\n") || strings.Contains(string(log), testRandom) {
		t.Fatalf("server log:\n%s", log)
	}
	e.assertAllStopped(t)
}

// AC4: a service failing health is named in dd-shot's status line, with
// its last log lines, and no screenshot is taken.
func TestDDShotStackFails(t *testing.T) {
	// web alone, so the 4 s deadline is all health wait, even under load.
	e := newDDStackEnv(t, func(s *DDStack) {
		s.Services = s.Services[1:]
		s.Services[0].Start = "echo $$ >>\"$DD_TEST_MARKS/pids\"; echo web-broke; exec sleep 60"
		s.PreviewEnv = nil
		s.TimeoutSec = 4
	})
	e.withDDShot(t, `touch "$DD_TEST_MARKS/up-5174"; exec sleep 60`)
	cmd := osexec.Command("sh", DDShotName, "DEF-1", "/")
	cmd.Dir = e.handoff
	cmd.Env = append(os.Environ(), "PATH="+e.bin+":"+os.Getenv("PATH"), "DD_TEST_MARKS="+e.marks, "TMPDIR="+e.tmp,
		"DD_PREVIEW_CMD=", "DD_PREVIEW_URL=", "DD_SHOT_WAIT=")
	out, _ := cmd.Output()
	if code := cmd.ProcessState.ExitCode(); code != 1 || string(out) != "DD-SHOT FAIL DEF-1 stack web health log=.dd-stack/web-start.log\nweb-broke\n" {
		t.Fatalf("exit %d %q", code, out)
	}
	if left, _ := os.ReadDir(filepath.Join(e.handoff, "fix-evidence", "DEF-1")); len(left) != 0 {
		t.Fatal("a screenshot was taken")
	}
	e.assertAllStopped(t)
}

// S10: with a Stack sign-in, dd-stack exports DD_SIGNIN_* to the services
// and the command, and fills the {signin.*} placeholders.
func TestDDStackSignInEnv(t *testing.T) {
	a := ProjectSettings{PreviewURL: "http://127.0.0.1:5174/", JobStackSignIn: &StackSignIn{URL: "/login", Email: "seed@example.com", Password: "it's-a-pass"}}
	e := newDDStackEnv(t, func(s *DDStack) {
		s.SignIn = a.SignInValues()
		s.Services = s.Services[1:]
		s.Services[0].Install = `echo "$DD_SIGNIN_EMAIL" >"$DD_TEST_MARKS/svc-signin"`
		s.PreviewEnv = map[string]string{"AS": "{signin.email}|{signin.url}"}
	})
	out, code := e.run(t, "run", "--", "sh", "-c", `echo "$AS|$DD_SIGNIN_URL|$DD_SIGNIN_PASSWORD" >"$DD_TEST_MARKS/cmd"`)
	if code != 0 || strings.Contains(out, "it's-a-pass") {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	if got := e.read(t, "cmd"); got != "seed@example.com|http://127.0.0.1:5174/login|http://127.0.0.1:5174/login|it's-a-pass\n" {
		t.Fatalf("command env %q", got)
	}
	if got := e.read(t, "svc-signin"); got != "seed@example.com\n" {
		t.Fatalf("service env %q", got)
	}
	e.assertAllStopped(t)
}

// The HTTP body is JSON: placeholder values are escaped into it.
func TestNewShotSignIn(t *testing.T) {
	a := ProjectSettings{PreviewURL: "http://127.0.0.1:5173", JobStack: []JobStackService{{Name: "api", Port: 3000}},
		JobStackSignIn: &StackSignIn{URL: "/login", Email: "seed@example.com", Password: `pa"ss\word<1>`, Method: SignInHTTP,
			HTTP:        &SignInCall{Method: "POST", Path: "/api/login", BodyTemplate: `{"e":"{signin.email}","p":"{signin.password}","r":"{random}","u":"{services.api.url}"}`},
			SuccessPath: "/account?tab=1"}}
	vals := DDStack{Services: a.JobStack, Random: "R", SignIn: a.SignInValues()}.placeholderValues()
	got := NewShotSignIn(a, vals)
	if got.HTTPBody != `{"e":"seed@example.com","p":"pa\"ss\\word\u003c1\u003e","r":"R","u":"http://127.0.0.1:3000"}` {
		t.Fatalf("body %s", got.HTTPBody)
	}
	var probe map[string]string
	if err := json.Unmarshal([]byte(got.HTTPBody), &probe); err != nil || probe["p"] != a.JobStackSignIn.Password {
		t.Fatalf("body is not the JSON it should be: %v %q", err, probe["p"])
	}
	if got.URL != "http://127.0.0.1:5173/login" || got.HTTPURL != "http://127.0.0.1:5173/api/login" || got.SuccessURL != "http://127.0.0.1:5173/account?tab=1" {
		t.Fatalf("urls %+v", got)
	}
}

// S10: dd-shot's sign-in variant boots the stack, waits for the app, then
// hands every screenshot to dd-worker shot with its arguments, passes its
// status on, and stops the stack. Without a sign-in, the job-stack variant
// is 6404704's.
func TestDDShotSignIn(t *testing.T) {
	e := newDDStackEnv(t, func(s *DDStack) { s.Services = s.Services[1:]; s.PreviewEnv = nil })
	worker := filepath.Join(e.bin, "dd-worker")
	if err := os.WriteFile(worker, []byte("#!/bin/sh\necho \"$*\" >\"$DD_TEST_MARKS/shot-args\"\necho 'DD-SHOT SIGNIN OK form'\necho 'DD-SHOT FAIL DEF-1 / no-png x'\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	e.withDDShot(t, `echo $$ >>"$DD_TEST_MARKS/pids"; touch "$DD_TEST_MARKS/up-5174"; exec sleep 60`)
	c := DDShot{Browser: filepath.Join(e.bin, "chromium"), Dir: e.stack.Dirs["web"], PreviewCmd: `echo $$ >>"$DD_TEST_MARKS/pids"; touch "$DD_TEST_MARKS/up-5174"; exec sleep 60`,
		PreviewURL: "http://127.0.0.1:5174/", Stack: true, TimeoutSec: e.stack.TimeoutSec, Worker: worker}
	if err := WriteDDShot(e.handoff, c); err != nil {
		t.Fatal(err)
	}
	cmd := osexec.Command("sh", DDShotName, "DEF-1", "/", "/account")
	cmd.Dir = e.handoff
	cmd.Env = append(os.Environ(), "PATH="+e.bin+":"+os.Getenv("PATH"), "DD_TEST_MARKS="+e.marks, "TMPDIR="+e.tmp,
		"DD_PREVIEW_CMD=", "DD_PREVIEW_URL=", "DD_SHOT_WAIT=")
	out, _ := cmd.Output()
	if code := cmd.ProcessState.ExitCode(); code != 1 || string(out) != "DD-SHOT SIGNIN OK form\nDD-SHOT FAIL DEF-1 / no-png x\n" {
		t.Fatalf("exit %d %q", code, out)
	}
	// The config path is the handoff's as dd-shot's pwd prints it (on a Mac,
	// under /private).
	head, tail := "shot --browser "+c.Browser+" --config /", "/handoff/dd-shot-signin.json --def DEF-1 --dir fix-evidence/DEF-1 --after 0 --url http://127.0.0.1:5174 --timeout 60 -- / /account\n"
	if got := e.read(t, "shot-args"); !strings.HasPrefix(got, head) || !strings.HasSuffix(got, tail) {
		t.Fatalf("args %q", got)
	}
	e.assertAllStopped(t)

	// A sign-in needs the job stack; without one the variant is 6404704's.
	c.Stack = false
	if err := WriteDDShot(t.TempDir(), c); err == nil {
		t.Fatal("wrote a sign-in dd-shot without the job stack")
	}
	c.Stack, c.Worker = true, ""
	dir := t.TempDir()
	if err := WriteDDShot(dir, c); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, DDShotName)); strings.Contains(string(b), "dd-signin") {
		t.Fatal("the job-stack variant has sign-in lines")
	}
}
