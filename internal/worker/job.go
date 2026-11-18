package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/more-than-code/defect-drainer-worker/exec"
	"github.com/more-than-code/defect-drainer-worker/internal/botagent/config"
	"github.com/more-than-code/defect-drainer-worker/internal/botagent/spool"
	"github.com/more-than-code/defect-drainer-worker/workerproto"
)

// Job states in job.json.
const (
	stateStarting = "starting"
	stateRunning  = "running"
	stateEnded    = "ended"
)

// jobMeta is {state}/jobs/<id>/job.json (mode 0600). It never holds a job
// token or a secret value. NextSeq is the job's seq record: one above the
// highest seq ever spooled (spooled before sent). It survives a lease_lost
// spool drop and a worker restart; only discard removes it, and every run
// continues from it (Main Dev, 2026-11-16).
type jobMeta struct {
	JobID     string       `json:"jobId"`
	LeaseID   string       `json:"leaseId"`
	Adapter   string       `json:"adapter"`
	Model     string       `json:"model,omitempty"`
	State     string       `json:"state"`
	NextSeq   int64        `json:"nextSeq"`
	Spec      exec.JobSpec `json:"spec"`
	EndedAt   string       `json:"endedAt,omitempty"`
	LeaseLost bool         `json:"leaseLost,omitempty"`
}

// job is one job this worker holds: running, or ended with a spool, stored
// control results or worktrees.
type job struct {
	w  *Worker
	id string
	sp *spool.Spool

	// applyMu orders events: an event is prepared (redacted, blobs uploaded)
	// and queued before the next one is looked at.
	applyMu sync.Mutex
	// ctrlMu serialises controls for this job.
	ctrlMu sync.Mutex

	mu          sync.Mutex
	meta        jobMeta
	token       string
	attached    bool
	acked       int64
	pending     []exec.Event
	timer       *time.Timer
	gen         int
	live        bool
	doneSpooled bool
	cancelled   bool
	ctx         context.Context
	cancel      context.CancelFunc
	proc        *exec.Proc
	secretEnv   string
	ctrlActive  int // controls running now: their lease is renewed by ping
	// gitTokens are this job's minted GitHub tokens (§14), keyed repo+access.
	// Memory only: never in job.json, the spool or a result.
	gitTokens map[string]cachedGitToken
}

func spoolID(seq int64) string { return fmt.Sprintf("p%019d", seq) }

func seqOf(id string) (int64, bool) {
	if !strings.HasPrefix(id, "p") {
		return 0, false
	}
	n, err := strconv.ParseInt(id[1:], 10, 64)
	return n, err == nil && n > 0
}

// openJob opens (creating) the job's dir and spool.
func (w *Worker) openJob(meta jobMeta) (*job, error) {
	dir := w.opts.Paths.JobDir(meta.JobID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	// The cap leaves room for the final done frame of a job that hit it.
	sp, err := spool.OpenLimited(w.opts.Paths.SpoolRoot(meta.JobID), w.opts.SpoolMax+2*workerproto.MaxFrameBytes)
	if err != nil {
		return nil, err
	}
	if meta.NextSeq < 1 {
		meta.NextSeq = 1
	}
	j := &job{w: w, id: meta.JobID, sp: sp, meta: meta}
	j.ctx, j.cancel = context.WithCancel(context.Background())
	items, err := sp.List()
	if err != nil {
		return nil, err
	}
	j.acked = meta.NextSeq - 1
	for _, it := range items {
		if seq, ok := seqOf(it.ID); ok {
			if seq-1 < j.acked {
				j.acked = seq - 1
			}
			if seq >= j.meta.NextSeq {
				j.meta.NextSeq = seq + 1
			}
		}
	}
	return j, j.saveMetaLocked()
}

func (j *job) saveMetaLocked() error {
	b, err := json.Marshal(j.meta)
	if err != nil {
		return err
	}
	return config.WriteFileAtomic(j.w.opts.Paths.JobMeta(j.id), b, 0o600, 0o700)
}

func readMeta(path string) (jobMeta, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return jobMeta{}, err
	}
	var m jobMeta
	if err := json.Unmarshal(b, &m); err != nil {
		return jobMeta{}, err
	}
	return m, nil
}

// busy is §4.1's rule: a live agent child, an unacked spool entry, or an
// unacked control result.
func (j *job) busy() bool {
	j.mu.Lock()
	live := j.live
	j.mu.Unlock()
	if live {
		return true
	}
	if items, err := j.sp.List(); err == nil && len(items) > 0 {
		return true
	}
	return j.w.hasStoredResults(j.id)
}

// --- exec.Sink, per run ---

// runSink is one run's view of the job. A run whose generation is stale
// (the job was cancelled, lost or resumed since) is told ErrInactive.
type runSink struct {
	j   *job
	gen int
}

func (s runSink) Apply(e exec.Event) error {
	j := s.j
	j.applyMu.Lock()
	defer j.applyMu.Unlock()
	j.mu.Lock()
	if s.gen != j.gen || j.doneSpooled {
		j.mu.Unlock()
		return exec.ErrInactive
	}
	if j.cancelled && (e.Kind != exec.EventLog || anyGuarded(e.Lines)) {
		j.mu.Unlock()
		return exec.ErrInactive
	}
	ctx := j.ctx
	j.mu.Unlock()
	e = j.prepare(ctx, e)
	j.mu.Lock()
	defer j.mu.Unlock()
	if s.gen != j.gen || j.doneSpooled {
		return exec.ErrInactive
	}
	j.enqueueLocked(e)
	return nil
}

func anyGuarded(lines []exec.LogLine) bool {
	for _, l := range lines {
		if l.Guarded {
			return true
		}
	}
	return false
}

func (s runSink) Gone() bool { return false }

func (s runSink) Cancelled() bool {
	s.j.mu.Lock()
	defer s.j.mu.Unlock()
	return s.j.cancelled || s.gen != s.j.gen
}

func (s runSink) Track(p *exec.Proc) bool {
	s.j.mu.Lock()
	defer s.j.mu.Unlock()
	if s.j.cancelled || s.gen != s.j.gen {
		return false
	}
	s.j.proc = p
	return true
}

func (s runSink) Untrack(p *exec.Proc) {
	s.j.mu.Lock()
	defer s.j.mu.Unlock()
	if s.j.proc == p {
		s.j.proc = nil
	}
}

// --- queueing, spool and send ---

// enqueueLocked adds e to the batch and flushes on a done, a full batch, or
// the batch interval (§6.1).
func (j *job) enqueueLocked(e exec.Event) {
	e.JobID = j.id
	j.pending = append(j.pending, e)
	if e.Kind == exec.EventDone || len(j.pending) >= workerproto.MaxBatchEvents {
		j.flushLocked()
		return
	}
	if j.timer == nil {
		j.timer = time.AfterFunc(j.w.t.BatchInterval, func() {
			j.mu.Lock()
			defer j.mu.Unlock()
			j.timer = nil
			j.flushLocked()
		})
	}
}

// emitLocked queues a worker-authored event (log, done) for the current
// run, bypassing the sink's generation check.
func (j *job) emitLocked(e exec.Event) {
	if j.doneSpooled {
		return
	}
	e, truncated := capEvent(e, j.w.currentRedactor())
	for _, p := range truncated {
		e.Lines = append(e.Lines, exec.LogLine{Text: "field truncated: " + p, Level: "warn", Source: "dd-worker"})
	}
	j.enqueueLocked(e)
}

func logEvent(text, level string) exec.Event {
	return exec.Event{Kind: exec.EventLog, Lines: []exec.LogLine{{Text: text, Level: level, Source: "dd-worker"}}}
}

func doneEvent(state, line string) exec.Event {
	return exec.Event{Kind: exec.EventDone, Done: &exec.Done{State: state, Line: line}}
}

// flushLocked writes the pending events as progress frames: each one to the
// spool first (mode 0600), then to the socket when a lease is held.
func (j *job) flushLocked() {
	if j.timer != nil {
		j.timer.Stop()
		j.timer = nil
	}
	for len(j.pending) > 0 {
		n := len(j.pending)
		if n > workerproto.MaxBatchEvents {
			n = workerproto.MaxBatchEvents
		}
		frame, n := j.buildFrameLocked(n)
		hasDone := false
		for _, e := range j.pending[:n] {
			if e.Kind == exec.EventDone {
				hasDone = true
			}
		}
		if !hasDone {
			if used, err := j.sp.Bytes(); err == nil && used+int64(len(frame)) > j.w.opts.SpoolMax {
				j.spoolFullLocked(used)
				continue
			}
		}
		seq := j.meta.NextSeq
		if err := j.sp.Put(spoolID(seq), frame, time.Now()); err != nil {
			j.w.logf("job %s: spool write failed: %v", j.id, err)
			if errors.Is(err, spool.ErrFull) && !hasDone {
				j.spoolFullLocked(0)
				continue
			}
			// The frame cannot be kept: drop it rather than send something
			// that a reconnect could not replay.
			j.pending = j.pending[n:]
			continue
		}
		j.pending = j.pending[n:]
		j.meta.NextSeq = seq + 1
		if hasDone {
			j.doneSpooled = true
			j.live = false
			j.meta.State = stateEnded
			j.meta.EndedAt = time.Now().UTC().Format(time.RFC3339)
			j.w.releaseSecretEnv(j.secretEnv)
			j.secretEnv = ""
			j.pending = nil
		}
		_ = j.saveMetaLocked()
		if j.attached {
			j.w.sendRaw(frame)
		}
	}
}

// buildFrameLocked marshals the first n pending events, halving n until the
// frame fits 1 MiB (each event is already ≤ 768 KiB).
func (j *job) buildFrameLocked(n int) ([]byte, int) {
	// Every event names this job (§2), including worker-authored ones
	// queued without going through enqueueLocked.
	for i := range j.pending {
		j.pending[i].JobID = j.id
	}
	for {
		f := workerproto.Progress{
			Header: workerproto.NewHeader(workerproto.TypeProgress),
			JobID:  j.id, LeaseID: j.meta.LeaseID, Seq: j.meta.NextSeq,
			Events: j.pending[:n],
		}
		b, err := json.Marshal(f)
		if err == nil && (len(b) <= workerproto.MaxFrameBytes || n == 1) {
			return b, n
		}
		n /= 2
		if n < 1 {
			n = 1
		}
	}
}

// spoolFullLocked ends a job whose spool alone is over the cap (§6.1).
func (j *job) spoolFullLocked(used int64) {
	j.w.logf("job %s: spool over %d MiB, ending the job", j.id, j.w.opts.SpoolMax>>20)
	j.cancelled = true
	j.gen++
	p := j.proc
	j.cancel()
	if p != nil {
		go j.w.stopProc(p)
	}
	j.pending = []exec.Event{doneEvent("failed", fmt.Sprintf("%s: job spool over %d MiB", workerproto.ReasonSpoolFull, j.w.opts.SpoolMax>>20))}
}

// ack drops every spooled frame up to seq (acks are cumulative).
func (j *job) ack(seq int64) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if seq > j.meta.NextSeq-1 {
		// Never drop a frame that was not sent; a server acking beyond it
		// is wrong, so the excess is ignored.
		j.w.logf("job %s: ack %d beyond the last sent seq %d", j.id, seq, j.meta.NextSeq-1)
		seq = j.meta.NextSeq - 1
	}
	if seq <= j.acked {
		return
	}
	for s := j.acked + 1; s <= seq; s++ {
		_ = j.sp.Delete(spoolID(s))
	}
	j.acked = seq
	if j.doneSpooled && j.acked >= j.meta.NextSeq-1 {
		// The done is applied: the lease is over. Blobs are no longer
		// needed for a resend; worktrees stay until discard.
		j.attached = false
		_ = os.RemoveAll(j.w.opts.Paths.Blobs(j.id))
	}
}

// spoolItems lists the spool ordered by frame seq (the id), not by the
// order the entries were written.
func (j *job) spoolItems() ([]spool.Item, error) {
	items, err := j.sp.List()
	if err != nil {
		return nil, err
	}
	sort.SliceStable(items, func(a, b int) bool {
		sa, _ := seqOf(items[a].ID)
		sb, _ := seqOf(items[b].ID)
		return sa < sb
	})
	return items, nil
}

// replay resends every spooled frame from seq from (lowest first), with
// the current lease id. Frames below from are already applied and are
// dropped.
func (j *job) replay(from int64) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if !j.attached {
		return
	}
	items, err := j.spoolItems()
	if err != nil {
		j.w.logf("job %s: spool read failed: %v", j.id, err)
		return
	}
	for _, it := range items {
		seq, ok := seqOf(it.ID)
		if !ok {
			continue
		}
		if seq < from {
			_ = j.sp.Delete(it.ID)
			continue
		}
		var f workerproto.Progress
		if err := json.Unmarshal(it.Frame, &f); err != nil {
			continue
		}
		f.LeaseID = j.meta.LeaseID
		b, err := json.Marshal(f)
		if err != nil {
			continue
		}
		j.w.sendRaw(b)
	}
}

// resync is the seq_gap answer (Main Dev, 2026-11-16): the server has
// applied everything below expected, so spooled frames below it are dropped
// and the rest are resent in order, renumbered from expected. Within a run
// this changes nothing; for a worker whose new run is ahead of the server
// it brings both back in step. The seq record (job.json nextSeq) follows.
func (j *job) resync(expected int64) {
	if expected < 1 {
		return
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	if expected > j.meta.NextSeq {
		// The server expects more than this worker ever sent: nothing to
		// renumber towards. Logged, not followed.
		j.w.logf("job %s: seq_gap expects %d beyond the record %d", j.id, expected, j.meta.NextSeq)
		return
	}
	items, err := j.spoolItems()
	if err != nil {
		j.w.logf("job %s: spool read failed: %v", j.id, err)
		return
	}
	type entry struct {
		id string
		f  workerproto.Progress
	}
	var keep []entry
	for _, it := range items {
		seq, ok := seqOf(it.ID)
		if !ok {
			continue
		}
		var f workerproto.Progress
		if err := json.Unmarshal(it.Frame, &f); err != nil {
			continue
		}
		if seq < expected {
			// Already applied by the server (its last_seq is expected-1).
			_ = j.sp.Delete(it.ID)
			continue
		}
		keep = append(keep, entry{it.ID, f})
	}
	next := expected
	newIDs := map[string]bool{}
	var frames [][]byte
	for _, e := range keep {
		e.f.Seq = next
		e.f.LeaseID = j.meta.LeaseID
		b, err := json.Marshal(e.f)
		if err != nil {
			continue
		}
		id := spoolID(next)
		if id != e.id {
			if err := j.sp.Put(id, b, time.Now()); err != nil {
				j.w.logf("job %s: spool rewrite failed: %v", j.id, err)
				return
			}
		}
		newIDs[id] = true
		frames = append(frames, b)
		next++
	}
	for _, e := range keep {
		if !newIDs[e.id] {
			_ = j.sp.Delete(e.id)
		}
	}
	j.acked = expected - 1
	j.meta.NextSeq = next
	_ = j.saveMetaLocked()
	if j.attached {
		for _, b := range frames {
			j.w.sendRaw(b)
		}
	}
}

// attach records a lease on the current socket and replays the spool. A
// lease frame for a job that lost its lease reactivates it (a Resume).
func (j *job) attach(leaseID, token string) {
	j.mu.Lock()
	j.attached = true
	if leaseID != "" && (leaseID != j.meta.LeaseID || j.meta.LeaseLost) {
		j.meta.LeaseID = leaseID
		j.meta.LeaseLost = false
		_ = j.saveMetaLocked()
	}
	if token != "" {
		j.token = token
	}
	j.mu.Unlock()
	j.w.rememberToken(token)
	j.replay(0)
}

func (j *job) detach() {
	j.mu.Lock()
	j.attached = false
	j.mu.Unlock()
}

// leaseLost is §5.3: stop the child, drop only this job's spool and stored
// control results, keep the worktrees.
func (j *job) leaseLost() {
	j.mu.Lock()
	j.cancelled = true
	j.gen++
	j.attached = false
	j.live = false
	j.doneSpooled = true
	j.pending = nil
	if j.timer != nil {
		j.timer.Stop()
		j.timer = nil
	}
	p := j.proc
	j.cancel()
	items, _ := j.sp.List()
	for _, it := range items {
		_ = j.sp.Delete(it.ID)
	}
	j.meta.State = stateEnded
	j.meta.LeaseLost = true
	_ = j.saveMetaLocked()
	j.w.releaseSecretEnv(j.secretEnv)
	j.secretEnv = ""
	j.mu.Unlock()
	_ = os.RemoveAll(j.w.opts.Paths.JobControls(j.id))
	if p != nil {
		go j.w.stopProc(p)
	}
	j.w.logf("job %s: lease lost; spool dropped, worktrees kept", j.id)
}

// cancelRun is the cancel control: stop the child (SIGINT, SIGKILL after
// the grace) and end the events with done{cancelled} at once (§6.3).
func (j *job) cancelRun() {
	j.applyMu.Lock()
	defer j.applyMu.Unlock()
	j.mu.Lock()
	if j.doneSpooled {
		j.mu.Unlock()
		return
	}
	j.cancelled = true
	p := j.proc
	j.cancel()
	j.emitLocked(doneEvent("cancelled", "cancelled by the operator"))
	j.mu.Unlock()
	if p != nil {
		go j.w.stopProc(p)
	}
}

// stopProc sends SIGINT to the child's process group (or the child), then
// SIGKILL after the grace. It returns once the SIGKILL has been sent.
func (w *Worker) stopProc(p *exec.Proc) {
	if p == nil || p.Cmd == nil || p.Cmd.Process == nil {
		return
	}
	pid := p.Cmd.Process.Pid
	if pgid, err := syscall.Getpgid(pid); err == nil && pgid == pid {
		_ = syscall.Kill(-pid, syscall.SIGINT)
	} else {
		_ = p.Cmd.Process.Signal(syscall.SIGINT)
	}
	time.Sleep(w.t.CancelGrace)
	p.Kill()
	if pgid, err := syscall.Getpgid(pid); err == nil && pgid == pid {
		_ = syscall.Kill(-pid, syscall.SIGKILL)
	}
}

// --- recovery (§6.4) ---

// recoverJobs opens every job left on disk. A job that was running when the
// worker stopped has lost its child: unless its spool already holds a done,
// a done{failed, worker_restarted} is appended. Ended jobs' unacked frames
// older than 24 h are dropped (§6.1).
func (w *Worker) recoverJobs() error {
	entries, err := os.ReadDir(w.opts.Paths.JobsDir())
	if err != nil {
		return err
	}
	for _, e := range entries {
		if !e.IsDir() || !workerproto.ValidID(e.Name()) {
			continue
		}
		meta, err := readMeta(w.opts.Paths.JobMeta(e.Name()))
		if err != nil || meta.JobID != e.Name() {
			continue
		}
		j, err := w.openJob(meta)
		if err != nil {
			w.logf("job %s: cannot reopen: %v", meta.JobID, err)
			continue
		}
		hasDone := spoolHasDone(j.sp)
		if hasDone {
			j.doneSpooled = true
		}
		if meta.State != stateEnded && !meta.LeaseLost {
			j.mu.Lock()
			if !hasDone {
				j.live = false
				j.pending = []exec.Event{doneEvent("failed", workerproto.ReasonWorkerRestarted+": agent child lost")}
				j.flushLocked()
			}
			j.meta.State = stateEnded
			_ = j.saveMetaLocked()
			j.mu.Unlock()
		} else if meta.State == stateEnded {
			_ = j.sp.DropOlderThan(workerproto.SpoolEndedJobTTL, time.Now())
		}
		j.doneSpooled = true
		w.jobs[j.id] = j
	}
	return nil
}

func spoolHasDone(sp *spool.Spool) bool {
	items, err := sp.List()
	if err != nil {
		return false
	}
	for _, it := range items {
		var f workerproto.Progress
		if json.Unmarshal(it.Frame, &f) != nil {
			continue
		}
		for _, e := range f.Events {
			if e.Kind == exec.EventDone {
				return true
			}
		}
	}
	return false
}

// sweepEndedSpools drops unacked frames older than 24 h of jobs that
// already ended (§6.1); the server has marked those jobs worker_lost.
func (w *Worker) sweepEndedSpools() {
	w.mu.Lock()
	jobs := w.jobList()
	w.mu.Unlock()
	for _, j := range jobs {
		j.mu.Lock()
		ended := j.meta.State == stateEnded && !j.live
		j.mu.Unlock()
		if ended {
			_ = j.sp.DropOlderThan(workerproto.SpoolEndedJobTTL, time.Now())
		}
	}
}

// spoolBytes is the worker-wide spool size.
func (w *Worker) spoolBytes() int64 {
	w.mu.Lock()
	jobs := make([]*job, 0, len(w.jobs))
	for _, j := range w.jobs {
		jobs = append(jobs, j)
	}
	w.mu.Unlock()
	var n int64
	for _, j := range jobs {
		if b, err := j.sp.Bytes(); err == nil {
			n += b
		}
	}
	return n
}

// --- process env for the test-account secret ---

// acquireSecretEnv sets name in this process's environment for the run
// (exec reads it in-process; children never see it under its own name, it
// is in their deny set). Reference-counted across concurrent jobs.
func (w *Worker) acquireSecretEnv(name, value string) {
	if name == "" {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	w.envRefs[name]++
	_ = os.Setenv(name, value)
}

func (w *Worker) releaseSecretEnv(name string) {
	if name == "" {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	w.envRefs[name]--
	if w.envRefs[name] <= 0 {
		delete(w.envRefs, name)
		_ = os.Unsetenv(name)
	}
}

// --- stored control results (§6.3) ---

func (w *Worker) resultPath(jobID, requestID string) string {
	mustID(requestID)
	return filepath.Join(w.opts.Paths.JobControls(jobID), requestID+".json")
}

func (w *Worker) hasStoredResults(jobID string) bool {
	entries, err := os.ReadDir(w.opts.Paths.JobControls(jobID))
	return err == nil && len(entries) > 0
}
