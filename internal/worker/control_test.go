package worker

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	dexec "github.com/more-than-code/defect-drainer-worker/exec"
	"github.com/more-than-code/defect-drainer-worker/workerproto"
)

// fakeGH puts a gh first on PATH that only logs its argv: under
// gittoken.v1 the worker never runs gh (§14), so the log must stay empty.
func fakeGH(t *testing.T) (log string) {
	t.Helper()
	dir := t.TempDir()
	log = fakeGhOnPath(t, dir)
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return log
}

func (h *harness) controlAndWait(jobID, reqID string, c dexec.Control, n int) workerproto.ControlResult {
	h.t.Helper()
	hellos := len(h.fs.frames(workerproto.TypeHello))
	h.fs.control(jobID, reqID, c)
	var r workerproto.ControlResult
	eventually(h.t, 30*time.Second, "control_result "+reqID, func() bool {
		var got int
		r, got = h.fs.controlResult(reqID)
		if got >= n {
			return true
		}
		// The hub's controls are durable: one still unanswered when the
		// socket drops (under load the 1 s test heartbeat can lapse) is
		// redelivered on the next socket, and the worker answers it from its
		// stored result if it already ran.
		if now := len(h.fs.frames(workerproto.TypeHello)); now > hellos && h.fs.connected() && h.w.sockUp() {
			hellos = now
			h.fs.control(jobID, reqID, c)
		}
		return false
	})
	return r
}

// T11, §12.3 and §14: ship_preview, diff (512 KiB cap), ship (push only
// with a write token, once, idempotent by request_id across a socket
// drop, no gh), the refusals, and discard.
func TestControlsAndShipping(t *testing.T) {
	ghLog := fakeGH(t)
	agent := fixArtifacts(testDefect) +
		"head -c 700000 /dev/zero | tr '\\0' 'a' >> ../worktrees/app/README.md\n" +
		"cat '" + claudeFixture(t, "c1-basic") + "'\nexit 0\n"
	h := newHarness(t, harnessOpts{agent: agent})
	id := "bjob_ship"
	h.offerJob(fixSpec(id), nil)
	h.waitDone(id, 60*time.Second)

	r := h.controlAndWait(id, "rq_prev", dexec.Control{Kind: dexec.ControlShipPreview}, 1)
	var prev workerproto.ShipPreviewResult
	if !r.OK || json.Unmarshal(r.Result, &prev) != nil || len(prev.Repos) != 1 || strings.Join(prev.Repos[0].Files, ",") != "README.md" {
		t.Fatalf("ship_preview %+v %s", r, r.Result)
	}

	r = h.controlAndWait(id, "rq_diff", dexec.Control{Kind: dexec.ControlDiff, Repo: "app"}, 1)
	var diff workerproto.DiffResult
	if !r.OK || json.Unmarshal(r.Result, &diff) != nil || !diff.Truncated || len(diff.Diff) > workerproto.MaxDiffBytes || !strings.Contains(diff.Diff, "README.md") {
		t.Fatalf("diff ok=%v truncated=%v len=%d", r.OK, diff.Truncated, len(diff.Diff))
	}
	if r = h.controlAndWait(id, "rq_diff_bad", dexec.Control{Kind: dexec.ControlDiff, Repo: "other"}, 1); r.OK || r.Code != workerproto.DeclineBadSpec {
		t.Fatalf("diff of an unknown repo %+v", r)
	}

	ship := dexec.Control{Kind: dexec.ControlShip, Repos: []dexec.ShipRepo{{Repo: "app", Files: []string{"README.md"}, Message: "Fix the button", Title: "Fix the button", Body: "Batch fix"}}}
	r = h.controlAndWait(id, "rq_ship", ship, 1)
	var res workerproto.ShipPushResult
	if !r.OK || json.Unmarshal(r.Result, &res) != nil || len(res.Repos) != 1 {
		t.Fatalf("ship %+v %s", r, r.Result)
	}
	pushed := res.Repos[0]
	if !pushed.Pushed || pushed.Error != "" || pushed.Repo != "app" || pushed.Branch != "dd/"+id+"/app" || pushed.Base != "main" || !commitRe.MatchString(pushed.HeadSHA) {
		t.Fatalf("ship repo %+v", pushed)
	}
	// The push reached the origin, at head_sha.
	origin := filepath.Join(h.tmp, "origin", "app.git")
	out, err := exec.Command("git", "-C", origin, "log", "--oneline", "dd/"+id+"/app").CombinedOutput()
	if err != nil || !strings.Contains(string(out), "Fix the button") {
		t.Fatalf("branch not pushed: %v %s", err, out)
	}
	if out, _ := exec.Command("git", "-C", origin, "rev-parse", "dd/"+id+"/app").Output(); strings.TrimSpace(string(out)) != pushed.HeadSHA {
		t.Fatalf("origin branch at %s, head_sha %s", out, pushed.HeadSHA)
	}
	writeMints := func() int {
		n := 0
		for _, m := range h.fs.mints() {
			if m.access == workerproto.GitAccessWrite && m.status == 200 {
				n++
			}
		}
		return n
	}
	if n := writeMints(); n != 1 {
		t.Fatalf("write tokens minted: %d", n)
	}
	// The stored result keeps the job busy until control_ack.
	if !contains(h.w.busyIDs(), id) {
		t.Fatal("an unacked control result does not make the job busy")
	}

	// The socket drops before control_ack; the server redelivers the same
	// request_id; the stored result answers and nothing runs twice.
	h.fs.closeCurrent(1000)
	eventually(t, 10*time.Second, "reconnect", func() bool { return h.fs.connected() && h.w.sockUp() })
	r2 := h.controlAndWait(id, "rq_ship", ship, 2)
	if string(r2.Result) != string(r.Result) {
		t.Fatalf("redelivery answered differently:\n%s\n%s", r.Result, r2.Result)
	}
	if n := writeMints(); n != 1 {
		t.Fatalf("redelivered ship minted again: %d write tokens", n)
	}
	h.fs.send(workerproto.ControlAck{Header: workerproto.NewHeader(workerproto.TypeControlAck), JobID: id, RequestID: "rq_ship"})
	eventually(t, 5*time.Second, "stored result deleted", func() bool {
		_, err := os.Stat(h.w.resultPath(id, "rq_ship"))
		return os.IsNotExist(err)
	})

	// A local base is PR-only refused; allow_ship off is policy_denied.
	j := h.w.job(id)
	j.mu.Lock()
	j.meta.Spec.Worktrees[0].BaseSource = "local"
	j.mu.Unlock()
	if r = h.controlAndWait(id, "rq_ship_local", ship, 1); r.OK || r.Code != workerproto.DeclineBadSpec {
		t.Fatalf("local ship %+v", r)
	}
	j.mu.Lock()
	j.meta.Spec.Worktrees[0].BaseSource = "origin"
	j.mu.Unlock()
	writeFile(t, h.paths.Policy(), strings.Replace(defaultPolicy, "allow_ship = true", "allow_ship = false", 1), 0o600)
	h.w.reloadHostFiles()
	if r = h.controlAndWait(id, "rq_ship_denied", ship, 1); r.OK || r.Code != workerproto.ReasonPolicyDenied {
		t.Fatalf("ship without allow_ship %+v", r)
	}
	if n := writeMints(); n != 1 {
		t.Fatalf("a refused ship minted a write token: %d", n)
	}
	if b, err := os.ReadFile(ghLog); err == nil {
		t.Fatalf("gh ran: %s", b)
	}

	// Discard removes the worktree and the job dir; a redelivery is
	// answered from the stored result.
	r = h.controlAndWait(id, "rq_discard", dexec.Control{Kind: dexec.ControlDiscard}, 1)
	if !r.OK {
		t.Fatalf("discard %+v", r)
	}
	if _, err := os.Stat(h.paths.JobDir(id)); !os.IsNotExist(err) {
		t.Fatal("job dir kept after discard")
	}
	if r = h.controlAndWait(id, "rq_discard", dexec.Control{Kind: dexec.ControlDiscard}, 2); !r.OK {
		t.Fatalf("discard redelivery %+v", r)
	}
	if r = h.controlAndWait(id, "rq_diff_gone", dexec.Control{Kind: dexec.ControlDiff, Repo: "app"}, 1); r.OK {
		t.Fatal("diff answered for a discarded job")
	}
}

// An unknown control kind is a bad frame, never a stored result.
func TestUnknownControlKind(t *testing.T) {
	h := newHarness(t, harnessOpts{})
	h.fs.control("bjob_x", "rq_weird", dexec.Control{Kind: "reboot"})
	eventually(t, 5*time.Second, "bad_frame", func() bool {
		for _, f := range h.fs.frames(workerproto.TypeError) {
			if mustDecode[workerproto.Error](h.fs, f.raw).Code == workerproto.CodeBadFrame {
				return true
			}
		}
		return false
	})
	if _, n := h.fs.controlResult("rq_weird"); n != 0 {
		t.Fatal("a result was sent for an unknown kind")
	}
}

func contains(xs []string, x string) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}

// Ship of route files (the live Create PR that hung): the control with
// SvelteKit names passes, and git adds the listed paths literally, so
// [id] never globs onto a sibling such as i/.
func TestShipRouteFiles(t *testing.T) {
	agent := fixArtifacts(testDefect) +
		"mkdir -p '../worktrees/app/src/routes/[id]' ../worktrees/app/src/routes/i '../worktrees/app/src/routes/(app)'\n" +
		"echo fix > '../worktrees/app/src/routes/[id]/+page.svelte'\n" +
		"echo layout > '../worktrees/app/src/routes/(app)/+layout.svelte'\n" +
		"echo decoy > ../worktrees/app/src/routes/i/+page.svelte\n" +
		"cat '" + claudeFixture(t, "c1-basic") + "'\nexit 0\n"
	h := newHarness(t, harnessOpts{agent: agent})
	id := "bjob_ship_routes"
	h.offerJob(fixSpec(id), nil)
	h.waitDone(id, 60*time.Second)
	files := []string{"src/routes/[id]/+page.svelte", "src/routes/(app)/+layout.svelte"}
	ship := dexec.Control{Kind: dexec.ControlShip, Repos: []dexec.ShipRepo{{Repo: "app", Files: files, Message: "Fix the route", Title: "Fix the route"}}}
	r := h.controlAndWait(id, "rq_ship_routes", ship, 1)
	var res workerproto.ShipPushResult
	if !r.OK || json.Unmarshal(r.Result, &res) != nil || len(res.Repos) != 1 || !res.Repos[0].Pushed || res.Repos[0].Error != "" {
		t.Fatalf("ship %+v %s", r, r.Result)
	}
	origin := filepath.Join(h.tmp, "origin", "app.git")
	out, err := exec.Command("git", "-C", origin, "show", "--name-only", "--format=", "dd/"+id+"/app").CombinedOutput()
	if err != nil || strings.Join(strings.Fields(string(out)), ",") != "src/routes/(app)/+layout.svelte,src/routes/[id]/+page.svelte" {
		t.Fatalf("pushed files: %v %q", err, out)
	}
}
