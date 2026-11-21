package worker

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	dexec "github.com/more-than-code/defect-drainer-worker/exec"
	"github.com/more-than-code/defect-drainer-worker/workerproto"
)

func TestCanonicalRepoURL(t *testing.T) {
	ok := map[string]string{
		"https://github.com/More-Than-Code/App.git": "https://github.com/More-Than-Code/App",
		"https://GitHub.com/a/b/":                   "https://github.com/a/b",
		"git@github.com:a/b.git":                    "ssh://git@github.com/a/b",
		"ssh://git@github.com/a/b":                  "ssh://git@github.com/a/b",
		"ssh://github.com:22/a/b":                   "ssh://git@github.com:22/a/b",
	}
	for in, want := range ok {
		got, err := CanonicalRepoURL(in)
		if err != nil || got != want {
			t.Errorf("%q → %q, %v (want %q)", in, got, err, want)
		}
	}
	for _, bad := range []string{
		"ext::sh -c touch% /tmp/pwned", "file:///etc", "/srv/repo", "-uhttps://x/y", "http://github.com/a/b",
		"https://user:pw@github.com/a/b", "https://tok@github.com/a/b", "ssh://root@github.com/a/b",
		"https://github.com/a/../b", "https://github.com/a/b?x=1", "https://github.com/a/-b",
		"https://github.com", "git@github.com:", "https://github.com/a/*", "",
	} {
		if got, err := CanonicalRepoURL(bad); err == nil {
			t.Errorf("%q accepted as %q", bad, got)
		}
	}
}

func TestRepoMatching(t *testing.T) {
	p := Policy{Repos: []string{"https://github.com/example-org/*", "git@gitlab.example.com:team/app"}}
	if r := p.check(); r != "" && !strings.HasPrefix(r, "orgs") {
		t.Fatal(r)
	}
	p.Orgs = []string{"org_a"}
	p.Adapters = []string{"claude"}
	p.MinSandbox, p.MaxTurns, p.MaxWallSec, p.MaxJobs = "strict", 80, 2700, 1
	if r := p.check(); r != "" {
		t.Fatal(r)
	}
	for url, want := range map[string]bool{
		"https://github.com/example-org/app.git": true,
		"git@github.com:example-org/app":         true, // host and path match; scheme is not compared
		"https://github.com/example-org/a/b":     false,
		"https://github.com/other/app":           false,
		"https://evil.com/example-org/app":       false,
		"https://gitlab.example.com/team/app":    true,
		"https://gitlab.example.com/team/app2":   false,
		"ext::https://github.com/example-org/x":  false,
	} {
		if got := p.RepoAllowed(url); got != want {
			t.Errorf("%s: %v, want %v", url, got, want)
		}
	}
}

func TestLoadPolicyDefaultsAndRefusals(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "policy.toml")
	writeFile(t, path, "enabled = true\norgs = [\"org_a\"]\n", 0o644)
	p := LoadPolicy(path, false, rootUID)
	if !p.Valid || p.AllowShip || p.AllowVerifyCommands || p.MaxJobs != 1 || p.MinSandbox != "workspace" ||
		p.MaxTurns != DefaultMaxTurns || p.MaxWallSec != DefaultMaxWallSec || strings.Join(p.Adapters, ",") != "claude,grok,codex,antigravity" {
		t.Fatalf("defaults %+v", p)
	}
	if w := p.Wire(); w.AllowShip || len(w.Repos) != 0 || w.Repos == nil {
		t.Fatalf("wire %+v", w)
	}
	// Not root-owned (the real owner of a temp file), group-writable, a bad
	// value or an unknown key: invalid, and the wire policy says so.
	if p := LoadPolicy(path, false, nil); p.Valid || p.Reason != "not_root" {
		if os.Geteuid() != 0 {
			t.Fatalf("non-root file %+v", p)
		}
	}
	for body, reason := range map[string]string{
		"enabled = true\norgs = []\n":                                   "orgs_empty",
		"enabled = true\norgs = [\"org_a\"]\nadapters = [\"vim\"]\n":    "bad_adapters",
		"enabled = true\norgs = [\"org_a\"]\nmin_sandbox = \"open\"\n":  "bad_min_sandbox",
		"enabled = true\norgs = [\"org_a\"]\nmax_jobs = 0\n":            "bad_max_jobs",
		"enabled = true\norgs = [\"org_a\"]\nrepos = [\"file:///x\"]\n": "bad_repos",
		"enabled = true\norgs = [\"org_a\"]\nrunners = [\"claude\"]\n":  "unknown_key",
	} {
		writeFile(t, path, body, 0o644)
		if p := LoadPolicy(path, false, rootUID); p.Valid || p.Reason != reason || p.Wire().Valid || p.Wire().Enabled {
			t.Errorf("%q: %+v", body, p)
		}
	}
	writeFile(t, path, "enabled = true\norgs = [\"org_a\"]\n", 0o664)
	if p := LoadPolicy(path, false, rootUID); p.Valid || p.Reason != "group_writable" {
		t.Fatalf("group writable %+v", p)
	}
	if p := LoadPolicy(filepath.Join(dir, "none.toml"), false, rootUID); p.Valid || p.Reason != "missing" {
		t.Fatalf("missing %+v", p)
	}
}

func TestEffectiveSandbox(t *testing.T) {
	for _, c := range [][3]string{
		{"workspace", "workspace", "workspace"},
		{"workspace", "strict", "strict"},
		{"strict", "workspace", "strict"},
		{"", "workspace", "strict"},
		{"workspace", "read-only", "read-only"},
		{"bogus", "workspace", "strict"},
	} {
		if got := EffectiveSandbox(c[0], c[1]); got != c[2] {
			t.Errorf("%v → %s", c, got)
		}
	}
}

func TestSecretsFileAndDenySet(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "secrets.env")
	writeFile(t, path, "QA_PASSWORD=pw-123456\nANTHROPIC_API_KEY=sk-not-real-000\nOTHER_PW=\"x y z 123\"\nbad-name=1\n", 0o600)
	s, err := LoadSecrets(path, rootUID)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(s.Names(), ",") != "ANTHROPIC_API_KEY,OTHER_PW,QA_PASSWORD" {
		t.Fatalf("names %v", s.Names())
	}
	got := s.DenySet([]string{"ANTHROPIC_API_KEY"}, []string{"APP_SECRET", "QA_PASSWORD", "not valid"})
	if strings.Join(got, ",") != "APP_SECRET,OTHER_PW,QA_PASSWORD" {
		t.Fatalf("deny %v", got)
	}
	writeFile(t, path, "A=1\n", 0o640)
	if _, err := LoadSecrets(path, rootUID); err == nil {
		t.Fatal("group-readable secrets.env accepted")
	}
	writeFile(t, path, "A=1\n", 0o600)
	if _, err := LoadSecrets(path, func(os.FileInfo) (uint32, bool) { return 4242, true }); err == nil && os.Geteuid() != 4242 {
		t.Fatal("secrets.env owned by another user accepted")
	}
	if s, err := LoadSecrets(filepath.Join(dir, "none"), rootUID); err != nil || len(s.Names()) != 0 {
		t.Fatal("missing secrets.env is not an empty set")
	}
}

// T5 and §12.6: a decline for each §6.5 rule, and the narrowing of the
// sandbox, the caps and verify. Nothing runs for a declined offer.
func TestPolicyDeclines(t *testing.T) {
	h := newHarness(t, harnessOpts{secrets: "QA_PASSWORD=pw-12345678\n"})
	cases := []struct {
		name   string
		mutate func(*dexec.JobSpec)
		want   string
	}{
		{"repo outside policy", func(s *dexec.JobSpec) { s.Worktrees[0].RepoURL = "https://git.test/other/app" }, workerproto.DeclinePolicyDenied},
		{"ext:: repo", func(s *dexec.JobSpec) { s.Worktrees[0].RepoURL = "ext::sh -c id" }, workerproto.DeclinePolicyDenied},
		{"repo entry with userinfo", func(s *dexec.JobSpec) { s.Project.RepoEntries[0].URL = "https://u:p@git.test/acme/app" }, workerproto.DeclinePolicyDenied},
		{"adapter not allowed", func(s *dexec.JobSpec) { s.Adapter = "grok" }, workerproto.DeclinePolicyDenied},
		{"model with a dash", func(s *dexec.JobSpec) { s.Model = "-rf" }, workerproto.DeclineBadSpec},
		{"local base", func(s *dexec.JobSpec) { s.Worktrees[0].BaseSource = "local" }, workerproto.DeclineBadSpec},
		{"local repo entry", func(s *dexec.JobSpec) { s.Project.RepoEntries[0].BaseSource = "local" }, workerproto.DeclineBadSpec},
		{"traversal repo", func(s *dexec.JobSpec) { s.Worktrees[0].Repo = "../etc" }, workerproto.DeclineBadSpec},
		{"absolute evidence", func(s *dexec.JobSpec) {
			s.Fix.Evidence = []dexec.EvidenceRef{{Path: "/etc/passwd", SHA256: strings.Repeat("a", 64)}}
		}, workerproto.DeclineBadSpec},
		{"NUL evidence", func(s *dexec.JobSpec) {
			s.Fix.Evidence = []dexec.EvidenceRef{{Path: "evidence/a\x00b", SHA256: strings.Repeat("a", 64)}}
		}, workerproto.DeclineBadSpec},
		{"evidence without hash", func(s *dexec.JobSpec) { s.Fix.Evidence = []dexec.EvidenceRef{{Path: "evidence/a.png"}} }, workerproto.DeclineBadSpec},
		{"bad defect id", func(s *dexec.JobSpec) { s.HarvestIDs = []string{"../DEF"} }, workerproto.DeclineBadSpec},
		{"secret missing", func(s *dexec.JobSpec) {
			s.Kind = dexec.KindTestRun
			s.Worktrees[0].Branch = ""
			s.TestRun = &dexec.TestRunSpec{RunID: "TRUN-1", SecretEnv: "NOT_ON_HOST"}
		}, workerproto.DeclineSecretMissing},
		{"diffBase option injection", func(s *dexec.JobSpec) {
			s.DiffBase = map[string]string{"app": "--output=/tmp/pwned"}
		}, workerproto.DeclineBadSpec},
		{"branch not the job branch", func(s *dexec.JobSpec) { s.Worktrees[0].Branch = "main" }, workerproto.DeclineBadSpec},
		{"test run with a branch", func(s *dexec.JobSpec) {
			s.Kind = dexec.KindTestRun
			s.TestRun = &dexec.TestRunSpec{RunID: "TRUN-2"}
		}, workerproto.DeclineBadSpec},
	}
	for i, c := range cases {
		spec := fixSpec("bjob_decl" + string(rune('a'+i)))
		c.mutate(&spec)
		h.offerJob(spec, nil)
		eventually(t, 10*time.Second, c.name, func() bool { return h.fs.declineFor(spec.JobID) != "" })
		if got := h.fs.declineFor(spec.JobID); got != c.want {
			t.Errorf("%s: declined %s, want %s", c.name, got, c.want)
		}
		if _, err := os.Stat(h.paths.JobDir(spec.JobID)); !os.IsNotExist(err) {
			t.Errorf("%s: a job dir was created", c.name)
		}
	}
	if b, _ := os.ReadFile(h.argv); len(b) != 0 {
		t.Fatalf("an agent ran for a declined offer: %s", b)
	}
	// A spec_url other than the job's own spec path: bad_spec, the token is
	// never sent there.
	spec := fixSpec("bjob_specurl")
	h.fs.addJob(spec, nil)
	h.fs.send(workerproto.Offer{Header: workerproto.NewHeader(workerproto.TypeOffer), JobID: spec.JobID, LeaseID: "ls_x",
		LeaseUntil: time.Now().Add(time.Minute).UTC().Format(time.RFC3339), Mode: "fresh",
		SpecURL: "https://elsewhere.invalid/worker/v1/jobs/bjob_specurl/spec", JobToken: "dd_wjt_x"})
	eventually(t, 5*time.Second, "spec_url decline", func() bool { return h.fs.declineFor(spec.JobID) == workerproto.DeclineBadSpec })
}

func TestPolicyInvalidOrDisabledDeclinesEverything(t *testing.T) {
	for name, body := range map[string]string{
		"disabled":   strings.Replace(defaultPolicy, "enabled = true", "enabled = false", 1),
		"unknownkey": defaultPolicy + "runners = [\"claude\"]\n",
		"otherorg":   strings.Replace(defaultPolicy, "org_test1", "org_other", 1),
	} {
		h := newHarness(t, harnessOpts{policy: body})
		spec := fixSpec("bjob_pol_" + name)
		h.offerJob(spec, nil)
		eventually(t, 10*time.Second, name, func() bool { return h.fs.declineFor(spec.JobID) != "" })
		if got := h.fs.declineFor(spec.JobID); got != workerproto.DeclinePolicyDenied {
			t.Errorf("%s: %s", name, got)
		}
		h.stop()
	}
}

// §12.6 narrowing: a looser sandbox, higher caps and verify without
// allow_verify_commands are narrowed, said in the log, and the job runs.
func TestPolicyNarrowing(t *testing.T) {
	pol := strings.Replace(defaultPolicy, "allow_verify_commands = true", "allow_verify_commands = false", 1) +
		"min_sandbox = \"strict\"\nmax_turns = 50\nmax_wall_sec = 600\n"
	h := newHarness(t, harnessOpts{policy: pol})
	spec := fixSpec("bjob_narrow")
	spec.Project.Sandbox = "workspace"
	spec.Project.MaxTurns = 500
	spec.Caps.AgentMaxWallSec = 99999
	spec.Project.VerifyCommands = []dexec.VerifyCommand{{Repo: "app", Command: "touch /tmp/should-not-run-" + spec.JobID}}
	h.offerJob(spec, nil)
	d := h.waitDone(spec.JobID, 60*time.Second)
	log := joinLines(h.fs.jobEvents(spec.JobID))
	for _, want := range []string{"sandbox narrowed to strict", "agent_max_turns clamped to 50", "verify skipped by host policy", "grok_sandbox=strict", "agent_max_turns=50"} {
		if !strings.Contains(log, want) {
			t.Errorf("log lacks %q:\n%s", want, log)
		}
	}
	if d.State != "completed" {
		t.Fatalf("done %+v", d)
	}
	var ran *bool
	for _, e := range h.fs.jobEvents(spec.JobID) {
		if e.Verification != nil {
			r := e.Verification.Ran
			ran = &r
		}
	}
	if ran == nil || *ran {
		t.Fatalf("verification event ran=%v", ran)
	}
	if _, err := os.Stat("/tmp/should-not-run-" + spec.JobID); err == nil {
		t.Fatal("a verify command ran")
	}
	meta, _ := readMeta(h.paths.JobMeta(spec.JobID))
	if meta.Spec.Caps.AgentMaxWallSec != 600 || meta.Spec.Project.Sandbox != "strict" {
		t.Fatalf("stored spec %+v", meta.Spec.Caps)
	}
}

// Adapter not ready: adapter_unavailable, and busy at max_jobs.
func TestAdapterUnavailableAndBusy(t *testing.T) {
	h := newHarness(t, harnessOpts{agent: "trap '' INT\nexec sleep 30\n"})
	h.offerJob(fixSpec("bjob_busy1"), nil)
	eventually(t, 20*time.Second, "first job running", func() bool { b, _ := os.ReadFile(h.argv); return len(b) > 0 })
	h.offerJob(fixSpec("bjob_busy2"), nil)
	eventually(t, 5*time.Second, "busy decline", func() bool { return h.fs.declineFor("bjob_busy2") == workerproto.DeclineBusy })
	h.stop()

	h2 := newHarness(t, harnessOpts{})
	writeFile(t, h2.agent, "#!/bin/sh\nexit 1\n", 0o755) // --version now fails
	h2.offerJob(fixSpec("bjob_notready"), nil)
	eventually(t, 10*time.Second, "decline", func() bool { return h2.fs.declineFor("bjob_notready") != "" })
	if got := h2.fs.declineFor("bjob_notready"); got != workerproto.DeclineAdapterUnavailable {
		t.Fatalf("%s", got)
	}
	// A status with the re-checked readiness was sent before the decline.
	sts := h2.fs.frames(workerproto.TypeStatus)
	if len(sts) == 0 || !strings.Contains(string(sts[len(sts)-1].raw), `"reason":"missing"`) {
		t.Fatalf("no status after readiness re-check: %d", len(sts))
	}
}

func TestPathsUnder(t *testing.T) {
	base := "/state/jobs/bjob_1/evidence"
	if p, err := Under(base, "evidence/DEF-1/a.png"); err != nil || p != filepath.Join(base, "evidence/DEF-1/a.png") {
		t.Fatal(p, err)
	}
	for _, bad := range []string{"../x", "/abs", "a/../../x", "a\\b", "a\x00b", "", ".", "a//b"} {
		if _, err := Under(base, bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
	defer func() {
		if recover() == nil {
			t.Fatal("JobDir accepted an unchecked id")
		}
	}()
	Paths{StateDir: "/s"}.JobDir("../x")
}
