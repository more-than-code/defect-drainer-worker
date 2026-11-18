package worker

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	dexec "github.com/more-than-code/defect-drainer-worker/exec"
	"github.com/more-than-code/defect-drainer-worker/workerproto"
)

// Main Dev's 2026-11-16 clarification (3): seq is per job across every run.
// After a lease_lost (which drops the spool) a Resume — a lease frame on the
// same lease id, then control resume — continues at the record + 1.
func TestResumeAfterLeaseLostContinuesSeq(t *testing.T) {
	g := newRunGate(t)
	h := newHarness(t, harnessOpts{agent: g.agent(t)})
	spec := fixSpec("bjob_seq_resume")
	h.offerJob(spec, nil)
	h.waitDone(spec.JobID, 60*time.Second)
	n := h.fs.lastSeq(spec.JobID)
	g.arm(t)
	h.fs.send(workerproto.Error{Header: workerproto.NewHeader(workerproto.TypeError), Code: workerproto.CodeLeaseLost, JobID: spec.JobID, LeaseID: "ls_" + spec.JobID})
	eventually(t, 5*time.Second, "lease lost", func() bool { m, _ := readMeta(h.paths.JobMeta(spec.JobID)); return m.LeaseLost })
	if m, _ := readMeta(h.paths.JobMeta(spec.JobID)); m.NextSeq != n+1 {
		t.Fatalf("seq record %d after lease_lost, want %d", m.NextSeq, n+1)
	}

	h.fs.mu.Lock()
	h.fs.jobs[spec.JobID].spec.Mode = dexec.ModeResume
	h.fs.mu.Unlock()
	before := len(h.fs.progressSeqs(spec.JobID))
	h.fs.lease(spec.JobID)
	h.fs.control(spec.JobID, "rq_resume1", dexec.Control{Kind: dexec.ControlResume, SpecURL: workerproto.PathPrefix + "jobs/" + spec.JobID + "/spec"})
	eventually(t, 10*time.Second, "resume result", func() bool { _, c := h.fs.controlResult("rq_resume1"); return c > 0 })
	if r, _ := h.fs.controlResult("rq_resume1"); !r.OK {
		t.Fatalf("resume refused: %+v", r)
	}
	g.openAfter(t, h, spec.JobID, n+1)
	eventually(t, 60*time.Second, "second done", func() bool { return h.fs.lastSeq(spec.JobID) > n+1 && lastIsDone(h.fs.jobEvents(spec.JobID)) })
	seqs := h.fs.progressSeqs(spec.JobID)[before:]
	if len(seqs) == 0 || seqs[0] != n+1 {
		t.Fatalf("resume frames start at %v, want %d", seqs, n+1)
	}
	if m, _ := readMeta(h.paths.JobMeta(spec.JobID)); m.LeaseLost {
		t.Fatal("lease still marked lost after the resume lease")
	}
}

// (3) across a worker restart, and a rerun offer (new lease id) on the
// existing worktrees continues at the record + 1.
func TestRerunAfterRestartContinuesSeq(t *testing.T) {
	g := newRunGate(t)
	h := newHarness(t, harnessOpts{agent: g.agent(t)})
	spec := fixSpec("bjob_seq_rerun")
	h.offerJob(spec, nil)
	h.waitDone(spec.JobID, 60*time.Second)
	eventually(t, 5*time.Second, "acked", func() bool { return len(h.w.busyIDs()) == 0 })
	n := h.fs.lastSeq(spec.JobID)
	g.arm(t)
	h.stop()
	h.start(Timing{})

	before := len(h.fs.progressSeqs(spec.JobID))
	h.fs.offerMode(spec.JobID, "ls_rerun2", "dd_wjt_rerun2", dexec.ModeRerun)
	g.openAfter(t, h, spec.JobID, n+1)
	eventually(t, 60*time.Second, "rerun done", func() bool { return h.fs.lastSeq(spec.JobID) > n+1 && lastIsDone(h.fs.jobEvents(spec.JobID)) })
	if !h.fs.accepted(spec.JobID) || h.fs.declineFor(spec.JobID) != "" {
		t.Fatalf("rerun declined: %s", h.fs.declineFor(spec.JobID))
	}
	seqs := h.fs.progressSeqs(spec.JobID)[before:]
	if seqs[0] != n+1 {
		t.Fatalf("rerun frames start at %d, want %d", seqs[0], n+1)
	}
	for _, f := range h.fs.frames(workerproto.TypeProgress)[before:] {
		if p := mustDecode[workerproto.Progress](h.fs, f.raw); p.JobID == spec.JobID && p.LeaseID != "ls_rerun2" {
			t.Fatalf("rerun frame on lease %s", p.LeaseID)
		}
	}
}

// (4): a worker whose record is ahead of the server gets seq_gap{expected}
// and resends its unacked frames renumbered from expected; the server
// applies them all, in order, once.
func TestSeqGapRenumbersAWorkerThatIsAhead(t *testing.T) {
	g := newRunGate(t)
	h := newHarness(t, harnessOpts{agent: g.agent(t)})
	spec := fixSpec("bjob_seq_ahead")
	h.offerJob(spec, nil)
	h.waitDone(spec.JobID, 60*time.Second)
	eventually(t, 5*time.Second, "acked", func() bool { return len(h.w.busyIDs()) == 0 })
	n := h.fs.lastSeq(spec.JobID)
	g.arm(t)
	h.stop()
	// The worker's record is 5 ahead of the server's last_seq.
	m, _ := readMeta(h.paths.JobMeta(spec.JobID))
	m.NextSeq = n + 6
	b, _ := json.Marshal(m)
	writeFile(t, h.paths.JobMeta(spec.JobID), string(b), 0o600)
	h.start(Timing{})

	h.fs.offerMode(spec.JobID, "ls_ahead2", "dd_wjt_ahead2", dexec.ModeRerun)
	g.openAfter(t, h, spec.JobID, n+1)
	eventually(t, 60*time.Second, "rerun applied", func() bool { return h.fs.lastSeq(spec.JobID) > n+1 && lastIsDone(h.fs.jobEvents(spec.JobID)) })
	h.fs.mu.Lock()
	fj := h.fs.jobs[spec.JobID]
	blocked, frames, last := fj.blocked, fj.frames, fj.lastSeq
	h.fs.mu.Unlock()
	if blocked || int64(frames) != last {
		t.Fatalf("frames %d last %d blocked %v", frames, last, blocked)
	}
	gap := false
	for _, s := range h.fs.progressSeqs(spec.JobID) {
		if s == n+6 {
			gap = true
		}
	}
	if !gap {
		t.Fatal("the worker never sent from its own record (the test did not exercise seq_gap)")
	}
	if m, _ := readMeta(h.paths.JobMeta(spec.JobID)); m.NextSeq != last+1 {
		t.Fatalf("record %d after resync, want %d", m.NextSeq, last+1)
	}
}

// (5): with the job dir or its seq record gone, a rerun offer is declined
// bad_spec and a resume answers ok:false bad_spec; nothing restarts at 1.
// Discard removes both, so it refuses them too.
func TestMissingSeqRecordRefusesRerunAndResume(t *testing.T) {
	h := newHarness(t, harnessOpts{})
	for _, id := range []string{"bjob_noseq", "bjob_discarded"} {
		h.offerJob(fixSpec(id), nil)
		h.waitDone(id, 60*time.Second)
	}
	eventually(t, 5*time.Second, "acked", func() bool { return len(h.w.busyIDs()) == 0 })
	if err := os.Remove(h.paths.JobMeta("bjob_noseq")); err != nil {
		t.Fatal(err)
	}
	h.fs.control("bjob_discarded", "rq_disc", dexec.Control{Kind: dexec.ControlDiscard})
	eventually(t, 10*time.Second, "discard", func() bool { _, c := h.fs.controlResult("rq_disc"); return c > 0 })
	if r, _ := h.fs.controlResult("rq_disc"); !r.OK {
		t.Fatalf("discard %+v", r)
	}
	for _, id := range []string{"bjob_noseq", "bjob_discarded"} {
		frames := len(h.fs.progressSeqs(id))
		h.fs.offerMode(id, "ls_again_"+id, "dd_wjt_again_"+id, dexec.ModeRerun)
		eventually(t, 10*time.Second, "decline "+id, func() bool { return h.fs.declineFor(id) != "" })
		if r := h.fs.declineFor(id); r != workerproto.DeclineBadSpec {
			t.Fatalf("%s: rerun declined %s", id, r)
		}
		h.fs.mu.Lock()
		h.fs.jobs[id].spec.Mode = dexec.ModeResume
		h.fs.mu.Unlock()
		h.fs.control(id, "rq_res_"+id, dexec.Control{Kind: dexec.ControlResume, SpecURL: workerproto.PathPrefix + "jobs/" + id + "/spec"})
		eventually(t, 10*time.Second, "resume answer "+id, func() bool { _, c := h.fs.controlResult("rq_res_" + id); return c > 0 })
		if r, _ := h.fs.controlResult("rq_res_" + id); r.OK || r.Code != workerproto.DeclineBadSpec {
			t.Fatalf("%s: resume %+v", id, r)
		}
		time.Sleep(200 * time.Millisecond)
		if got := len(h.fs.progressSeqs(id)); got != frames {
			t.Fatalf("%s: %d frames sent after the refusal", id, got-frames)
		}
	}
}

func lastIsDone(ev []dexec.Event) bool {
	return len(ev) > 0 && ev[len(ev)-1].Kind == dexec.EventDone
}

// runGate makes a job's second run span at least two progress frames. The
// worker batches events by time, and a rerun or resume on existing worktrees
// can finish inside one batch interval, so its done would ride in its first
// frame and a wait for seq > n+1 would never end. Once armed (after the first
// run), the fake agent waits at its start until the server has applied the
// second run's first frame (the launch and spawn events), then finishes.
type runGate struct{ armed, open string }

func newRunGate(t *testing.T) runGate {
	dir := t.TempDir()
	return runGate{armed: filepath.Join(dir, "armed"), open: filepath.Join(dir, "open")}
}

func (g runGate) agent(t *testing.T) string {
	return "if [ -f '" + g.armed + "' ]; then while [ ! -f '" + g.open + "' ]; do sleep 0.02; done; fi\n" +
		fixArtifacts(testDefect) + "cat '" + claudeFixture(t, "c1-basic") + "'\nexit 0\n"
}

func (g runGate) arm(t *testing.T) { writeFile(t, g.armed, "", 0o644) }

// openAfter opens the gate once the server's last_seq for jobID reaches seq.
func (g runGate) openAfter(t *testing.T, h *harness, jobID string, seq int64) {
	t.Helper()
	eventually(t, 30*time.Second, "the second run's first frame", func() bool { return h.fs.lastSeq(jobID) >= seq })
	writeFile(t, g.open, "", 0o644)
}
