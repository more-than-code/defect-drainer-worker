package worker

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"os"
	"slices"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"
	"github.com/more-than-code/defect-drainer-worker/internal/botagent/config"
	"github.com/more-than-code/defect-drainer-worker/internal/botagent/conn"
	"github.com/more-than-code/defect-drainer-worker/workerproto"
)

// socket is one authenticated connection.
type socket struct {
	c       *websocket.Conn
	writeMu sync.Mutex
	// unknown are frame types answered with unknown_type on this socket.
	unknown map[string]bool
	ctx     context.Context
	// lastRx is the unix nanos of the last frame read; the ping loop
	// closes a socket that has been silent for 3 heartbeats.
	lastRx atomic.Int64
}

func (s *socket) write(ctx context.Context, b []byte) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	return conn.WriteRaw(ctx, s.c, b)
}

// send marshals v and writes it on the current socket. With no socket the
// frame is dropped: progress is replayed from the spool, controls from the
// stored results, the rest is re-sent by the next hello or status.
func (w *Worker) send(v any) {
	b, err := json.Marshal(v)
	if err != nil {
		return
	}
	w.sendRaw(b)
}

func (w *Worker) sendRaw(b []byte) {
	w.mu.Lock()
	s := w.sock
	w.mu.Unlock()
	if s == nil {
		return
	}
	if err := s.write(s.ctx, b); err != nil {
		w.logf("send failed: %v", conn.Safe(err))
	}
}

func (w *Worker) sendError(code, refType, detail string) {
	w.send(workerproto.Error{Header: workerproto.NewHeader(workerproto.TypeError), Code: code, RefType: refType, Detail: detail})
}

// closeError is how a session ended.
type closeError struct {
	code websocket.StatusCode
	err  error
}

// Unwrap lets the session-end log reach the server's websocket.CloseError
// (its reason text).
func (e closeError) Unwrap() error { return e.err }

func (e closeError) Error() string {
	if e.err != nil {
		return e.err.Error()
	}
	return "closed " + e.code.String()
}

// Run is the session loop: dial, hello, serve frames, reconnect with
// backoff, until ctx ends (nil) or the server ends this worker for good.
func (w *Worker) Run(ctx context.Context) error {
	backoff := conn.NewBackoff()
	var replaced []time.Time
	go w.readinessLoop(ctx)
	for {
		err := w.session(ctx, &backoff)
		if ctx.Err() != nil {
			return nil
		}
		if err != nil {
			w.logf("%s", sessionEndReason(err))
		}
		var ce closeError
		errors.As(err, &ce)
		switch {
		case errors.Is(err, errUnauthenticated) || ce.code == workerproto.CloseRevoked:
			w.revoke()
			return ErrRevoked
		case ce.code == workerproto.CloseProtocolUnsupported:
			return ErrProtocol
		case ce.code == workerproto.CloseOrgMismatch:
			return ErrOrgMismatch
		case ce.code == workerproto.CloseReplaced:
			now := time.Now()
			replaced = append(replaced, now)
			var recent []time.Time
			for _, t := range replaced {
				if now.Sub(t) <= w.t.ReplacedWindow {
					recent = append(recent, t)
				}
			}
			replaced = recent
			if len(replaced) >= workerproto.ReplacedMaxPer10Min {
				return ErrReplaced
			}
			wait := w.t.ReplacedBackoff + jitter(w.t.ReplacedBackoff/2)
			w.logf("socket replaced by another connection with this credential; waiting %s", wait.Round(time.Second))
			if !sleepCtx(ctx, wait) {
				return nil
			}
		default:
			d := backoff.Take()
			if d > w.t.BackoffMax {
				d = w.t.BackoffMax
			}
			w.logf("reconnecting in %s", d.Round(time.Millisecond))
			if !sleepCtx(ctx, d) {
				return nil
			}
		}
	}
}

// dialError marks a session that never connected.
type dialError struct{ err error }

func (e dialError) Error() string { return e.err.Error() }
func (e dialError) Unwrap() error { return e.err }

// sessionEndReason is the log line for a session's end (F9): the close code
// and its name with the server's reason, a connection that never opened, or
// the read/write error. Error text goes through conn.Safe, and logf redacts
// and strips control characters.
func sessionEndReason(err error) string {
	var de dialError
	if errors.As(err, &de) {
		return "connect failed: " + conn.Safe(de.err)
	}
	if errors.Is(err, errUnauthenticated) {
		return "connect refused: unauthenticated"
	}
	code := websocket.CloseStatus(err)
	var ce closeError
	if errors.As(err, &ce) {
		code = ce.code
	}
	if code != -1 {
		line := fmt.Sprintf("socket closed: code %d (%s)", int(code), closeCodeName(code))
		var wce websocket.CloseError
		if errors.As(err, &wce) && wce.Reason != "" {
			r := []rune(conn.Safe(errors.New(wce.Reason)))
			if len(r) > 120 {
				r = r[:120]
			}
			line += ": " + string(r)
		}
		return line
	}
	return "socket error: " + conn.Safe(err)
}

// closeCodeName names the protocol's own close codes and the standard ones.
func closeCodeName(c websocket.StatusCode) string {
	switch c {
	case workerproto.CloseRevoked:
		return "revoked"
	case workerproto.CloseProtocolUnsupported:
		return "protocol unsupported"
	case workerproto.CloseOrgMismatch:
		return "org mismatch"
	case workerproto.CloseReplaced:
		return "replaced"
	}
	return c.String()
}

func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// jitter is a random duration in [0, max).
func jitter(max time.Duration) time.Duration {
	if max <= 0 {
		return 0
	}
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return 0
	}
	u := float64(binary.BigEndian.Uint64(b[:])) / float64(math.MaxUint64)
	return time.Duration(u * float64(max))
}

// dial authenticates with env, then env.prev (§4.3). A 401 on both is a
// revocation (§4.4). A successful dial with env deletes env.prev.
func (w *Worker) dial(ctx context.Context) (*websocket.Conn, error) {
	u, err := wsURL(w.origin())
	if err != nil {
		return nil, err
	}
	p := w.opts.Paths
	cur, err := config.Load(p.Env())
	if err != nil {
		return nil, err
	}
	c, status, err := conn.DialStatus(ctx, u, cur.Credential)
	if err == nil {
		w.setConfig(cur)
		if _, statErr := os.Stat(p.EnvPrev()); statErr == nil {
			_ = os.Remove(p.EnvPrev())
			w.logf("signed in with the rotated credential; env.prev deleted")
		}
		return c, nil
	}
	if status != http.StatusUnauthorized {
		return nil, errors.New(conn.Safe(err, cur.Credential))
	}
	prev, perr := config.Load(p.EnvPrev())
	if perr != nil {
		return nil, errUnauthenticated
	}
	c, status, err = conn.DialStatus(ctx, u, prev.Credential)
	if err == nil {
		w.setConfig(prev)
		w.logf("signed in with env.prev: the newer credential was refused")
		return c, nil
	}
	if status == http.StatusUnauthorized {
		return nil, errUnauthenticated
	}
	return nil, errors.New(conn.Safe(err, prev.Credential))
}

func (w *Worker) setConfig(c config.Config) {
	w.mu.Lock()
	w.cfg = c
	w.rebuildRedactorLocked()
	w.mu.Unlock()
}

// session runs one connection to its end.
func (w *Worker) session(ctx context.Context, backoff *conn.Backoff) error {
	c, err := w.dial(ctx)
	if err != nil {
		if errors.Is(err, errUnauthenticated) {
			return err
		}
		return dialError{err}
	}
	sctx, cancel := context.WithCancel(ctx)
	defer cancel()
	s := &socket{c: c, unknown: map[string]bool{}, ctx: sctx}
	defer c.CloseNow()

	hello := w.helloFrame()
	b, _ := json.Marshal(hello)
	if err := s.write(sctx, b); err != nil {
		return err
	}
	hctx, hcancel := context.WithTimeout(sctx, w.t.HelloTimeout)
	ok, err := w.awaitHelloOK(hctx, s)
	hcancel()
	if err != nil {
		return closeErr(err)
	}
	backoff.Reset()
	w.mu.Lock()
	w.sock = s
	w.serverCaps = append([]string(nil), ok.Capabilities...)
	w.mu.Unlock()
	w.logf("connected (protocol %d, heartbeat %ds)", ok.Protocol, ok.HeartbeatSec)
	if !slices.Contains(ok.Capabilities, workerproto.CapGitTokenV1) {
		w.logf("server lacks %s: offers will be declined %s", workerproto.CapGitTokenV1, workerproto.DeclineBadSpec)
	}
	w.writeSnapshot(true)
	defer w.writeSnapshot(false)
	defer w.dropSocket(s)

	hb := time.Duration(ok.HeartbeatSec) * time.Second
	if hb <= 0 {
		hb = workerproto.HeartbeatSec * time.Second
	}
	s.lastRx.Store(time.Now().UnixNano())
	go w.pingLoop(sctx, s, hb)
	go w.leaseWait(sctx, hello.BusyJobIDs)

	for {
		typ, data, err := conn.ReadFrame(sctx, c)
		s.lastRx.Store(time.Now().UnixNano())
		if err != nil {
			if typ == "" && data == nil {
				return closeErr(err)
			}
			// A frame whose envelope is not v1 JSON.
			w.sendError(workerproto.CodeBadFrame, typ, "envelope")
			continue
		}
		w.dispatch(sctx, s, typ, data)
	}
}

func closeErr(err error) error {
	if code := websocket.CloseStatus(err); code != -1 {
		return closeError{code: code, err: err}
	}
	return err
}

// awaitHelloOK reads until hello_ok. A close before it ends the session.
func (w *Worker) awaitHelloOK(ctx context.Context, s *socket) (workerproto.HelloOK, error) {
	for {
		typ, data, err := conn.ReadFrame(ctx, s.c)
		if err != nil {
			if typ == "" && data == nil {
				return workerproto.HelloOK{}, err
			}
			continue
		}
		switch typ {
		case workerproto.TypeHelloOK:
			var ok workerproto.HelloOK
			if json.Unmarshal(data, &ok) != nil {
				return ok, errHelloRefused
			}
			if ok.Protocol != workerproto.Protocol {
				return ok, closeError{code: workerproto.CloseProtocolUnsupported}
			}
			return ok, nil
		case workerproto.TypeError:
			var e workerproto.Error
			_ = json.Unmarshal(data, &e)
			w.logf("server error before hello_ok: %s %s", e.Code, e.Detail)
		}
	}
}

func (w *Worker) dropSocket(s *socket) {
	w.mu.Lock()
	if w.sock == s {
		w.sock = nil
	}
	jobs := w.jobList()
	offers := w.offers
	w.offers = map[string]*pendingOffer{}
	w.mu.Unlock()
	for _, j := range jobs {
		j.detach()
	}
	// An offer is bound to its socket (§4.5): an unanswered one is gone.
	for _, o := range offers {
		o.abort()
	}
}

// jobList is w.jobs as a slice. Caller holds w.mu.
func (w *Worker) jobList() []*job {
	out := make([]*job, 0, len(w.jobs))
	for _, j := range w.jobs {
		out = append(out, j)
	}
	sort.Slice(out, func(a, b int) bool { return out[a].id < out[b].id })
	return out
}

// busyIDs is §4.1's busy_job_ids, including accepted offers still starting.
func (w *Worker) busyIDs() []string {
	w.mu.Lock()
	jobs := w.jobList()
	w.mu.Unlock()
	ids := []string{}
	seen := map[string]bool{}
	for _, j := range jobs {
		seen[j.id] = true
		if j.busy() {
			ids = append(ids, j.id)
		}
	}
	// Stored results of jobs no longer held (discarded) count too (§4.1).
	if entries, err := os.ReadDir(w.opts.Paths.ControlsDir()); err == nil {
		for _, e := range entries {
			if e.IsDir() && workerproto.ValidID(e.Name()) && !seen[e.Name()] && w.hasStoredResults(e.Name()) {
				ids = append(ids, e.Name())
			}
		}
	}
	sort.Strings(ids)
	return ids
}

func (w *Worker) helloFrame() workerproto.Hello {
	w.mu.Lock()
	st := append([]workerproto.AdapterStatus(nil), w.statuses...)
	pol := w.policy
	workerID := w.cfg.WorkerID
	w.mu.Unlock()
	if st == nil {
		st = w.checkReadiness(context.Background())
	}
	return workerproto.Hello{
		Header: workerproto.NewHeader(workerproto.TypeHello), WorkerID: workerID,
		Version: Version, Protocol: workerproto.Protocol, Capabilities: []string{workerproto.CapBlobsV1, workerproto.CapGitTokenV1},
		MaxJobs: pol.MaxJobs, BusyJobIDs: w.busyIDs(), Adapters: st, Tools: w.toolsNow(), Policy: pol.Wire(),
	}
}

func (w *Worker) statusFrame() workerproto.Status {
	w.mu.Lock()
	st := append([]workerproto.AdapterStatus(nil), w.statuses...)
	pol := w.policy
	w.mu.Unlock()
	return workerproto.Status{
		Header: workerproto.NewHeader(workerproto.TypeStatus), Adapters: st, Tools: w.toolsNow(), Policy: pol.Wire(),
		MaxJobs: pol.MaxJobs, BusyJobIDs: w.busyIDs(),
	}
}

func (w *Worker) toolsNow() []workerproto.ToolStatus {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]workerproto.ToolStatus(nil), w.tools...)
}

// recheckTools probes git and stores the result (display only). It runs on
// hello and in the 5-minute readiness loop, never inside an offer's answer
// window.
func (w *Worker) recheckTools(ctx context.Context) {
	tools := checkTools(ctx)
	w.mu.Lock()
	w.tools = tools
	w.mu.Unlock()
}

// checkReadiness probes every adapter under its job child env, and git
// under the worker's own host-tool env, and stores the results.
func (w *Worker) checkReadiness(ctx context.Context) []workerproto.AdapterStatus {
	w.mu.Lock()
	adapters := w.adapters
	sec := w.secrets
	w.mu.Unlock()
	out := make([]workerproto.AdapterStatus, 0, len(KnownAdapters))
	for _, name := range KnownAdapters {
		h := adapters[name]
		out = append(out, h.check(ctx, w.opts.Paths, sec.DenySet(h.keep(), nil)))
	}
	w.mu.Lock()
	w.statuses = out
	w.mu.Unlock()
	w.recheckTools(ctx)
	return out
}

// readinessLoop re-reads the host files and re-checks every adapter every
// 5 minutes, and sends status when anything changed (§4.2).
func (w *Worker) readinessLoop(ctx context.Context) {
	t := time.NewTicker(w.t.Readiness)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		before, _ := json.Marshal(w.statusFrame())
		w.sweepEndedSpools()
		w.reloadHostFiles()
		w.checkReadiness(ctx)
		after := w.statusFrame()
		if b, _ := json.Marshal(after); string(b) != string(before) {
			w.send(after)
		}
		w.mu.Lock()
		connected := w.sock != nil
		w.mu.Unlock()
		w.writeSnapshot(connected)
	}
}

// pingLoop sends ping every heartbeat, naming the leases this worker holds
// (at most max_jobs).
// It also closes a socket silent for 3 heartbeats (the server pongs every
// ping, so silence means a dead link), and sends status when the busy set
// changed since the last hello or status (§4.2).
func (w *Worker) pingLoop(ctx context.Context, s *socket, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	lastBusy := w.busyIDs()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		if silent := time.Since(time.Unix(0, s.lastRx.Load())); silent > 3*every {
			w.logf("socket closed: heartbeat timeout (no frame from the server for %s)", silent.Round(time.Second))
			s.c.CloseNow()
			return
		}
		w.mu.Lock()
		max := w.policy.MaxJobs
		jobs := w.jobList()
		w.mu.Unlock()
		var ids []string
		for _, j := range jobs {
			j.mu.Lock()
			held := j.attached && (j.live || !j.doneSpooled || j.acked < j.meta.NextSeq-1 || j.ctrlActive > 0)
			j.mu.Unlock()
			if held && len(ids) < max {
				ids = append(ids, j.id)
			}
		}
		w.send(workerproto.Ping{Header: workerproto.NewHeader(workerproto.TypePing), JobIDs: ids})
		if busy := w.busyIDs(); !slices.Equal(busy, lastBusy) {
			lastBusy = busy
			w.send(w.statusFrame())
		}
	}
}

// leaseWait replays a busy job whose lease frame has not come within the
// wait after hello_ok: the server then answers ack or lease_lost.
func (w *Worker) leaseWait(ctx context.Context, busy []string) {
	if !sleepCtx(ctx, w.t.LeaseWait) {
		return
	}
	for _, id := range busy {
		w.mu.Lock()
		j := w.jobs[id]
		w.mu.Unlock()
		if j == nil {
			continue
		}
		j.mu.Lock()
		attached := j.attached
		j.mu.Unlock()
		if !attached && j.busy() {
			j.attach("", "")
		}
	}
}

// --- frame dispatch ---

func (w *Worker) dispatch(ctx context.Context, s *socket, typ string, data []byte) {
	bad := func(err error) { w.sendError(workerproto.CodeBadFrame, typ, err.Error()) }
	switch typ {
	case workerproto.TypePong, workerproto.TypeHelloOK:
	case workerproto.TypeAck:
		var f workerproto.Ack
		if err := decode(data, &f); err != nil {
			bad(err)
			return
		}
		if err := f.Validate(); err != nil {
			bad(err)
			return
		}
		if j := w.job(f.JobID); j != nil {
			j.ack(f.Seq)
		}
	case workerproto.TypeLease:
		var f workerproto.Lease
		if err := decode(data, &f); err != nil {
			bad(err)
			return
		}
		if err := validateLease(f); err != nil {
			bad(err)
			return
		}
		w.onLease(f)
	case workerproto.TypeOffer:
		var f workerproto.Offer
		if err := decode(data, &f); err != nil {
			bad(err)
			return
		}
		if err := f.Validate(); err != nil {
			bad(err)
			return
		}
		w.rememberToken(f.JobToken)
		go w.handleOffer(ctx, s, f)
	case workerproto.TypeControl:
		var f workerproto.Control
		if err := decode(data, &f); err != nil {
			bad(err)
			return
		}
		if err := f.Validate(); err != nil {
			bad(err)
			return
		}
		w.rememberToken(f.JobToken)
		go w.handleControl(ctx, f)
	case workerproto.TypeControlAck:
		var f workerproto.ControlAck
		if err := decode(data, &f); err != nil {
			bad(err)
			return
		}
		if !workerproto.ValidID(f.JobID) || !workerproto.ValidID(f.RequestID) {
			bad(errors.New("invalid job_id or request_id"))
			return
		}
		_ = os.Remove(w.resultPath(f.JobID, f.RequestID))
	case workerproto.TypeRotated:
		var f workerproto.Rotated
		if err := decode(data, &f); err != nil {
			bad(err)
			return
		}
		w.onRotated(f)
	case workerproto.TypeError:
		var f workerproto.Error
		if err := decode(data, &f); err != nil {
			return // never answer an error with an error
		}
		w.onError(ctx, f)
	default:
		if !s.unknown[typ] {
			s.unknown[typ] = true
			w.sendError(workerproto.CodeUnknownType, typ, "")
		}
	}
}

// decode is lenient on unknown keys and strict on types (§2).
func decode(data []byte, v any) error {
	if err := json.Unmarshal(data, v); err != nil {
		return errors.New("wrong type or malformed JSON")
	}
	return nil
}

func validateLease(f workerproto.Lease) error {
	if !workerproto.ValidID(f.JobID) || !workerproto.ValidID(f.LeaseID) {
		return errors.New("invalid job_id or lease_id")
	}
	if f.JobToken == "" {
		return errors.New("missing job_token")
	}
	return nil
}

func (w *Worker) job(id string) *job {
	if !workerproto.ValidID(id) {
		return nil
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.jobs[id]
}

// onLease is a re-attach (§5.3) or a control lease (§6.3): record it and
// resume streaming from the spool.
func (w *Worker) onLease(f workerproto.Lease) {
	w.rememberToken(f.JobToken)
	if j := w.job(f.JobID); j != nil {
		j.attach(f.LeaseID, f.JobToken)
		return
	}
	// A lease for a job this worker does not hold: nothing to stream. The
	// server learns from the next ping, which will not name it.
	w.logf("lease for unknown job %s ignored", f.JobID)
}

func (w *Worker) onError(ctx context.Context, f workerproto.Error) {
	switch f.Code {
	case workerproto.CodeLeaseLost:
		w.mu.Lock()
		o := w.offers[f.JobID]
		w.mu.Unlock()
		if o != nil && (f.LeaseID == "" || f.LeaseID == o.offer.LeaseID) {
			o.abort()
		}
		if j := w.job(f.JobID); j != nil {
			j.mu.Lock()
			match := f.LeaseID == "" || f.LeaseID == j.meta.LeaseID
			j.mu.Unlock()
			if match {
				j.leaseLost()
				// The job is no longer busy: say so now, so the scheduler can
				// offer a Rerun without waiting for the next re-check.
				w.send(w.statusFrame())
			}
		}
	case workerproto.CodeSeqGap:
		if j := w.job(f.JobID); j != nil {
			j.resync(f.Expected)
		}
	case workerproto.CodeBlobMissing:
		if j := w.job(f.JobID); j != nil {
			if err := j.reuploadBlob(ctx, f.SHA256); err != nil {
				w.logf("job %s: blob %s cannot be re-uploaded: %v", f.JobID, f.SHA256, err)
			}
			j.replay(0)
		}
	default:
		w.logf("server error %s (%s): %s", f.Code, f.RefType, f.Detail)
	}
}

// onRotated is §4.3: env → env.prev, the new credential → env, then ack.
func (w *Worker) onRotated(f workerproto.Rotated) {
	p := w.opts.Paths
	w.mu.Lock()
	cur := w.cfg
	w.mu.Unlock()
	next := cur
	next.Credential = f.Credential
	if err := next.Validate(); err != nil {
		w.sendError(workerproto.CodeBadFrame, workerproto.TypeRotated, "credential is not usable")
		return
	}
	if err := os.Rename(p.Env(), p.EnvPrev()); err != nil {
		w.logf("rotation: cannot keep the current credential as env.prev: %v", err)
		return
	}
	if err := config.Write(p.Env(), next); err != nil {
		_ = os.Rename(p.EnvPrev(), p.Env())
		w.logf("rotation: cannot write the new credential: %v", err)
		return
	}
	w.setConfig(next)
	w.logf("credential rotated; previous kept as env.prev until the next sign-in")
	w.send(workerproto.RotateAck{Header: workerproto.NewHeader(workerproto.TypeRotateAck)})
}

// revoke is §4.4 on the worker: stop every child (SIGINT, SIGKILL after the
// grace), delete the credential files and every spool and stored result,
// keep the worktrees.
func (w *Worker) revoke() {
	w.mu.Lock()
	w.revoked = true
	jobs := w.jobList()
	w.tokens = map[string]bool{}
	w.mu.Unlock()
	var wg sync.WaitGroup
	for _, j := range jobs {
		j.mu.Lock()
		j.cancelled = true
		j.gen++
		j.attached = false
		j.token = ""
		j.gitTokens = nil
		p := j.proc
		j.cancel()
		j.mu.Unlock()
		if p != nil {
			wg.Add(1)
			go func() { defer wg.Done(); w.stopProc(p) }()
		}
		_ = os.RemoveAll(w.opts.Paths.SpoolRoot(j.id) + "/spool")
		_ = os.RemoveAll(w.opts.Paths.Blobs(j.id))
	}
	wg.Wait()
	_ = os.RemoveAll(w.opts.Paths.ControlsDir())
	_ = os.Remove(w.opts.Paths.Env())
	_ = os.Remove(w.opts.Paths.EnvPrev())
	w.logf("credential revoked: children stopped, credential and spool deleted, worktrees kept")
}

// sockUp reports whether a session is established.
func (w *Worker) sockUp() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.sock != nil
}
