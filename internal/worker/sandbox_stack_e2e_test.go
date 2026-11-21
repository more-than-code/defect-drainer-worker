//go:build linux

package worker

import (
	"context"
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

// TestDDStackPostgresBwrap is job stack AC7 on Linux: dd-stack boots
// a sample stack inside bwrap with a browser job's generated settings, the
// way TestDDShotChromium runs dd-shot. The stack is a throwaway Postgres
// from the distro's binaries (policy agent_postgres) and a tiny HTTP service
// (socat + psql) whose /health answers 200 only once its seeded row is in
// the database. It runs a command against it, then checks nothing is left
// running. bwrap maps the user to 1000, since Postgres refuses root.
//
// Run by packaging/test/sandbox_test.sh in the stack image; skipped
// elsewhere.
func TestDDStackPostgresBwrap(t *testing.T) {
	if os.Getenv("DD_STACK_E2E") != "1" {
		t.Skip("set DD_STACK_E2E=1 (packaging/test/sandbox_test.sh, stack image)")
	}
	pgs, _ := filepath.Glob("/usr/lib/postgresql/*/bin")
	if len(pgs) != 1 {
		t.Fatalf("want one Postgres bin dir, found %v", pgs)
	}
	pg := pgs[0]
	for _, bin := range []string{"bwrap", "socat", "psql", "curl", "setsid"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Fatalf("%s not installed", bin)
		}
	}
	// The host's binaries pass the agent_postgres readiness check as root's.
	if st := checkPostgres(context.Background(), pg, nil); !st.Ready {
		t.Fatalf("postgres readiness %+v", st)
	}

	tmp := t.TempDir()
	t.Setenv("HOME", filepath.Join(tmp, "home"))
	p := Paths{ConfigDir: filepath.Join(tmp, "etc"), StateDir: filepath.Join(tmp, "state")}
	handoff := p.Handoff("bjob_ps1")
	wt := filepath.Join(p.WorktreeRoot("bjob_ps1"), "app")
	if err := os.MkdirAll(filepath.Join(handoff, "tmp"), 0o755); err != nil {
		t.Fatal(err)
	}
	// The service, as an app's repo would hold it.
	writeFile(t, filepath.Join(wt, "serve.sh"), `#!/bin/sh
read -r _
v=$(psql "$DATABASE_URL" -tAc 'select v from t limit 1' 2>/dev/null)
if [ -n "$v" ] && [ -n "$APP_TOKEN" ]; then
	printf 'HTTP/1.0 200 OK\r\nContent-Type: text/plain\r\n\r\n%s\n' "$v"
else
	printf 'HTTP/1.0 503 Service Unavailable\r\n\r\n'
fi
`, 0o755)
	stack := dexec.DDStack{
		Services: []dexec.JobStackService{{
			Name: "api", Repo: "app", NeedsPostgres: true,
			Migrate: `psql "$DATABASE_URL" -v ON_ERROR_STOP=1 -c 'create table t (v text)'`,
			Seed:    []string{`psql "$DATABASE_URL" -v ON_ERROR_STOP=1 -c "insert into t values ('seeded-row')"`},
			Start:   "exec socat TCP-LISTEN:3000,bind=127.0.0.1,reuseaddr,fork EXEC:./serve.sh",
			Port:    3000, Health: "/health",
			Env: map[string]string{"DATABASE_URL": "{postgres.url}", "APP_TOKEN": "{random}"},
		}},
		PreviewEnv: map[string]string{"API_URL": "{services.api.url}"},
		Dirs:       map[string]string{"app": wt},
		Postgres:   pg, TimeoutSec: 90, Random: strings.Repeat("ab", 32),
	}
	if err := dexec.WriteDDStack(handoff, stack); err != nil {
		t.Fatal(err)
	}
	host := (&hostAdapter{}).host(p, "bjob_ps1", "", []git.WorktreeBinding{{Repo: "app", WorktreeAbs: wt}}, nil)
	task := aa.Task{Role: aa.Implementer, Handoff: handoff, Worktrees: []string{wt}, ReadOnly: host.ReadOnly,
		DenyRead: host.DenyRead(), DenyWrite: host.DenyWrite, UnixSockets: true}
	body, err := claude.Settings(task)
	if err != nil {
		t.Fatal(err)
	}
	args := append(bwrapArgs(t, body), "--unshare-user", "--uid", "1000", "--gid", "1000",
		"--chdir", handoff, "--setenv", "TMPDIR", filepath.Join(handoff, "tmp"),
		"sh", "dd-stack", "run", "--", "sh", "-c", `curl -fsS "$API_URL/health"`)
	start := time.Now()
	out, err := exec.Command("bwrap", args...).Output()
	t.Logf("dd-stack in %s:\n%s", time.Since(start).Round(time.Millisecond), out)
	if err != nil || !strings.Contains(string(out), "DD-STACK RUN\nseeded-row\nDD-STACK DOWN\n") {
		for _, f := range []string{"postgres-initdb.log", "postgres.log", "api-migrate.log", "api-seed-1.log", "api-start.log"} {
			b, _ := os.ReadFile(filepath.Join(handoff, ".dd-stack", f))
			t.Logf("%s:\n%s", f, b)
		}
		t.Fatalf("dd-stack: %v", err)
	}
	if strings.Contains(string(out), stack.Random) {
		t.Fatal("dd-stack printed the {random} value")
	}
	for _, s := range []string{"TCP-LISTEN:3000", filepath.Join(handoff, "tmp")} {
		if running(s) {
			t.Fatalf("%q left running", s)
		}
	}
	if left, _ := os.ReadDir(filepath.Join(handoff, "tmp")); len(left) != 0 {
		t.Fatalf("$TMPDIR keeps %d entries", len(left))
	}

	// A failing seed is named, with its log, and still tears down.
	stack.Services[0].Seed = []string{`psql "$DATABASE_URL" -v ON_ERROR_STOP=1 -c 'insert into nope values (1)'`}
	if err := dexec.WriteDDStack(handoff, stack); err != nil {
		t.Fatal(err)
	}
	out, err = exec.Command("bwrap", args...).Output()
	t.Logf("failing seed:\n%s", out)
	if ee, ok := err.(*exec.ExitError); !ok || ee.ExitCode() != 125 ||
		!strings.Contains(string(out), "DD-STACK FAIL api seed-1 log=.dd-stack/api-seed-1.log\n") ||
		!strings.Contains(string(out), `relation "nope" does not exist`) {
		t.Fatalf("failing seed: %v", err)
	}
	if running(filepath.Join(handoff, "tmp")) {
		t.Fatal("Postgres left running after a failure")
	}
}
