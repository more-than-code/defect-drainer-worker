package worker

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/more-than-code/defect-drainer-worker/exec"
	"github.com/more-than-code/defect-drainer-worker/workerproto"
)

// fakeServer is the server side of worker protocol v1 for tests, built on
// internal/workerproto. Every frame the worker sends is decoded and must
// pass its Validate (a failure fails the test). It applies progress the way
// the contract says the hub does: cumulative acks after "commit", re-ack of
// a seq ≤ last, seq_gap on a gap, blob_missing on an unknown hash, and no
// later frame of a job applied after a rejection until the resend.
type fakeServer struct {
	t   *testing.T
	srv *httptest.Server

	mu         sync.Mutex
	creds      map[string]bool
	joinTokens map[string]bool
	workerID   string
	orgID      string
	enrollCred string
	heartbeat  int

	// Behaviour switches.
	redirect      bool
	withholdAcks  bool
	helloClose    websocket.StatusCode // close the socket with this code after hello
	ignoreRotAck  bool                 // the rotate_ack is "lost"
	dropProgress  bool                 // progress frames are "lost" in transit
	dropSeqOnce   map[int64]bool       // these seqs are lost once
	dropPuts      int                  // this many blob PUTs answer 201 but are "lost"
	noReattach    bool                 // do not send lease frames after hello
	silent        bool                 // stop answering on existing sockets (a dead link)
	pendingRot    []string
	noGitTokenCap bool   // hello_ok without gittoken.v1
	targetRelease string // hello_ok.target_release
	gitTokenFail  int    // git-token answers this status instead of minting

	// gitMints records every git-token call (§14); pendingShips is the
	// ship controls not yet answered, by request id → job and repos.
	gitMints     []gitMint
	pendingShips map[string]pendingShip

	jobs      map[string]*fakeJob
	jobTokens map[string]string // token → job id
	recv      []rx
	hellos    []workerproto.Hello
	conns     []*websocket.Conn
	cur       *websocket.Conn
	upgrades  int
	unauth    int
	putCount  int
}

type rx struct {
	typ string
	raw []byte
	at  time.Time
}

type fakeJob struct {
	spec       exec.JobSpec
	rawSpec    []byte // served instead of spec when set (an older server's keys)
	leaseID    string
	lastSeq    int64
	blocked    bool  // a rejection is outstanding
	expect     int64 // seq the server waits for after a rejection
	events     []exec.Event
	evidence   map[string][]byte // sha → bytes the spec references
	blobs      map[string][]byte // PUT blobs
	blobTypes  map[string]string
	putOrder   map[string]int // sha → put counter
	appliedAt  map[string]int // sha → put counter at the time an event named it
	ackedDone  bool
	frames     int // progress frames applied
	duplicates int // progress frames re-acked
}

func newFakeServer(t *testing.T) *fakeServer {
	t.Helper()
	fs := &fakeServer{
		t: t, creds: map[string]bool{}, joinTokens: map[string]bool{},
		workerID: "wrk_test1", orgID: "org_test1", heartbeat: 1,
		jobs: map[string]*fakeJob{}, jobTokens: map[string]string{}, dropSeqOnce: map[int64]bool{},
		pendingShips: map[string]pendingShip{},
	}
	mux := http.NewServeMux()
	mux.HandleFunc(workerproto.PathSocket, fs.ws)
	mux.HandleFunc(workerproto.PathPrefix+"jobs/", fs.side)
	fs.srv = httptest.NewServer(mux)
	t.Cleanup(fs.srv.Close)
	return fs
}

func (fs *fakeServer) URL() string { return fs.srv.URL }

func bearer(r *http.Request) string {
	return strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
}

func (fs *fakeServer) ws(w http.ResponseWriter, r *http.Request) {
	fs.mu.Lock()
	redirect := fs.redirect
	fs.mu.Unlock()
	if redirect {
		http.Redirect(w, r, "https://elsewhere.invalid"+workerproto.PathSocket, http.StatusFound)
		return
	}
	tok := bearer(r)
	fs.mu.Lock()
	switch {
	case strings.HasPrefix(tok, workerproto.PrefixJoinToken):
		if !fs.joinTokens[tok] {
			fs.mu.Unlock()
			c, err := websocket.Accept(w, r, nil)
			if err == nil {
				c.Close(workerproto.ClosePolicy, "bad join token")
			}
			return
		}
		delete(fs.joinTokens, tok)
		fs.mu.Unlock()
		fs.enroll(w, r)
		return
	case !fs.creds[tok]:
		fs.unauth++
		fs.mu.Unlock()
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	fs.upgrades++
	fs.mu.Unlock()
	c, err := websocket.Accept(w, r, nil)
	if err != nil {
		return
	}
	c.SetReadLimit(workerproto.MaxFrameBytes)
	fs.serve(r.Context(), c, tok)
}

func (fs *fakeServer) enroll(w http.ResponseWriter, r *http.Request) {
	c, err := websocket.Accept(w, r, nil)
	if err != nil {
		return
	}
	ctx := r.Context()
	_, data, err := c.Read(ctx)
	if err != nil {
		return
	}
	var f workerproto.Enroll
	if err := json.Unmarshal(data, &f); err != nil || f.Validate() != nil {
		fs.t.Errorf("enroll frame invalid: %s", data)
		c.Close(workerproto.ClosePolicy, "")
		return
	}
	fs.record(workerproto.TypeEnroll, data)
	fs.mu.Lock()
	cred := fs.enrollCred
	fs.creds[cred] = true
	fs.mu.Unlock()
	b, _ := json.Marshal(workerproto.Enrolled{Header: workerproto.NewHeader(workerproto.TypeEnrolled), WorkerID: fs.workerID, OrgID: fs.orgID, Credential: cred})
	_ = c.Write(ctx, websocket.MessageText, b)
	c.Close(websocket.StatusNormalClosure, "")
}

func (fs *fakeServer) record(typ string, raw []byte) {
	fs.mu.Lock()
	fs.recv = append(fs.recv, rx{typ: typ, raw: append([]byte(nil), raw...), at: time.Now()})
	fs.mu.Unlock()
}

// serve is one authenticated socket.
func (fs *fakeServer) serve(ctx context.Context, c *websocket.Conn, cred string) {
	fs.mu.Lock()
	if fs.cur != nil {
		fs.cur.Close(workerproto.CloseReplaced, "replaced")
	}
	fs.cur = c
	fs.conns = append(fs.conns, c)
	fs.mu.Unlock()
	defer func() {
		fs.mu.Lock()
		if fs.cur == c {
			fs.cur = nil
		}
		fs.mu.Unlock()
	}()
	for {
		_, data, err := c.Read(ctx)
		if err != nil {
			return
		}
		var env workerproto.Envelope
		if err := json.Unmarshal(data, &env); err != nil || env.V != workerproto.Version {
			fs.t.Errorf("worker sent a frame without a v1 envelope: %s", data)
			continue
		}
		fs.record(env.Type, data)
		fs.mu.Lock()
		silent := fs.silent && fs.cur != c
		fs.mu.Unlock()
		if silent {
			continue
		}
		fs.handle(ctx, c, env.Type, data)
	}
}

func mustDecode[T any](fs *fakeServer, data []byte) T {
	var v T
	if err := json.Unmarshal(data, &v); err != nil {
		fs.t.Errorf("decode %T: %v: %s", v, err, data)
	}
	return v
}

func (fs *fakeServer) handle(ctx context.Context, c *websocket.Conn, typ string, data []byte) {
	switch typ {
	case workerproto.TypeHello:
		f := mustDecode[workerproto.Hello](fs, data)
		if err := f.Validate(); err != nil {
			fs.t.Errorf("hello invalid: %v", err)
		}
		if len(f.Capabilities) == 0 || f.Capabilities[0] != workerproto.CapBlobsV1 {
			fs.t.Errorf("hello without blobs.v1: %v", f.Capabilities)
		}
		fs.mu.Lock()
		fs.hellos = append(fs.hellos, f)
		closeCode := fs.helloClose
		hb := fs.heartbeat
		fs.mu.Unlock()
		if closeCode != 0 {
			c.Close(closeCode, "scripted")
			return
		}
		caps := []string{workerproto.CapBlobsV1, workerproto.CapGitTokenV1}
		fs.mu.Lock()
		if fs.noGitTokenCap {
			caps = caps[:1]
		}
		target := fs.targetRelease
		fs.mu.Unlock()
		fs.write(c, workerproto.HelloOK{Header: workerproto.NewHeader(workerproto.TypeHelloOK), Protocol: 1, HeartbeatSec: hb,
			ServerTime: time.Now().UTC().Format(time.RFC3339), Capabilities: caps, TargetRelease: target})
		// §5.3 re-attach: a lease frame, same lease id, fresh token, for each
		// busy job this server knows; lease_lost for the others.
		fs.mu.Lock()
		reattach := !fs.noReattach
		var leases []workerproto.Lease
		var lost []string
		for _, id := range f.BusyJobIDs {
			if j := fs.jobs[id]; j != nil && j.leaseID != "" {
				tok := "dd_wjt_re_" + id
				fs.jobTokens[tok] = id
				leases = append(leases, workerproto.Lease{Header: workerproto.NewHeader(workerproto.TypeLease), JobID: id, LeaseID: j.leaseID,
					LeaseUntil: time.Now().Add(2 * time.Minute).UTC().Format(time.RFC3339), JobToken: tok})
			} else {
				lost = append(lost, id)
			}
		}
		fs.mu.Unlock()
		if reattach {
			for _, l := range leases {
				fs.write(c, l)
			}
			for _, id := range lost {
				fs.write(c, workerproto.Error{Header: workerproto.NewHeader(workerproto.TypeError), Code: workerproto.CodeLeaseLost, JobID: id})
			}
		}
	case workerproto.TypePing:
		_ = mustDecode[workerproto.Ping](fs, data)
		fs.write(c, workerproto.Pong{Header: workerproto.NewHeader(workerproto.TypePong)})
	case workerproto.TypeStatus:
		f := mustDecode[workerproto.Status](fs, data)
		if len(f.Adapters) > workerproto.MaxAdapters || f.MaxJobs < 1 {
			fs.t.Errorf("status invalid: %s", data)
		}
	case workerproto.TypeAccept:
		f := mustDecode[workerproto.Accept](fs, data)
		if err := f.Validate(); err != nil {
			fs.t.Errorf("accept invalid: %v", err)
		}
	case workerproto.TypeDecline:
		f := mustDecode[workerproto.Decline](fs, data)
		if err := f.Validate(); err != nil {
			fs.t.Errorf("decline invalid: %v", err)
		}
		switch f.Reason {
		case workerproto.DeclineBusy, workerproto.DeclineAdapterUnavailable, workerproto.DeclinePolicyDenied,
			workerproto.DeclineSecretMissing, workerproto.DeclineBadSpec, workerproto.DeclineInternal,
			workerproto.DeclineModelNotAllowed:
		default:
			fs.t.Errorf("decline reason %q not in §5.2", f.Reason)
		}
	case workerproto.TypeProgress:
		f := mustDecode[workerproto.Progress](fs, data)
		if err := f.Validate(); err != nil {
			fs.t.Errorf("progress invalid: %v", err)
			return
		}
		if len(data) > workerproto.MaxFrameBytes {
			fs.t.Errorf("progress frame %d bytes", len(data))
		}
		for _, e := range f.Events {
			if b, _ := json.Marshal(e); len(b) > workerproto.MaxEventBytes {
				fs.t.Errorf("event %d bytes", len(b))
			}
		}
		fs.mu.Lock()
		drop := fs.dropProgress || fs.dropSeqOnce[f.Seq]
		delete(fs.dropSeqOnce, f.Seq)
		fs.mu.Unlock()
		if !drop {
			fs.applyProgress(c, f)
		}
	case workerproto.TypeControlResult:
		f := mustDecode[workerproto.ControlResult](fs, data)
		if err := f.Validate(); err != nil {
			fs.t.Errorf("control_result invalid: %v", err)
		}
		fs.mu.Lock()
		delete(fs.pendingShips, f.RequestID)
		fs.mu.Unlock()
	case workerproto.TypeRotateAck:
		fs.mu.Lock()
		if fs.ignoreRotAck {
			fs.mu.Unlock()
			return
		}
		// Every credential older than the newest pending one is revoked.
		if n := len(fs.pendingRot); n > 0 {
			newest := fs.pendingRot[n-1]
			for k := range fs.creds {
				if k != newest {
					delete(fs.creds, k)
				}
			}
			fs.pendingRot = nil
		}
		fs.mu.Unlock()
	case workerproto.TypeError:
		_ = mustDecode[workerproto.Error](fs, data)
	default:
		fs.t.Errorf("worker sent unknown frame type %q", typ)
	}
}

func (fs *fakeServer) write(c *websocket.Conn, v any) {
	b, err := json.Marshal(v)
	if err != nil {
		fs.t.Errorf("marshal: %v", err)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = c.Write(ctx, websocket.MessageText, b)
}

// send writes v on the current socket.
func (fs *fakeServer) send(v any) {
	fs.mu.Lock()
	c := fs.cur
	fs.mu.Unlock()
	if c == nil {
		fs.t.Errorf("send %T with no socket", v)
		return
	}
	fs.write(c, v)
}

func (fs *fakeServer) sendRaw(s string) {
	fs.mu.Lock()
	c := fs.cur
	fs.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = c.Write(ctx, websocket.MessageText, []byte(s))
}

func (fs *fakeServer) applyProgress(c *websocket.Conn, f workerproto.Progress) {
	fs.mu.Lock()
	j := fs.jobs[f.JobID]
	if j == nil || j.leaseID != f.LeaseID {
		fs.mu.Unlock()
		fs.write(c, workerproto.Error{Header: workerproto.NewHeader(workerproto.TypeError), Code: workerproto.CodeLeaseLost, JobID: f.JobID, LeaseID: f.LeaseID})
		return
	}
	if f.Seq <= j.lastSeq {
		j.duplicates++
		seq := j.lastSeq
		withhold := fs.withholdAcks
		fs.mu.Unlock()
		if !withhold {
			fs.write(c, workerproto.Ack{Header: workerproto.NewHeader(workerproto.TypeAck), JobID: f.JobID, Seq: seq})
		}
		return
	}
	if j.blocked && f.Seq != j.expect {
		fs.mu.Unlock()
		return
	}
	if f.Seq != j.lastSeq+1 {
		j.blocked, j.expect = true, j.lastSeq+1
		fs.mu.Unlock()
		fs.write(c, workerproto.Error{Header: workerproto.NewHeader(workerproto.TypeError), Code: workerproto.CodeSeqGap, JobID: f.JobID, Expected: j.lastSeq + 1})
		return
	}
	for _, sha := range blobRefs(f.Events) {
		if _, ok := j.blobs[sha]; !ok {
			j.blocked, j.expect = true, f.Seq
			fs.mu.Unlock()
			fs.write(c, workerproto.Error{Header: workerproto.NewHeader(workerproto.TypeError), Code: workerproto.CodeBlobMissing, JobID: f.JobID, SHA256: sha})
			return
		}
		j.appliedAt[sha] = fs.putCount
	}
	j.blocked = false
	j.events = append(j.events, f.Events...)
	j.lastSeq = f.Seq
	j.frames++
	for _, e := range f.Events {
		if e.Kind == exec.EventDone {
			j.ackedDone = true
		}
	}
	withhold := fs.withholdAcks
	fs.mu.Unlock()
	if !withhold {
		fs.write(c, workerproto.Ack{Header: workerproto.NewHeader(workerproto.TypeAck), JobID: f.JobID, Seq: f.Seq})
	}
}

func blobRefs(events []exec.Event) []string {
	var out []string
	for _, e := range events {
		if e.Harvest != nil {
			for _, d := range e.Harvest.Defects {
				for _, im := range d.Images {
					out = append(out, im.SHA256)
				}
				if d.NoteSHA256 != "" {
					out = append(out, d.NoteSHA256)
				}
			}
		}
		if e.TestResults != nil {
			for _, s := range e.TestResults.Steps {
				for _, im := range s.Images {
					out = append(out, im.SHA256)
				}
			}
		}
	}
	return out
}

// side is the HTTPS side channel (§7).
func (fs *fakeServer) side(w http.ResponseWriter, r *http.Request) {
	fs.mu.Lock()
	redirect := fs.redirect
	fs.mu.Unlock()
	if redirect {
		http.Redirect(w, r, "https://elsewhere.invalid"+r.URL.Path, http.StatusFound)
		return
	}
	rest := strings.TrimPrefix(r.URL.Path, workerproto.PathPrefix+"jobs/")
	parts := strings.Split(rest, "/")
	fs.mu.Lock()
	jobID := fs.jobTokens[bearer(r)]
	j := fs.jobs[jobID]
	fs.mu.Unlock()
	if j == nil || len(parts) < 2 || parts[0] != jobID {
		http.NotFound(w, r)
		return
	}
	switch {
	case parts[1] == strings.TrimPrefix(workerproto.GitTokenPathSuffix, "/") && len(parts) == 2 && r.Method == http.MethodPost:
		fs.gitToken(w, r, jobID, j)
	case parts[1] == "spec" && r.Method == http.MethodGet:
		fs.mu.Lock()
		b, _ := json.Marshal(j.spec)
		if j.rawSpec != nil {
			b = j.rawSpec
		}
		fs.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.Write(b)
	case parts[1] == "blobs" && len(parts) == 3 && r.Method == http.MethodGet:
		fs.mu.Lock()
		b, ok := j.evidence[parts[2]]
		fs.mu.Unlock()
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Write(b)
	case parts[1] == "blobs" && len(parts) == 3 && r.Method == http.MethodPut:
		body, _ := io.ReadAll(io.LimitReader(r.Body, workerproto.MaxBlobBytesPerJob))
		ct := r.Header.Get("Content-Type")
		allowed := false
		for _, a := range workerproto.AllowedBlobTypes {
			if a == ct {
				allowed = true
			}
		}
		sniff := http.DetectContentType(body)
		if !allowed || !strings.HasPrefix(sniff, ct) {
			http.Error(w, "type", http.StatusUnsupportedMediaType)
			return
		}
		if hashHex(body) != parts[2] {
			http.Error(w, "hash_mismatch", http.StatusBadRequest)
			return
		}
		fs.mu.Lock()
		if fs.dropPuts > 0 {
			fs.dropPuts--
			fs.mu.Unlock()
			w.WriteHeader(http.StatusCreated)
			return
		}
		fs.putCount++
		j.blobs[parts[2]] = body
		j.blobTypes[parts[2]] = ct
		j.putOrder[parts[2]] = fs.putCount
		fs.mu.Unlock()
		w.WriteHeader(http.StatusCreated)
	default:
		http.NotFound(w, r)
	}
}

// addJob registers a job's spec and evidence; offer sends the offer.
func (fs *fakeServer) addJob(spec exec.JobSpec, evidence map[string][]byte) *fakeJob {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	j := &fakeJob{spec: spec, evidence: evidence, blobs: map[string][]byte{}, blobTypes: map[string]string{},
		putOrder: map[string]int{}, appliedAt: map[string]int{}}
	if j.evidence == nil {
		j.evidence = map[string][]byte{}
	}
	fs.jobs[spec.JobID] = j
	return j
}

func (fs *fakeServer) offer(jobID, leaseID, token, adapter string) {
	fs.mu.Lock()
	fs.jobs[jobID].leaseID = leaseID
	fs.jobTokens[token] = jobID
	fs.mu.Unlock()
	fs.send(workerproto.Offer{Header: workerproto.NewHeader(workerproto.TypeOffer), JobID: jobID, LeaseID: leaseID,
		LeaseUntil: time.Now().Add(2 * time.Minute).UTC().Format(time.RFC3339), Adapter: adapter,
		Mode: exec.ModeFresh, SpecURL: workerproto.PathPrefix + "jobs/" + jobID + "/spec", JobToken: token})
}

// --- waiting and inspection ---

func (fs *fakeServer) frames(typ string) []rx {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	var out []rx
	for _, f := range fs.recv {
		if f.typ == typ {
			out = append(out, f)
		}
	}
	return out
}

func (fs *fakeServer) jobEvents(id string) []exec.Event {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	if j := fs.jobs[id]; j != nil {
		return append([]exec.Event(nil), j.events...)
	}
	return nil
}

func (fs *fakeServer) done(id string) *exec.Done {
	for _, e := range fs.jobEvents(id) {
		if e.Kind == exec.EventDone {
			return e.Done
		}
	}
	return nil
}

func (fs *fakeServer) closeCurrent(code websocket.StatusCode) {
	fs.mu.Lock()
	c := fs.cur
	fs.cur = nil
	fs.mu.Unlock()
	if c != nil {
		c.Close(code, "scripted")
	}
}

func (fs *fakeServer) connected() bool {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	return fs.cur != nil
}

// eventually polls cond until it holds or the timeout ends.
func eventually(t *testing.T, timeout time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	// A timeout here has been a hang (a lock-order deadlock) as often as a
	// slow step: the stacks tell them apart.
	buf := make([]byte, 1<<22)
	t.Logf("goroutines at the timeout:\n%s", buf[:runtime.Stack(buf, true)])
	t.Fatalf("timed out waiting for %s", what)
}

// declines returns the decline reasons received for jobID.
func (fs *fakeServer) declineFor(jobID string) string {
	for _, f := range fs.frames(workerproto.TypeDecline) {
		d := mustDecode[workerproto.Decline](fs, f.raw)
		if d.JobID == jobID {
			return d.Reason
		}
	}
	return ""
}

func (fs *fakeServer) accepted(jobID string) bool {
	for _, f := range fs.frames(workerproto.TypeAccept) {
		if mustDecode[workerproto.Accept](fs, f.raw).JobID == jobID {
			return true
		}
	}
	return false
}

func (fs *fakeServer) controlResult(reqID string) (workerproto.ControlResult, int) {
	var last workerproto.ControlResult
	n := 0
	for _, f := range fs.frames(workerproto.TypeControlResult) {
		r := mustDecode[workerproto.ControlResult](fs, f.raw)
		if r.RequestID == reqID {
			last = r
			n++
		}
	}
	return last, n
}

// offerMode offers jobID with mode and a new lease (a rerun).
func (fs *fakeServer) offerMode(jobID, leaseID, token, mode string) {
	fs.mu.Lock()
	fs.jobs[jobID].leaseID = leaseID
	fs.jobs[jobID].spec.Mode = mode
	fs.jobTokens[token] = jobID
	fs.mu.Unlock()
	fs.send(workerproto.Offer{Header: workerproto.NewHeader(workerproto.TypeOffer), JobID: jobID, LeaseID: leaseID,
		LeaseUntil: time.Now().Add(2 * time.Minute).UTC().Format(time.RFC3339), Mode: mode,
		SpecURL: workerproto.PathPrefix + "jobs/" + jobID + "/spec", JobToken: token})
}

// control sends a control frame with a fresh job token.
func (fs *fakeServer) control(jobID, reqID string, c exec.Control) {
	fs.mu.Lock()
	j := fs.jobs[jobID]
	lease := ""
	if j != nil {
		lease = j.leaseID
	}
	tok := "dd_wjt_ctl_" + reqID
	fs.jobTokens[tok] = jobID
	if c.Kind == exec.ControlShip {
		ps := pendingShip{jobID: jobID, repos: map[string]bool{}}
		for _, r := range c.Repos {
			ps.repos[r.Repo] = true
		}
		fs.pendingShips[reqID] = ps
	}
	fs.mu.Unlock()
	c.JobID = jobID
	fs.send(workerproto.Control{Header: workerproto.NewHeader(workerproto.TypeControl), JobID: jobID, LeaseID: lease,
		RequestID: reqID, Control: c, JobToken: tok})
}

// lease reactivates jobID's lease with a fresh token (a Resume, §6.3).
func (fs *fakeServer) lease(jobID string) {
	fs.mu.Lock()
	j := fs.jobs[jobID]
	tok := "dd_wjt_lease_" + jobID + time.Now().Format("150405.000000")
	tok = strings.NewReplacer(".", "_").Replace(tok)
	fs.jobTokens[tok] = jobID
	lease := j.leaseID
	fs.mu.Unlock()
	fs.send(workerproto.Lease{Header: workerproto.NewHeader(workerproto.TypeLease), JobID: jobID, LeaseID: lease,
		LeaseUntil: time.Now().Add(2 * time.Minute).UTC().Format(time.RFC3339), JobToken: tok})
}

// seqsApplied are the frame seqs applied for jobID since mark events.
func (fs *fakeServer) progressSeqs(jobID string) []int64 {
	var out []int64
	for _, f := range fs.frames(workerproto.TypeProgress) {
		p := mustDecode[workerproto.Progress](fs, f.raw)
		if p.JobID == jobID {
			out = append(out, p.Seq)
		}
	}
	return out
}

func (fs *fakeServer) lastSeq(jobID string) int64 {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	return fs.jobs[jobID].lastSeq
}

type gitMint struct {
	jobID, repo, access, bearer, token string
	status                             int
}

type pendingShip struct {
	jobID string
	repos map[string]bool
}

// gitToken is the fake §14 mint: the repo must be one of the job's, write
// only while a ship naming it is unanswered and never for a test run. The
// token is ghs_fake_<n>, an hour long, with the repo's canonical URL.
func (fs *fakeServer) gitToken(w http.ResponseWriter, r *http.Request, jobID string, j *fakeJob) {
	var req workerproto.GitTokenRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&req); err != nil {
		http.Error(w, "bad body", http.StatusBadRequest)
		return
	}
	fs.mu.Lock()
	defer fs.mu.Unlock()
	m := gitMint{jobID: jobID, repo: req.Repo, access: req.Access, bearer: bearer(r)}
	url := ""
	for _, b := range j.spec.Worktrees {
		if b.Repo == req.Repo {
			url, _ = CanonicalRepoURL(b.RepoURL)
		}
	}
	shipping := false
	for _, ps := range fs.pendingShips {
		if ps.jobID == jobID && ps.repos[req.Repo] {
			shipping = true
		}
	}
	switch {
	case fs.gitTokenFail != 0:
		m.status = fs.gitTokenFail
	case url == "":
		m.status = http.StatusForbidden
	case req.Access == workerproto.GitAccessWrite && (!shipping || j.spec.Kind == exec.KindTestRun):
		m.status = http.StatusForbidden
	case req.Access != workerproto.GitAccessRead && req.Access != workerproto.GitAccessWrite:
		m.status = http.StatusBadRequest
	default:
		m.status = http.StatusOK
		m.token = fmt.Sprintf("ghs_fake%04dTOKENVALUE", len(fs.gitMints)+1)
	}
	fs.gitMints = append(fs.gitMints, m)
	if m.status != http.StatusOK {
		http.Error(w, http.StatusText(m.status), m.status)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	json.NewEncoder(w).Encode(workerproto.GitTokenResponse{Token: m.token,
		ExpiresAt: time.Now().Add(time.Hour).UTC().Format(time.RFC3339), URL: url})
}

// mints returns the git-token calls so far.
func (fs *fakeServer) mints() []gitMint {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	return append([]gitMint(nil), fs.gitMints...)
}
