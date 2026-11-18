package worker

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/more-than-code/defect-drainer-worker/internal/botagent/config"
	"github.com/more-than-code/defect-drainer-worker/workerproto"
)

// T1: enrolment writes the credential atomically at 0600 and sends the
// machine id hash; a refused join token writes nothing.
func TestEnroll(t *testing.T) {
	fs := newFakeServer(t)
	tmp := t.TempDir()
	p := Paths{ConfigDir: filepath.Join(tmp, "etc"), StateDir: filepath.Join(tmp, "state")}
	mid := filepath.Join(tmp, "machine-id")
	writeFile(t, mid, "abc123machine\n", 0o644)
	join := "dd_wjn_" + "jointokenjointoken01"
	fs.joinTokens[join] = true
	fs.enrollCred = testCred

	// A bad token: close 1008, nothing written.
	_, err := Enroll(context.Background(), EnrollOptions{Paths: p, Server: fs.URL(), JoinToken: "dd_wjn_" + "wrongtokenwrongtoken", InsecureDev: true, MachineIDFile: mid})
	if err == nil {
		t.Fatal("bad join token enrolled")
	}
	if _, err := os.Stat(p.Env()); !os.IsNotExist(err) {
		t.Fatal("credential written after a refused token")
	}
	// http without --insecure-dev is refused before dialling.
	if _, err := Enroll(context.Background(), EnrollOptions{Paths: p, Server: fs.URL(), JoinToken: join, MachineIDFile: mid}); err == nil {
		t.Fatal("http origin accepted without insecure-dev")
	}

	cfg, err := Enroll(context.Background(), EnrollOptions{Paths: p, Server: fs.URL(), JoinToken: join, InsecureDev: true, MachineIDFile: mid, Hostname: "Pi One"})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.WorkerID != "wrk_test1" || cfg.OrgID != "org_test1" {
		t.Fatalf("%+v", cfg)
	}
	fi, err := os.Stat(p.Env())
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("env %v %v", fi, err)
	}
	loaded, err := config.Load(p.Env())
	if err != nil || loaded.Credential != testCred {
		t.Fatalf("load %v", err)
	}
	enrolls := fs.frames(workerproto.TypeEnroll)
	if len(enrolls) != 1 {
		t.Fatalf("%d enroll frames", len(enrolls))
	}
	e := mustDecode[workerproto.Enroll](fs, enrolls[0].raw)
	if e.MachineIDHash != hashHex([]byte("abc123machine")) || e.Hostname != "pi-one" {
		t.Fatalf("enroll %+v", e)
	}
	if strings.Contains(string(enrolls[0].raw), join) {
		t.Fatal("join token in a frame")
	}
	// A second init refuses to overwrite the credential.
	fs.joinTokens[join] = true
	if _, err := Enroll(context.Background(), EnrollOptions{Paths: p, Server: fs.URL(), JoinToken: join, InsecureDev: true, MachineIDFile: mid}); err == nil {
		t.Fatal("re-enrolled over an existing credential")
	}
}

// T2 and criterion 7: hello carries readiness checked under the exact child
// env. A secrets.env name present in the worker's own env is stripped
// before the probe (ready), while a host variable outside secrets.env is
// what a child would see (the fake CLI then reports signed out).
func TestHelloAndReadinessUnderChildEnv(t *testing.T) {
	t.Setenv("HOST_SECRET_PW", "s3cret-in-process-env")
	t.Setenv("DEFECT_DRAINER_SOMETHING", "x")
	h := newHarness(t, harnessOpts{secrets: "HOST_SECRET_PW=s3cret-in-process-env\n"})
	hello := h.fs.hellos[0]
	if hello.WorkerID != "wrk_test1" || hello.Protocol != 1 || hello.MaxJobs != 1 || len(hello.Adapters) != 4 {
		t.Fatalf("hello %+v", hello)
	}
	if !hello.Policy.Valid || !hello.Policy.Enabled || hello.Policy.AllowShip != true || hello.Policy.Adapters[0] != "claude" {
		t.Fatalf("policy %+v", hello.Policy)
	}
	byName := map[string]workerproto.AdapterStatus{}
	for _, a := range hello.Adapters {
		byName[a.Name] = a
	}
	if c := byName["claude"]; !c.Ready || !c.MinOK || c.Version != "2.1.292" || c.CredentialMode != "subscription" {
		t.Fatalf("claude %+v", c)
	}
	if c := byName["codex"]; c.Ready || c.Reason != workerproto.ReasonMissing {
		t.Fatalf("codex %+v", c)
	}
	if c := byName["grok"]; c.Ready || c.Reason != workerproto.ReasonMissing {
		t.Fatalf("grok %+v", c)
	}
	h.stop()

	// The control: a secret-looking variable the host owner did NOT list in
	// secrets.env reaches the child, so the probe sees it.
	t.Setenv("HOST_SECRET_UNLISTED", "x")
	h2 := newHarness(t, harnessOpts{secrets: "HOST_SECRET_PW=s3cret-in-process-env\n"})
	for _, a := range h2.fs.hellos[0].Adapters {
		if a.Name == "claude" && (a.Ready || a.Reason != workerproto.ReasonSignedOut) {
			t.Fatalf("probe did not run under the child env: %+v", a)
		}
	}
}

// T3: the worker refuses 3xx on the upgrade and on the spec GET.
func TestRedirectsRefused(t *testing.T) {
	h := newHarness(t, harnessOpts{})
	h.fs.mu.Lock()
	h.fs.redirect = true
	h.fs.mu.Unlock()
	// The spec GET is redirected: the offer is declined, never followed.
	spec := fixSpec("bjob_redir")
	h.fs.addJob(spec, nil)
	h.fs.offer(spec.JobID, "ls_r", "dd_wjt_tok_redir", "")
	eventually(t, 10*time.Second, "decline", func() bool { return h.fs.declineFor("bjob_redir") != "" })
	if r := h.fs.declineFor("bjob_redir"); r != workerproto.DeclineInternal {
		t.Fatalf("reason %s", r)
	}
	// A reconnect gets a 302 and never follows it.
	h.fs.mu.Lock()
	ups := h.fs.upgrades
	h.fs.mu.Unlock()
	h.fs.closeCurrent(1000)
	time.Sleep(800 * time.Millisecond)
	h.fs.mu.Lock()
	defer h.fs.mu.Unlock()
	if h.fs.upgrades != ups || len(h.fs.hellos) != 1 {
		t.Fatalf("followed a redirect: upgrades %d→%d hellos %d", ups, h.fs.upgrades, len(h.fs.hellos))
	}
}

// T4 and criterion 11: unknown keys, unknown frame types and capabilities
// the worker does not know are tolerated; a malformed known frame gets
// bad_frame; unknown_type is answered once per type per socket.
func TestLenientDecoding(t *testing.T) {
	h := newHarness(t, harnessOpts{})
	h.fs.sendRaw(`{"v":1,"type":"sse_log","job_id":"bjob_x","future":{"a":1}}`)
	h.fs.sendRaw(`{"v":1,"type":"sse_log"}`)
	h.fs.sendRaw(`{"v":1,"type":"ack","job_id":"../x","seq":1}`)
	h.fs.sendRaw(`{"v":1,"type":"ack","job_id":"bjob_x","seq":"one"}`)
	eventually(t, 5*time.Second, "error frames", func() bool { return len(h.fs.frames(workerproto.TypeError)) >= 3 })
	var unknown, bad int
	for _, f := range h.fs.frames(workerproto.TypeError) {
		e := mustDecode[workerproto.Error](h.fs, f.raw)
		switch e.Code {
		case workerproto.CodeUnknownType:
			unknown++
		case workerproto.CodeBadFrame:
			bad++
		}
	}
	if unknown != 1 || bad != 2 {
		t.Fatalf("unknown_type %d bad_frame %d", unknown, bad)
	}
	// An offer with keys from a future server, and a spec with unknown
	// keys, still run.
	spec := fixSpec("bjob_lenient")
	h.fs.addJob(spec, nil)
	h.fs.mu.Lock()
	h.fs.jobs[spec.JobID].leaseID = "ls_l"
	h.fs.jobTokens["dd_wjt_tok_lenient"] = spec.JobID
	h.fs.mu.Unlock()
	h.fs.sendRaw(`{"v":1,"type":"offer","job_id":"bjob_lenient","lease_id":"ls_l","lease_until":"2030-01-01T00:00:00Z","adapter":"","model":"","mode":"fresh",` +
		`"spec_url":"/worker/v1/jobs/bjob_lenient/spec","job_token":"dd_wjt_tok_lenient","priority":7,"sse":{"x":true}}`)
	if d := h.waitDone("bjob_lenient", 60*time.Second); d.State != "completed" {
		t.Fatalf("done %+v", d)
	}
}

// T12 and §12.4: a rotation whose ack is lost, then a re-sent rotation: the
// worker keeps authenticating, env.prev falls back, and env.prev is
// deleted after a sign-in with env.
func TestRotationWithLostAck(t *testing.T) {
	h := newHarness(t, harnessOpts{})
	h.fs.mu.Lock()
	h.fs.ignoreRotAck = true
	h.fs.creds[testCredB] = true
	h.fs.mu.Unlock()
	h.fs.send(workerproto.Rotated{Header: workerproto.NewHeader(workerproto.TypeRotated), Credential: testCredB})
	eventually(t, 5*time.Second, "rotate_ack", func() bool { return len(h.fs.frames(workerproto.TypeRotateAck)) == 1 })
	cur, _ := config.Load(h.paths.Env())
	prev, _ := config.Load(h.paths.EnvPrev())
	if cur.Credential != testCredB || prev.Credential != testCred {
		t.Fatal("env/env.prev not rotated")
	}
	if fi, err := os.Stat(h.paths.Env()); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("env mode %v %v", fi, err)
	}
	// The ack was lost: the server re-sends with a new pending credential C.
	h.fs.mu.Lock()
	h.fs.creds[testCredC] = true
	h.fs.mu.Unlock()
	h.fs.send(workerproto.Rotated{Header: workerproto.NewHeader(workerproto.TypeRotated), Credential: testCredC})
	eventually(t, 5*time.Second, "second rotate_ack", func() bool { return len(h.fs.frames(workerproto.TypeRotateAck)) == 2 })
	// The newest (C) is refused, e.g. its mint never committed: the worker
	// falls back to env.prev (B).
	h.fs.mu.Lock()
	delete(h.fs.creds, testCredC)
	h.fs.mu.Unlock()
	hellos := len(h.fs.frames(workerproto.TypeHello))
	h.fs.closeCurrent(1000)
	eventually(t, 5*time.Second, "redial via env.prev", func() bool { return len(h.fs.frames(workerproto.TypeHello)) > hellos && h.fs.connected() })
	if _, err := os.Stat(h.paths.EnvPrev()); err != nil {
		t.Fatal("env.prev deleted after a fallback sign-in")
	}
	// C works again (the hard cutoff revokes A): a sign-in with env deletes
	// env.prev.
	h.fs.mu.Lock()
	h.fs.creds[testCredC] = true
	delete(h.fs.creds, testCred)
	h.fs.mu.Unlock()
	hellos = len(h.fs.frames(workerproto.TypeHello))
	h.fs.closeCurrent(1000)
	eventually(t, 5*time.Second, "redial via env", func() bool { return len(h.fs.frames(workerproto.TypeHello)) > hellos && h.fs.connected() })
	eventually(t, 2*time.Second, "env.prev deleted", func() bool { _, err := os.Stat(h.paths.EnvPrev()); return os.IsNotExist(err) })
	if strings.Contains(h.log.String(), testCredB) || strings.Contains(h.log.String(), testCredC) {
		t.Fatal("a credential reached the worker log")
	}
}

// T13 and §4.4: a 4001 revoke stops the children (SIGINT, then SIGKILL),
// deletes the credential files and the spool, keeps the worktrees, and Run
// exits non-zero without redialling.
func TestRevoke4001(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "agent.pid")
	h := newHarness(t, harnessOpts{agent: "echo $$ > '" + pidFile + "'\ntrap '' INT\nexec sleep 60\n"})
	h.fs.mu.Lock()
	h.fs.withholdAcks = true
	h.fs.mu.Unlock()
	h.offerJob(fixSpec("bjob_rev"), nil)
	eventually(t, 30*time.Second, "agent running", func() bool { _, err := os.Stat(pidFile); return err == nil })
	pid := readPid(t, pidFile)
	writeFile(t, h.paths.EnvPrev(), "", 0o600)
	upgrades := h.fs.upgrades
	h.fs.closeCurrent(workerproto.CloseRevoked)
	err := h.runErr(15 * time.Second)
	if !errors.Is(err, ErrRevoked) {
		t.Fatalf("Run = %v", err)
	}
	if processAlive(pid) {
		t.Fatal("agent child still alive after revoke")
	}
	for _, p := range []string{h.paths.Env(), h.paths.EnvPrev(), filepath.Join(h.paths.SpoolRoot("bjob_rev"), "spool"), h.paths.ControlsDir()} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Fatalf("%s kept after revoke", p)
		}
	}
	if _, err := os.Stat(filepath.Join(h.paths.WorktreeRoot("bjob_rev"), "app", "README.md")); err != nil {
		t.Fatal("worktree removed by revoke")
	}
	h.fs.mu.Lock()
	defer h.fs.mu.Unlock()
	if h.fs.upgrades != upgrades {
		t.Fatal("redialled after revoke")
	}
}

// §4.4 by 401: a dial refused with env and env.prev is a revocation.
func TestRevoke401(t *testing.T) {
	h := newHarness(t, harnessOpts{})
	h.fs.mu.Lock()
	delete(h.fs.creds, testCred)
	h.fs.mu.Unlock()
	h.fs.closeCurrent(1000)
	if err := h.runErr(10 * time.Second); !errors.Is(err, ErrRevoked) {
		t.Fatalf("Run = %v", err)
	}
	if _, err := os.Stat(h.paths.Env()); !os.IsNotExist(err) {
		t.Fatal("credential kept after 401")
	}
}

// T14 and §4.5: a replaced socket waits the backoff before redialling;
// three in the window end Run.
func TestReplacedBackoff(t *testing.T) {
	h := newHarness(t, harnessOpts{timing: Timing{ReplacedBackoff: 400 * time.Millisecond}})
	for i := 0; i < 3; i++ {
		before := len(h.fs.frames(workerproto.TypeHello))
		closedAt := time.Now()
		h.fs.closeCurrent(workerproto.CloseReplaced)
		if i == 2 {
			break
		}
		eventually(t, 5*time.Second, "redial", func() bool { return len(h.fs.frames(workerproto.TypeHello)) > before })
		hellos := h.fs.frames(workerproto.TypeHello)
		if gap := hellos[len(hellos)-1].at.Sub(closedAt); gap < 400*time.Millisecond {
			t.Fatalf("redialled after %s, want ≥ 400ms", gap)
		}
	}
	if err := h.runErr(5 * time.Second); !errors.Is(err, ErrReplaced) {
		t.Fatalf("Run = %v", err)
	}
	if _, err := os.Stat(h.paths.Env()); err != nil {
		t.Fatal("credential deleted on replace")
	}
	if d := (Timing{}).withDefaults().ReplacedBackoff; d < 60*time.Second {
		t.Fatalf("default replaced backoff %s", d)
	}
}

// 4002 and 4003 end Run and keep the credential.
func TestFatalCloseCodes(t *testing.T) {
	for code, want := range map[int]error{4002: ErrProtocol, 4003: ErrOrgMismatch} {
		h := newHarness(t, harnessOpts{noStart: true})
		h.fs.helloClose = closeCode(code)
		h.launch(Timing{})
		if err := h.runErr(5 * time.Second); !errors.Is(err, want) {
			t.Fatalf("%d: Run = %v", code, err)
		}
		if _, err := os.Stat(h.paths.Env()); err != nil {
			t.Fatalf("%d: credential deleted", code)
		}
	}
}

// Ping names held leases; status follows each readiness re-check.
func TestPingAndStatus(t *testing.T) {
	h := newHarness(t, harnessOpts{timing: Timing{Readiness: 300 * time.Millisecond}})
	eventually(t, 5*time.Second, "ping", func() bool { return len(h.fs.frames(workerproto.TypePing)) > 0 })
	// Make claude not ready: the next re-check sends a status.
	writeFile(t, h.agent, "#!/bin/sh\nexit 1\n", 0o755)
	eventually(t, 5*time.Second, "status after readiness change", func() bool {
		for _, f := range h.fs.frames(workerproto.TypeStatus) {
			var s workerproto.Status
			_ = json.Unmarshal(f.raw, &s)
			for _, a := range s.Adapters {
				if a.Name == "claude" && !a.Ready {
					return true
				}
			}
		}
		return false
	})
	snap, err := ReadSnapshot(h.paths)
	if err != nil || !snap.Connected || snap.WorkerID != "wrk_test1" {
		t.Fatalf("snapshot %+v %v", snap, err)
	}
	b, _ := os.ReadFile(snapshotPath(h.paths))
	if strings.Contains(string(b), testCred) {
		t.Fatal("credential in status.json")
	}
}
