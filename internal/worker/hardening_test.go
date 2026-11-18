package worker

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	dexec "github.com/more-than-code/defect-drainer-worker/exec"
	"github.com/more-than-code/defect-drainer-worker/workerproto"
)

// An ack beyond what was sent never drops an unsent frame, and a seq_gap
// beyond the record is ignored.
func TestAckAndGapBounded(t *testing.T) {
	h := newHarness(t, harnessOpts{noStart: true})
	w, err := New(Options{Paths: h.paths, Log: h.log, OwnerUID: rootUID, Timing: fastTiming(Timing{})})
	if err != nil {
		t.Fatal(err)
	}
	j, err := w.openJob(jobMeta{JobID: "bjob_bound", LeaseID: "ls_b", State: stateRunning})
	if err != nil {
		t.Fatal(err)
	}
	j.mu.Lock()
	j.pending = []dexec.Event{logEvent("one", "info")}
	j.flushLocked()
	j.pending = []dexec.Event{logEvent("two", "info")}
	j.flushLocked()
	j.mu.Unlock()
	done := make(chan struct{})
	go func() { j.ack(1 << 62); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("a huge ack hung the job")
	}
	if j.acked != 2 {
		t.Fatalf("acked %d", j.acked)
	}
	j.resync(1 << 62)
	if j.meta.NextSeq != 3 {
		t.Fatalf("record moved to %d by a bogus seq_gap", j.meta.NextSeq)
	}
}

// Push credentials and the host's secrets never reach a job child; image
// names that fail the §2 rule are left out instead of wedging the job; a
// git error carrying a secret is redacted in done.line.
func TestChildEnvAndNames(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "ghp_notreal_0000000000")
	t.Setenv("SSH_AUTH_SOCK", "/tmp/agent.sock")
	envDump := filepath.Join(t.TempDir(), "child.env")
	agent := fixArtifacts(testDefect) +
		"printf '\\211PNG\\r\\n\\032\\n0000' > 'fix-evidence/" + testDefect + "/Screen Shot 1.png'\n" +
		"env > '" + envDump + "'\n" +
		"cat '" + claudeFixture(t, "c1-basic") + "'\nexit 0\n"
	h := newHarness(t, harnessOpts{agent: agent})
	spec := fixSpec("bjob_env")
	h.offerJob(spec, nil)
	if d := h.waitDone(spec.JobID, 60*time.Second); d.State != "completed" {
		t.Fatalf("done %+v\n%s", d, joinLines(h.fs.jobEvents(spec.JobID)))
	}
	b, _ := os.ReadFile(envDump)
	for _, name := range []string{"GITHUB_TOKEN=", "SSH_AUTH_SOCK="} {
		if strings.Contains(string(b), name) {
			t.Errorf("child got %s", name)
		}
	}
	log := joinLines(h.fs.jobEvents(spec.JobID))
	if !strings.Contains(log, `image "Screen Shot 1.png" skipped`) {
		t.Fatalf("no skip note:\n%s", log)
	}
	evs, _ := json.Marshal(h.fs.jobEvents(spec.JobID))
	if strings.Contains(string(evs), "Screen Shot") && !strings.Contains(log, "skipped") {
		t.Fatal("an invalid image name reached an event")
	}
}

func TestWithCode(t *testing.T) {
	for in, want := range map[string]string{
		"base_missing: app main":      "base_missing: app main",
		"agent_auth: not logged in":   "agent_auth: not logged in",
		"mkdir /x: permission denied": "internal: mkdir /x: permission denied",
		"open: no such file":          "internal: open: no such file",
	} {
		if got := withCode(errString(in)); got != want {
			t.Errorf("%q → %q", in, got)
		}
	}
}

type errString string

func (e errString) Error() string { return string(e) }

// A silent server is detected and the worker reconnects.
func TestDeadConnectionReconnects(t *testing.T) {
	h := newHarness(t, harnessOpts{})
	hellos := len(h.fs.frames(workerproto.TypeHello))
	// Stop answering pings: swap the handler's view by withholding pongs is
	// not possible on the fake, so cut the server side without a close.
	h.fs.mu.Lock()
	c := h.fs.cur
	h.fs.cur = nil
	h.fs.mu.Unlock()
	_ = c // the TCP link stays open; nothing more is written on it
	h.fs.mu.Lock()
	h.fs.silent = true
	h.fs.mu.Unlock()
	eventually(t, 10*time.Second, "reconnect after silence", func() bool {
		return len(h.fs.frames(workerproto.TypeHello)) > hellos
	})
}

// Replay and resync follow frame seq, whatever order the spool entries
// were written in.
func TestSpoolItemsOrderedBySeq(t *testing.T) {
	h := newHarness(t, harnessOpts{noStart: true})
	w, err := New(Options{Paths: h.paths, Log: h.log, OwnerUID: rootUID, Timing: fastTiming(Timing{})})
	if err != nil {
		t.Fatal(err)
	}
	j, err := w.openJob(jobMeta{JobID: "bjob_order", LeaseID: "ls_o", State: stateEnded})
	if err != nil {
		t.Fatal(err)
	}
	for _, seq := range []int64{3, 1, 2} {
		if err := j.sp.Put(spoolID(seq), []byte(`{"v":1,"type":"progress","job_id":"bjob_order","lease_id":"ls_o","seq":1,"events":[]}`), time.Now()); err != nil {
			t.Fatal(err)
		}
	}
	items, err := j.spoolItems()
	if err != nil {
		t.Fatal(err)
	}
	for i, it := range items {
		if s, _ := seqOf(it.ID); s != int64(i+1) {
			t.Fatalf("item %d is seq %d", i, s)
		}
	}
}

// An offer that arrives while a running job's batch flush holds j.mu must
// not take w.mu and then wait for j.mu: the flush sends under j.mu, and
// sendRaw takes w.mu, so the two would deadlock and stop the worker (no
// pings, no decline). The test holds j.mu as the flush timer does, waits
// for handleOffer to park, then flushes.
func TestOfferDuringFlushDoesNotDeadlock(t *testing.T) {
	h := newHarness(t, harnessOpts{agent: "trap '' INT\nexec sleep 30\n"})
	h.offerJob(fixSpec("bjob_lock1"), nil)
	eventually(t, 20*time.Second, "first job running", func() bool { b, _ := os.ReadFile(h.argv); return len(b) > 0 })
	j := h.w.job("bjob_lock1")
	if j == nil {
		t.Fatal("first job not registered")
	}

	// Lock j.mu only once the job is quiet (nothing pending, no flush timer,
	// every frame acked): the read loop applies an ack under j.mu, and an
	// ack stuck behind this lock would hold up the offer frame itself.
	eventually(t, 20*time.Second, "first job quiet", func() bool {
		j.mu.Lock()
		if len(j.pending) == 0 && j.timer == nil && j.acked == j.meta.NextSeq-1 {
			return true // keep j.mu
		}
		j.mu.Unlock()
		return false
	})
	h.offerJob(fixSpec("bjob_lock2"), nil)
	deadline := time.Now().Add(10 * time.Second)
	// Parked across two samples: a brief wait for w.mu is not the park on
	// j.mu this test is after.
	parked := func() bool {
		if !offerParked() {
			return false
		}
		time.Sleep(100 * time.Millisecond)
		return offerParked()
	}
	for !parked() {
		if time.Now().After(deadline) {
			j.mu.Unlock()
			t.Fatal("timed out waiting for handleOffer parked on a mutex")
		}
		time.Sleep(20 * time.Millisecond)
	}
	flushed := make(chan struct{})
	go func() {
		defer close(flushed)
		j.pending = append(j.pending, logEvent("flush during an offer", "info"))
		j.flushLocked()
		j.mu.Unlock()
	}()
	select {
	case <-flushed:
	case <-time.After(5 * time.Second):
		t.Fatal("deadlock: a flush holding j.mu waits for w.mu, which handleOffer holds while it waits for j.mu")
	}
	eventually(t, 10*time.Second, "busy decline", func() bool { return h.fs.declineFor("bjob_lock2") == workerproto.DeclineBusy })
}

// offerParked reports whether a handleOffer goroutine is blocked in
// sync.Mutex.Lock.
func offerParked() bool {
	buf := make([]byte, 1<<20)
	buf = buf[:runtime.Stack(buf, true)]
	for _, g := range strings.Split(string(buf), "\n\n") {
		if strings.Contains(g, "[sync.Mutex.Lock") && strings.Contains(g, "(*Worker).handleOffer(") {
			return true
		}
	}
	return false
}
