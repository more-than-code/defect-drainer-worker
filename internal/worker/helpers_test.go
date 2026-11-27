package worker

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	dexec "github.com/more-than-code/defect-drainer-worker/exec"
	"github.com/more-than-code/defect-drainer-worker/git"
	"github.com/more-than-code/defect-drainer-worker/internal/botagent/config"
	"github.com/more-than-code/defect-drainer-worker/internal/cgroup"
	"github.com/more-than-code/defect-drainer-worker/internal/sandbox"
	"github.com/more-than-code/defect-drainer-worker/workerproto"
)

const (
	testCred   = "dd_wrk_" + "testcredentialAAAAAAAAAAAAAAAAAAAA"
	testCredB  = "dd_wrk_" + "testcredentialBBBBBBBBBBBBBBBBBBBB"
	testCredC  = "dd_wrk_" + "testcredentialCCCCCCCCCCCCCCCCCCCC"
	testRepo   = "https://git.test/acme/app"
	testDefect = "DEF-20261115-w2a-0001"
)

// pngBytes is a minimal PNG signature plus padding: enough for the sniffer.
var pngBytes = append([]byte("\x89PNG\r\n\x1a\n"), bytes.Repeat([]byte{0}, 64)...)

func rootUID(os.FileInfo) (uint32, bool) { return 0, true }

// syncBuf is the worker log, safe for concurrent writes.
type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// harness is one worker against one fake server.
type harness struct {
	t      *testing.T
	fs     *fakeServer
	paths  Paths
	log    *syncBuf
	w      *Worker
	cancel context.CancelFunc
	done   chan error
	agent  string // fake claude path
	argv   string // fake agent argv log
	tmp    string
	// verify is the verify-sandbox lookup the worker gets; bwrapLog is the
	// fake bwrap's argv (\x1f between args, \x1e after each run).
	verify   func(context.Context) (sandbox.Launcher, error)
	bwrapLog string
	cgroups  func() (*cgroup.Manager, error)
}

type harnessOpts struct {
	policy  string // policy.toml body; "" is the default test policy
	secrets string // secrets.env body
	agent   string // fake agent body after the version/auth cases; "" writes the fix artifacts
	noStart bool
	timing  Timing
	// verify replaces the verify sandbox lookup (nil: the fake bwrap,
	// ready).
	verify func(context.Context) (sandbox.Launcher, error)
	// cgroups is Options.Cgroups (nil: no per-job cgroups).
	cgroups func() (*cgroup.Manager, error)
}

const defaultPolicy = `enabled = true
orgs = ["org_test1"]
adapters = ["claude"]
repos = ["https://git.test/acme/*"]
allow_verify_commands = true
allow_ship = true
`

func newHarness(t *testing.T, o harnessOpts) *harness {
	t.Helper()
	tmp := t.TempDir()
	h := &harness{t: t, fs: newFakeServer(t), log: &syncBuf{}, tmp: tmp,
		paths: Paths{ConfigDir: filepath.Join(tmp, "etc"), StateDir: filepath.Join(tmp, "state")}}
	h.fs.creds[testCred] = true
	if err := os.MkdirAll(h.paths.ConfigDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := config.Write(h.paths.Env(), config.Config{ServerURL: h.fs.URL(), WorkerID: "wrk_test1", OrgID: "org_test1", Credential: testCred, InsecureDev: true}); err != nil {
		t.Fatal(err)
	}
	pol := o.policy
	if pol == "" {
		pol = defaultPolicy
	}
	writeFile(t, h.paths.Policy(), pol, 0o600)
	if o.secrets != "" {
		writeFile(t, h.paths.Secrets(), o.secrets, 0o600)
	}
	// The worker's own log, printed only when the test fails.
	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("worker log:\n%s", h.log.String())
		}
	})
	setupGit(t, tmp)
	h.argv = filepath.Join(tmp, "argv.log")
	h.agent = fakeAgent(t, tmp, h.argv, o.agent)
	h.verify = o.verify
	h.cgroups = o.cgroups
	if h.verify == nil {
		bw := fakeBwrap(t, tmp)
		h.bwrapLog = filepath.Join(tmp, "bwrap.log")
		h.verify = func(context.Context) (sandbox.Launcher, error) {
			return sandbox.Launcher{Bwrap: bw, Socat: "/bin/true", Seccomp: []byte{0}}, nil
		}
	}
	if !o.noStart {
		h.start(o.timing)
	}
	return h
}

func fastTiming(t Timing) Timing {
	def := func(d *time.Duration, v time.Duration) {
		if *d == 0 {
			*d = v
		}
	}
	def(&t.AcceptGrace, 150*time.Millisecond)
	def(&t.CancelGrace, 500*time.Millisecond)
	def(&t.BatchInterval, 30*time.Millisecond)
	def(&t.Readiness, time.Hour)
	def(&t.ReplacedBackoff, 200*time.Millisecond)
	def(&t.LeaseWait, 300*time.Millisecond)
	def(&t.BackoffMax, 100*time.Millisecond)
	def(&t.HelloTimeout, 3*time.Second)
	return t
}

func (h *harness) start(timing Timing) {
	h.t.Helper()
	before := len(h.fs.frames(workerproto.TypeHello))
	h.launch(timing)
	// A new hello and its socket: after a restart the previous worker's
	// socket may still look current for a moment.
	eventually(h.t, 10*time.Second, "worker connected", func() bool {
		return len(h.fs.frames(workerproto.TypeHello)) > before && h.fs.connected() && h.w.sockUp()
	})
}

// launch starts Run without waiting for the connection.
func (h *harness) launch(timing Timing) {
	h.t.Helper()
	w, err := New(Options{
		Paths: h.paths,
		// grok and agy point nowhere: a test never runs a real agent CLI.
		Adapters: AdapterConfig{ClaudeBin: h.agent, ClaudeConfigDir: filepath.Join(h.tmp, "claude-cfg"),
			GrokBin: filepath.Join(h.tmp, "no-such", "grok"), AgyBin: filepath.Join(h.tmp, "no-such", "agy")},
		Timing: fastTiming(timing), Log: h.log, OwnerUID: rootUID,
		VerifySandbox:     h.verify,
		Cgroups:           h.cgroups,
		SelfUpdateRootDir: filepath.Join(h.tmp, "su-root"),
	})
	if err != nil {
		h.t.Fatal(err)
	}
	h.w = w
	ctx, cancel := context.WithCancel(context.Background())
	h.cancel = cancel
	h.done = make(chan error, 1)
	go func() { h.done <- w.Run(ctx) }()
	h.t.Cleanup(h.stop)
}

// stop ends Run and waits for it.
func (h *harness) stop() {
	if h.cancel == nil {
		return
	}
	h.cancel()
	select {
	case <-h.done:
	case <-time.After(10 * time.Second):
		h.t.Error("worker did not stop")
	}
	h.cancel = nil
	// Let children the test started finish their kill.
	time.Sleep(50 * time.Millisecond)
}

// runErr waits for Run to end on its own.
func (h *harness) runErr(timeout time.Duration) error {
	select {
	case err := <-h.done:
		h.cancel = nil
		return err
	case <-time.After(timeout):
		h.t.Fatal("Run did not end")
		return nil
	}
}

func writeFile(t *testing.T, path, body string, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
}

// setupGit makes a bare origin with one commit on main and points
// https://git.test/acme/ at it through GIT_CONFIG_GLOBAL, so policy sees an
// https URL and nothing touches the network. git identity comes from env.
func setupGit(t *testing.T, tmp string) {
	t.Helper()
	bare := filepath.Join(tmp, "origin", "app.git")
	src := filepath.Join(tmp, "src")
	run := func(dir string, args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	if err := os.MkdirAll(src, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(bare), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_AUTHOR_NAME", "dd test")
	t.Setenv("GIT_AUTHOR_EMAIL", "dd@test.invalid")
	t.Setenv("GIT_COMMITTER_NAME", "dd test")
	t.Setenv("GIT_COMMITTER_EMAIL", "dd@test.invalid")
	run(tmp, "init", "-q", "--bare", "-b", "main", bare)
	run(src, "init", "-q", "-b", "main")
	writeFile(t, filepath.Join(src, "README.md"), "hello\n", 0o644)
	run(src, "add", ".")
	run(src, "commit", "-q", "-m", "init")
	run(src, "push", "-q", bare, "main")
	gc := filepath.Join(tmp, "gitconfig")
	writeFile(t, gc, "[url \""+bare+"\"]\n\tinsteadOf = "+testRepo+"\n[init]\n\tdefaultBranch = main\n", 0o644)
	t.Setenv("GIT_CONFIG_GLOBAL", gc)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
}

// fakeAgent writes a fake claude CLI: --version answers 2.1.292, auth
// status reports logged in only when the child env has no secrets.env
// name and no DD variable; anything else runs body (cwd is the handoff).
func fakeAgent(t *testing.T, dir, argvLog, body string) string {
	t.Helper()
	if body == "" {
		body = fixArtifacts(testDefect) + "cat '" + claudeFixture(t, "c1-basic") + "'\nexit 0\n"
	}
	script := "#!/bin/sh\n" +
		"case \"$1\" in --version) echo '2.1.292 (Claude Code)'; exit 0;; esac\n" +
		"if [ \"$1\" = auth ]; then\n" +
		"  if env | grep -q -e '^HOST_SECRET_' -e '^DEFECT_DRAINER_' -e '^ANTHROPIC_API_KEY='; then echo '{\"loggedIn\":false}'; else echo '{\"loggedIn\":true}'; fi\n" +
		"  exit 0\nfi\n" +
		"printf '%s\\n' \"$*\" >> '" + argvLog + "'\n" + body
	p := filepath.Join(dir, "bin", "claude")
	writeFile(t, p, script, 0o755)
	return p
}

func fixArtifacts(defID string) string {
	return "mkdir -p fix-evidence/" + defID + " fix-notes\n" +
		"printf '\\211PNG\\r\\n\\032\\n0000000000000000' > fix-evidence/" + defID + "/fix-01.png\n" +
		"printf 'fixed it' > fix-notes/" + defID + ".md\n" +
		"printf 'plan' > NOTES.md\n"
}

func claudeFixture(t *testing.T, name string) string {
	t.Helper()
	p, err := filepath.Abs(filepath.Join("..", "..", "agentadapter", "claude", "testdata", name+".jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// fixSpec is a remote-shaped batch fix spec (§8): no server-local paths.
func fixSpec(jobID string) dexec.JobSpec {
	now := time.Now().Add(-time.Minute).UTC().Format(time.RFC3339Nano)
	return dexec.JobSpec{
		JobID: jobID, Kind: dexec.KindBatchFix, ProjectID: "app1", Label: "BATCH-20261115-w2a", BatchID: "BATCH-20261115-w2a",
		Mode: dexec.ModeFresh, DefectIDs: []string{testDefect}, HarvestIDs: []string{testDefect},
		CreatedAt: now, RunStartedAt: now,
		Worktrees: []git.WorktreeBinding{{Repo: "app", Branch: "dd/" + jobID + "/app", RepoURL: testRepo, BaseSource: "origin", BaseBranch: "main"}},
		Project:   dexec.ProjectSettings{Sandbox: "strict", RepoEntries: []dexec.RepoEntry{{Name: "app", URL: testRepo, BaseSource: "origin", BaseBranch: "main"}}},
		Fix: &dexec.FixSpec{
			Batch:   dexec.Batch{ID: "BATCH-20261115-w2a", ProjectID: "app1", Title: "t", DefectIDs: []string{testDefect}},
			Defects: []dexec.Defect{{ID: testDefect, ProjectID: "app1", Title: "Button broken", Body: "## Acceptance\n- [ ] works\n"}},
		},
		Caps: dexec.Caps{AgentMaxIters: 2, AgentMaxWallSec: 120, ReviewOff: true},
	}
}

// offerJob registers spec and offers it.
func (h *harness) offerJob(spec dexec.JobSpec, evidence map[string][]byte) *fakeJob {
	j := h.fs.addJob(spec, evidence)
	h.fs.offer(spec.JobID, "ls_"+spec.JobID, "dd_wjt_tok_"+spec.JobID, "")
	return j
}

func (h *harness) waitDone(jobID string, timeout time.Duration) *dexec.Done {
	h.t.Helper()
	var d *dexec.Done
	eventually(h.t, timeout, "done for "+jobID, func() bool { d = h.fs.done(jobID); return d != nil })
	return d
}

// joinLines is every log line text the server applied for a job.
func joinLines(events []dexec.Event) string {
	var b strings.Builder
	for _, e := range events {
		for _, l := range e.Lines {
			b.WriteString(l.Text)
			b.WriteByte('\n')
		}
		if e.Done != nil {
			b.WriteString(e.Done.Line)
			b.WriteByte('\n')
		}
	}
	return b.String()
}

func readPid(t *testing.T, path string) int {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var pid int
	for _, c := range strings.TrimSpace(string(b)) {
		pid = pid*10 + int(c-'0')
	}
	return pid
}

// processAlive reports whether pid exists and is not a zombie.
func processAlive(pid int) bool {
	out, err := exec.Command("ps", "-o", "stat=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		return false
	}
	s := strings.TrimSpace(string(out))
	return s != "" && !strings.HasPrefix(s, "Z")
}

func closeCode(n int) websocket.StatusCode { return websocket.StatusCode(n) }

// fakeBwrap stands in for bwrap where there is none (the Mac): it applies
// only --chdir and the built environment (--clearenv, --setenv), says
// "ready" on fd 3 as the real wrapper does, and runs the command, the last
// argument, without any sandbox. Its argv goes to
// bwrap.log. The real layout is tested with real bwrap in
// packaging/test/sandbox_test.sh.
func fakeBwrap(t *testing.T, dir string) string {
	t.Helper()
	path := filepath.Join(dir, "fake-bwrap")
	log := filepath.Join(dir, "bwrap.log")
	writeFile(t, path, `#!/usr/bin/perl
open(my $l, ">>", "`+log+`"); print $l join("\x1f", @ARGV), "\x1e"; close($l);
my $dir = "/"; my %env;
while (@ARGV) {
	my $a = shift @ARGV;
	last if $a eq "--";
	if ($a eq "--chdir") { $dir = shift @ARGV }
	elsif ($a eq "--setenv") { my $k = shift @ARGV; $env{$k} = shift @ARGV }
	elsif ($a =~ /^--(ro-bind|bind|dev-bind)$/) { shift @ARGV; shift @ARGV }
	elsif ($a =~ /^--(tmpfs|proc|dev|cap-drop|size|seccomp)$/) { shift @ARGV }
}
# The real wrapper says it reached the command on fd 3.
if (open(my $r, ">&=", 3)) { print $r "ready\n"; close($r); }
%ENV = %env;
chdir $dir or die "chdir $dir: $!";
exec "/bin/sh", "-c", $ARGV[-1];
`, 0o755)
	return path
}
