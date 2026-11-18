package worker

import (
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	dexec "github.com/more-than-code/defect-drainer-worker/exec"
	"github.com/more-than-code/defect-drainer-worker/internal/botagent/redact"
	"github.com/more-than-code/defect-drainer-worker/internal/botagent/spool"
	"github.com/more-than-code/defect-drainer-worker/workerproto"
)

// T6 and §5.2: an accept that draws lease_lost within the grace starts
// nothing.
func TestLeaseLostDuringAcceptGrace(t *testing.T) {
	h := newHarness(t, harnessOpts{timing: Timing{AcceptGrace: 2 * time.Second}})
	spec := fixSpec("bjob_grace")
	h.offerJob(spec, nil)
	eventually(t, 10*time.Second, "accept", func() bool { return h.fs.accepted(spec.JobID) })
	h.fs.send(workerproto.Error{Header: workerproto.NewHeader(workerproto.TypeError), Code: workerproto.CodeLeaseLost, JobID: spec.JobID, LeaseID: "ls_" + spec.JobID})
	time.Sleep(2500 * time.Millisecond)
	if _, err := os.Stat(h.paths.JobDir(spec.JobID)); !os.IsNotExist(err) {
		t.Fatal("job started after lease_lost")
	}
	if b, _ := os.ReadFile(h.argv); len(b) != 0 {
		t.Fatal("agent ran")
	}
	if len(h.fs.jobEvents(spec.JobID)) != 0 {
		t.Fatal("progress sent for a withdrawn offer")
	}
}

// T8 and §12.1: progress (done included) lost in a socket drop is replayed
// after the reconnect and the server's re-attach: every seq applied once,
// none lost, done last.
func TestSpoolReplayAfterDrop(t *testing.T) {
	h := newHarness(t, harnessOpts{})
	h.fs.mu.Lock()
	h.fs.dropProgress = true
	h.fs.mu.Unlock()
	spec := fixSpec("bjob_replay")
	fj := h.offerJob(spec, nil)
	// Wait until the worker has spooled the done frame.
	eventually(t, 60*time.Second, "done spooled", func() bool {
		sp, err := spool.Open(h.paths.SpoolRoot(spec.JobID))
		return err == nil && spoolHasDone(sp)
	})
	sent := len(h.fs.frames(workerproto.TypeProgress))
	h.fs.mu.Lock()
	h.fs.dropProgress = false
	h.fs.mu.Unlock()
	h.fs.closeCurrent(1000)
	eventually(t, 10*time.Second, "done applied after reconnect", func() bool { return h.fs.done(spec.JobID) != nil })
	hellos := h.fs.frames(workerproto.TypeHello)
	last := mustDecode[workerproto.Hello](h.fs, hellos[len(hellos)-1].raw)
	if len(last.BusyJobIDs) != 1 || last.BusyJobIDs[0] != spec.JobID {
		t.Fatalf("hello busy_job_ids %v", last.BusyJobIDs)
	}
	h.fs.mu.Lock()
	frames, dup, lastSeq := fj.frames, fj.duplicates, fj.lastSeq
	h.fs.mu.Unlock()
	meta, _ := readMeta(h.paths.JobMeta(spec.JobID))
	if int64(frames) != lastSeq || dup != 0 || lastSeq != meta.NextSeq-1 || sent == 0 {
		t.Fatalf("applied %d frames, last seq %d (worker wrote %d), %d duplicates, %d sent before the drop", frames, lastSeq, meta.NextSeq-1, dup, sent)
	}
	ev := h.fs.jobEvents(spec.JobID)
	if ev[len(ev)-1].Kind != dexec.EventDone || ev[len(ev)-1].Done.State != "completed" {
		t.Fatalf("last event %+v", ev[len(ev)-1])
	}
	eventually(t, 5*time.Second, "spool empty after ack", func() bool { return len(h.w.busyIDs()) == 0 })
}

// §6.1: a lost frame draws seq_gap; the worker resends from expected and
// the server applies everything once.
func TestSeqGapResend(t *testing.T) {
	// Frames are batched by time, so the job could end in two frames and
	// the lost seq 2 would carry done, with no later frame to reveal the
	// gap. The agent waits twice: for seq 1 to land, then (after its
	// stream output makes a frame) for seq 2 to be lost. done is then ≥ 3.
	gates := t.TempDir()
	seq1, seq2 := filepath.Join(gates, "seq1"), filepath.Join(gates, "seq2")
	wait := func(p string) string { return "while [ ! -f '" + p + "' ]; do sleep 0.02; done\n" }
	agent := wait(seq1) + fixArtifacts(testDefect) + "cat '" + claudeFixture(t, "c1-basic") + "'\n" + wait(seq2) + "exit 0\n"
	h := newHarness(t, harnessOpts{agent: agent})
	h.fs.mu.Lock()
	h.fs.dropSeqOnce[2] = true
	h.fs.mu.Unlock()
	spec := fixSpec("bjob_gap")
	fj := h.offerJob(spec, nil)
	eventually(t, 30*time.Second, "seq 1 applied", func() bool { return h.fs.lastSeq(spec.JobID) >= 1 })
	writeFile(t, seq1, "", 0o644)
	eventually(t, 30*time.Second, "seq 2 lost", func() bool {
		h.fs.mu.Lock()
		defer h.fs.mu.Unlock()
		return len(h.fs.dropSeqOnce) == 0
	})
	writeFile(t, seq2, "", 0o644)
	h.waitDone(spec.JobID, 60*time.Second)
	h.fs.mu.Lock()
	defer h.fs.mu.Unlock()
	if fj.blocked || int64(fj.frames) != fj.lastSeq || fj.lastSeq < 3 {
		t.Fatalf("frames %d last %d blocked %v", fj.frames, fj.lastSeq, fj.blocked)
	}
	if len(h.fs.dropSeqOnce) != 0 {
		t.Fatal("seq 2 was never sent")
	}
}

// §7: a blob the server lost draws blob_missing; the worker re-uploads it
// and resends, and the event is applied with the blob present.
func TestBlobMissingReupload(t *testing.T) {
	h := newHarness(t, harnessOpts{})
	h.fs.mu.Lock()
	h.fs.dropPuts = 1
	h.fs.mu.Unlock()
	spec := fixSpec("bjob_blob")
	fj := h.offerJob(spec, nil)
	d := h.waitDone(spec.JobID, 60*time.Second)
	if d.State != "completed" {
		t.Fatalf("done %+v", d)
	}
	h.fs.mu.Lock()
	defer h.fs.mu.Unlock()
	if len(fj.blobs) != 1 {
		t.Fatalf("blobs %d", len(fj.blobs))
	}
	for sha := range fj.blobs {
		if fj.appliedAt[sha] < fj.putOrder[sha] {
			t.Fatal("event applied before its blob")
		}
	}
}

// T9 and §5.3: lease_lost stops the child, drops only that job's spool and
// stored results, and keeps its worktrees.
func TestLeaseLostDropsOnlyThatJob(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "agent.pid")
	h := newHarness(t, harnessOpts{
		policy:  strings.Replace(defaultPolicy, "allow_ship = true", "allow_ship = true\nmax_jobs = 2", 1),
		agent:   "echo $$ > '" + pidFile + "'\ntrap '' INT\nexec sleep 60\n",
		noStart: true,
	})
	// A long heartbeat: the ping loop's own busy check cannot be what sends
	// the status this test waits for.
	h.fs.heartbeat = 30
	h.start(Timing{})
	h.fs.mu.Lock()
	h.fs.withholdAcks = true
	h.fs.mu.Unlock()
	h.offerJob(fixSpec("bjob_ll1"), nil)
	eventually(t, 30*time.Second, "agent running", func() bool { _, err := os.Stat(pidFile); return err == nil })
	pid := readPid(t, pidFile)
	// A second, ended job with a spool and a stored control result.
	other := "bjob_ll2"
	sp, _ := spool.Open(h.paths.SpoolRoot(other))
	_ = sp.Put(spoolID(1), []byte(`{"v":1,"type":"progress","job_id":"bjob_ll2","lease_id":"ls_o","seq":1,"events":[]}`), time.Now())
	writeFile(t, filepath.Join(h.paths.JobControls(other), "rq_1.json"), "{}", 0o600)
	writeFile(t, filepath.Join(h.paths.JobControls("bjob_ll1"), "rq_2.json"), "{}", 0o600)

	statusesBefore := len(h.fs.frames(workerproto.TypeStatus))
	h.fs.send(workerproto.Error{Header: workerproto.NewHeader(workerproto.TypeError), Code: workerproto.CodeLeaseLost, JobID: "bjob_ll1", LeaseID: "ls_bjob_ll1"})
	eventually(t, 5*time.Second, "agent stopped", func() bool { return !processAlive(pid) })
	eventually(t, 5*time.Second, "spool dropped", func() bool {
		items, _ := os.ReadDir(filepath.Join(h.paths.SpoolRoot("bjob_ll1"), "spool"))
		return len(items) == 0
	})
	if _, err := os.Stat(h.paths.JobControls("bjob_ll1")); !os.IsNotExist(err) {
		t.Fatal("stored results of the lost job kept")
	}
	if items, _ := sp.List(); len(items) != 1 {
		t.Fatal("another job's spool was touched")
	}
	if _, err := os.Stat(filepath.Join(h.paths.JobControls(other), "rq_1.json")); err != nil {
		t.Fatal("another job's stored result was touched")
	}
	if _, err := os.Stat(filepath.Join(h.paths.WorktreeRoot("bjob_ll1"), "app", "README.md")); err != nil {
		t.Fatal("worktree removed")
	}
	for _, id := range h.w.busyIDs() {
		if id == "bjob_ll1" {
			t.Fatal("lost job still busy")
		}
	}
	meta, _ := readMeta(h.paths.JobMeta("bjob_ll1"))
	if !meta.LeaseLost || meta.State != stateEnded {
		t.Fatalf("meta %+v", meta)
	}
	// A status without the lost job follows at once (W2b's scheduler counts
	// a job busy until the next status).
	eventually(t, 3*time.Second, "status after lease_lost", func() bool {
		for _, f := range h.fs.frames(workerproto.TypeStatus)[statusesBefore:] {
			st := mustDecode[workerproto.Status](h.fs, f.raw)
			if !contains(st.BusyJobIDs, "bjob_ll1") && contains(st.BusyJobIDs, "bjob_ll2") {
				return true
			}
		}
		return false
	})
}

// T10: cancel ends the events with done{cancelled} within 5 s, and the
// child is gone even though it ignores SIGINT (SIGKILL after the grace).
func TestCancelWithinDeadline(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "agent.pid")
	h := newHarness(t, harnessOpts{
		agent:  "echo $$ > '" + pidFile + "'\ntrap '' INT\nexec sleep 60\n",
		timing: Timing{CancelGrace: workerproto.CancelGrace},
	})
	spec := fixSpec("bjob_cancel")
	h.offerJob(spec, nil)
	eventually(t, 30*time.Second, "agent running", func() bool { _, err := os.Stat(pidFile); return err == nil })
	pid := readPid(t, pidFile)
	start := time.Now()
	h.fs.send(workerproto.Control{Header: workerproto.NewHeader(workerproto.TypeControl), JobID: spec.JobID, LeaseID: "ls_" + spec.JobID,
		RequestID: "rq_cancel", Control: dexec.Control{Kind: dexec.ControlCancel, JobID: spec.JobID}})
	d := h.waitDone(spec.JobID, workerproto.CancelDeadline)
	if d.State != "cancelled" {
		t.Fatalf("done %+v", d)
	}
	if el := time.Since(start); el > workerproto.CancelDeadline {
		t.Fatalf("done after %s", el)
	}
	eventually(t, workerproto.CancelDeadline, "child killed", func() bool { return !processAlive(pid) })
	if res, n := h.fs.controlResult("rq_cancel"); n != 1 || !res.OK {
		t.Fatalf("control_result %+v (%d)", res, n)
	}
}

// T15 and §6.4: on start, a job that was running with no done in its spool
// gets done{failed, worker_restarted}, is listed busy, and is replayed; one
// whose spool already holds a done is replayed as is.
func TestWorkerRestartRule(t *testing.T) {
	h := newHarness(t, harnessOpts{noStart: true})
	mk := func(id string, withDone bool) {
		meta := jobMeta{JobID: id, LeaseID: "ls_" + id, Adapter: "claude", State: stateRunning, NextSeq: 3, Spec: fixSpec(id)}
		b, _ := json.Marshal(meta)
		writeFile(t, h.paths.JobMeta(id), string(b), 0o600)
		sp, err := spool.Open(h.paths.SpoolRoot(id))
		if err != nil {
			t.Fatal(err)
		}
		logEv := dexec.Event{Kind: dexec.EventLog, JobID: id, Lines: []dexec.LogLine{{Text: "before restart", Level: "info", Source: "x"}}}
		second := logEv
		if withDone {
			second = dexec.Event{Kind: dexec.EventDone, JobID: id, Done: &dexec.Done{State: "completed"}}
		}
		for i, ev := range []dexec.Event{logEv, second} {
			seq := int64(i + 1)
			f, _ := json.Marshal(workerproto.Progress{Header: workerproto.NewHeader(workerproto.TypeProgress), JobID: id, LeaseID: "ls_" + id, Seq: seq, Events: []dexec.Event{ev}})
			if err := sp.Put(spoolID(seq), f, time.Now()); err != nil {
				t.Fatal(err)
			}
		}
		h.fs.addJob(fixSpec(id), nil).leaseID = "ls_" + id
	}
	mk("bjob_rs_lost", false)
	mk("bjob_rs_done", true)
	h.start(Timing{})
	hello := h.fs.hellos[0]
	if strings.Join(hello.BusyJobIDs, ",") != "bjob_rs_done,bjob_rs_lost" {
		t.Fatalf("busy %v", hello.BusyJobIDs)
	}
	deadline := time.Now().Add(10 * time.Second)
	for h.fs.done("bjob_rs_lost") == nil || h.fs.done("bjob_rs_done") == nil {
		if time.Now().After(deadline) {
			var frames []string
			for _, f := range h.fs.recv {
				frames = append(frames, f.typ+" "+string(f.raw[:min(len(f.raw), 160)]))
			}
			t.Fatalf("not replayed.\nworker log:\n%s\nframes:\n%s", h.log.String(), strings.Join(frames, "\n"))
		}
		time.Sleep(20 * time.Millisecond)
	}
	if d := h.fs.done("bjob_rs_lost"); d.State != "failed" || !strings.HasPrefix(d.Line, "worker_restarted: ") {
		t.Fatalf("lost: %+v", d)
	}
	if d := h.fs.done("bjob_rs_done"); d.State != "completed" {
		t.Fatalf("done: %+v", d)
	}
	h.fs.mu.Lock()
	seq := h.fs.jobs["bjob_rs_lost"].lastSeq
	h.fs.mu.Unlock()
	if seq != 3 {
		t.Fatalf("restart done went out as seq %d, want 3", seq)
	}
}

// T16 and criterion 10: after a test-account run, no secret value is in
// the spool, agent.log, the events or the worker log; the child got the
// password only as DD_TEST_ACCOUNT_PASSWORD, never under its own name.
func TestSecretsNeverLeave(t *testing.T) {
	const pw = "Sup3r-Secret-Pa55w0rd"
	const other = "0ther-Host-Secret-77"
	envDump := filepath.Join(t.TempDir(), "child.env")
	agent := "echo \"typing $DD_TEST_ACCOUNT_PASSWORD into the form\"\n" +
		"env > '" + envDump + "'\n" +
		"mkdir -p results\nprintf 'used %s\\n' \"$DD_TEST_ACCOUNT_PASSWORD\" > NOTES.md\n" +
		"printf '%s\\n' \"$DD_TEST_ACCOUNT_PASSWORD\" > RUN.md\n" +
		"cat '" + claudeFixture(t, "c1-basic") + "'\nexit 0\n"
	h := newHarness(t, harnessOpts{secrets: "QA_PASSWORD=" + pw + "\nOTHER_APP_PW=" + other + "\n", agent: agent})
	h.fs.mu.Lock()
	h.fs.withholdAcks = true // keep the spool on disk for the grep
	h.fs.mu.Unlock()
	spec := fixSpec("bjob_secret")
	spec.Kind = dexec.KindTestRun
	spec.Fix = nil
	spec.DefectIDs, spec.HarvestIDs = nil, nil
	spec.Worktrees[0].Branch = ""
	spec.TestRun = &dexec.TestRunSpec{RunID: "TRUN-1", Loaded: true, SecretEnv: "QA_PASSWORD",
		Run: dexec.TestRun{ID: "TRUN-1", AppID: "app1", Title: "login", Env: map[string]any{"account": "env:QA_PASSWORD"},
			Steps: []dexec.RunStep{{Key: "s1", Position: 1, Action: "log in", Expected: "dashboard"}}}}
	h.offerJob(spec, nil)
	h.waitDone(spec.JobID, 60*time.Second)

	child, err := os.ReadFile(envDump)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(child), "DD_TEST_ACCOUNT_PASSWORD="+pw) {
		t.Fatal("the child did not get the password")
	}
	if strings.Contains(string(child), "QA_PASSWORD=") || strings.Contains(string(child), other) {
		t.Fatalf("the child saw a secrets.env name or another secret")
	}
	if os.Getenv("QA_PASSWORD") != "" {
		t.Fatal("the secret stayed in the worker's env after the job")
	}
	var haystack []string
	add := func(what, s string) { haystack = append(haystack, what+"\n"+s) }
	evs, _ := json.Marshal(h.fs.jobEvents(spec.JobID))
	add("events", string(evs))
	for _, f := range h.fs.frames(workerproto.TypeProgress) {
		add("frame", string(f.raw))
	}
	add("worker log", h.log.String())
	_ = filepath.Walk(filepath.Join(h.paths.SpoolRoot(spec.JobID), "spool"), func(p string, fi os.FileInfo, err error) error {
		if err == nil && !fi.IsDir() {
			b, _ := os.ReadFile(p)
			add("spool "+p, string(b))
		}
		return nil
	})
	if b, err := os.ReadFile(filepath.Join(h.paths.Handoff(spec.JobID), "agent.log")); err == nil {
		add("agent.log", string(b))
	} else {
		t.Fatalf("no agent.log: %v", err)
	}
	if len(haystack) < 4 {
		t.Fatal("nothing to grep")
	}
	for _, s := range haystack {
		for _, secret := range []string{pw, other, redactB64(pw)} {
			if strings.Contains(s, secret) {
				t.Fatalf("secret found in %s", strings.SplitN(s, "\n", 2)[0])
			}
		}
	}
	if !strings.Contains(string(evs), "typing [redacted] into the form") && !strings.Contains(string(evs), "typing [REDACTED] into the form") {
		t.Logf("events: %s", evs)
		t.Fatal("the agent's output line did not come through redacted")
	}
}

func redactB64(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }

// T18 and §6.2: strings over 256 KiB are truncated with a log line naming
// them; an event stays ≤ 768 KiB; secret values are redacted everywhere.
func TestEventCaps(t *testing.T) {
	r := redact.NewExact("hunter2-secret")
	big := strings.Repeat("x", 300<<10)
	e := dexec.Event{Kind: dexec.EventLog, JobID: "bjob_1", Lines: []dexec.LogLine{{Text: big, Level: "info", Source: "a"}, {Text: "pw hunter2-secret", Level: "info"}}}
	got, paths := capEvent(e, r)
	if len(got.Lines[0].Text) != workerproto.MaxStringFieldBytes || len(paths) != 1 || paths[0] != "lines[0].text" {
		t.Fatalf("len %d paths %v", len(got.Lines[0].Text), paths)
	}
	if got.Lines[1].Text != "pw [REDACTED]" {
		t.Fatalf("not redacted: %q", got.Lines[1].Text)
	}
	// Four 250 KiB fields: each is under the field cap but the event is not.
	var lines []dexec.LogLine
	for i := 0; i < 4; i++ {
		lines = append(lines, dexec.LogLine{Text: strings.Repeat("y", 250<<10), Level: "info"})
	}
	got, _ = capEvent(dexec.Event{Kind: dexec.EventLog, JobID: "bjob_1", Lines: lines}, r)
	if b, _ := json.Marshal(got); len(b) > workerproto.MaxEventBytes {
		t.Fatalf("event %d bytes", len(b))
	}
	// A truncated multibyte string stays valid UTF-8.
	if s := cutUTF8(strings.Repeat("é", 10), 5); s != "éé" {
		t.Fatalf("%q", s)
	}
}

// Frames split to stay ≤ 1 MiB however many large events are queued.
func TestFrameSplit(t *testing.T) {
	h := newHarness(t, harnessOpts{noStart: true})
	w, err := New(Options{Paths: h.paths, Log: h.log, OwnerUID: rootUID, Timing: fastTiming(Timing{})})
	if err != nil {
		t.Fatal(err)
	}
	j, err := w.openJob(jobMeta{JobID: "bjob_split", LeaseID: "ls_s", State: stateRunning})
	if err != nil {
		t.Fatal(err)
	}
	j.mu.Lock()
	for i := 0; i < 10; i++ {
		j.pending = append(j.pending, dexec.Event{Kind: dexec.EventLog, JobID: "bjob_split", Lines: []dexec.LogLine{{Text: strings.Repeat("z", 200<<10)}}})
	}
	j.flushLocked()
	j.mu.Unlock()
	items, _ := j.sp.List()
	if len(items) < 2 {
		t.Fatalf("%d frames", len(items))
	}
	n := 0
	for i, it := range items {
		if len(it.Frame) > workerproto.MaxFrameBytes {
			t.Fatalf("frame %d is %d bytes", i, len(it.Frame))
		}
		var f workerproto.Progress
		_ = json.Unmarshal(it.Frame, &f)
		if f.Seq != int64(i+1) {
			t.Fatalf("seq %d at %d", f.Seq, i)
		}
		n += len(f.Events)
	}
	if n != 10 {
		t.Fatalf("%d events spooled", n)
	}
}
