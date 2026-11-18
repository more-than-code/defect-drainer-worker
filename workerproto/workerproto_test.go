package workerproto

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	dexec "github.com/more-than-code/defect-drainer-worker/exec"
)

// The close codes are the protocol's numbers (§1).
func TestCloseCodes(t *testing.T) {
	for got, want := range map[int]int{int(CloseNormal): 1000, int(ClosePolicy): 1008, int(CloseTooBig): 1009,
		int(CloseReplaced): 4000, int(CloseRevoked): 4001, int(CloseProtocolUnsupported): 4002, int(CloseOrgMismatch): 4003} {
		if got != want {
			t.Errorf("close code %d, want %d", got, want)
		}
	}
}

func roundTrip[T any](t *testing.T, in T) T {
	t.Helper()
	b, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	if len(b) > MaxFrameBytes {
		t.Fatalf("fixture frame is %d bytes", len(b))
	}
	var out T
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(in, out) {
		t.Fatalf("round trip changed the frame:\n in %+v\nout %+v", in, out)
	}
	return out
}

func TestFramesRoundTripAndValidate(t *testing.T) {
	ev := dexec.Event{Kind: dexec.EventLog, JobID: "bjob_1", Lines: []dexec.LogLine{{Text: "x", Level: "info", Source: "Agent"}}}
	frames := []interface{ Validate() error }{
		Enroll{Header: NewHeader(TypeEnroll), Hostname: "worker-1", MachineIDHash: strings.Repeat("a", 64), Version: "0.1.0", OS: "linux", Arch: "arm64"},
		Hello{Header: NewHeader(TypeHello), WorkerID: "wrk_1", Version: "0.1.0", Protocol: 1, Capabilities: []string{CapBlobsV1}, MaxJobs: 1,
			BusyJobIDs: []string{"bjob_1"}, Adapters: []AdapterStatus{{Name: "claude", Version: "2.1.292", CredentialMode: "api_key", Ready: true, MinOK: true}},
			Policy: Policy{Valid: true, Enabled: true, Orgs: []string{"org_1"}, Adapters: []string{"claude"}, Repos: []string{"https://github.com/o/*"}, MinSandbox: "strict", MaxTurns: 200, MaxWallSec: 2700}},
		Offer{Header: NewHeader(TypeOffer), JobID: "bjob_1", LeaseID: "ls_1", LeaseUntil: "2026-11-15T00:00:00Z", Adapter: "claude", Mode: "fresh", SpecURL: "/worker/v1/jobs/bjob_1/spec", JobToken: PrefixJobToken + "x"},
		Accept{Header: NewHeader(TypeAccept), JobID: "bjob_1", LeaseID: "ls_1"},
		Decline{Header: NewHeader(TypeDecline), JobID: "bjob_1", LeaseID: "ls_1", Reason: DeclineBusy},
		Progress{Header: NewHeader(TypeProgress), JobID: "bjob_1", LeaseID: "ls_1", Seq: 1, Events: []dexec.Event{ev}},
		Ack{Header: NewHeader(TypeAck), JobID: "bjob_1", Seq: 1},
		Control{Header: NewHeader(TypeControl), JobID: "bjob_1", LeaseID: "ls_1", RequestID: "rq_1",
			Control: dexec.Control{Kind: dexec.ControlShip, JobID: "bjob_1", RequestID: "rq_1", Repos: []dexec.ShipRepo{{Repo: "webapp", Files: []string{"src/a.ts"}, Message: "m", Title: "t", Body: "b"}}}},
		ControlResult{Header: NewHeader(TypeControlResult), JobID: "bjob_1", RequestID: "rq_1", OK: true, Result: json.RawMessage(`{"prs":[]}`)},
	}
	for _, f := range frames {
		if err := f.Validate(); err != nil {
			t.Errorf("%T: %v", f, err)
		}
		switch v := f.(type) {
		case Enroll:
			roundTrip(t, v)
		case Hello:
			roundTrip(t, v)
		case Offer:
			roundTrip(t, v)
		case Accept:
			roundTrip(t, v)
		case Decline:
			roundTrip(t, v)
		case Progress:
			roundTrip(t, v)
		case Ack:
			roundTrip(t, v)
		case Control:
			roundTrip(t, v)
		case ControlResult:
			roundTrip(t, v)
		}
	}
}

// §2: unknown keys are ignored, required keys are checked.
func TestLenientDecodeStrictRequired(t *testing.T) {
	var h Hello
	raw := `{"v":1,"type":"hello","worker_id":"wrk_1","protocol":2,"max_jobs":1,"future":{"x":1},"adapters":[{"name":"claude","ready":true,"new_field":1}]}`
	if err := json.Unmarshal([]byte(raw), &h); err != nil {
		t.Fatal(err)
	}
	if err := h.Validate(); err != nil {
		t.Fatalf("a hello with unknown keys must validate: %v", err)
	}
	var missing Hello
	_ = json.Unmarshal([]byte(`{"v":1,"type":"hello","protocol":1,"max_jobs":1}`), &missing)
	if missing.Validate() == nil {
		t.Fatal("a hello without worker_id must fail")
	}
	var env Envelope
	if err := json.Unmarshal([]byte(`{"v":1,"type":"something_new","a":1}`), &env); err != nil || env.Type != "something_new" {
		t.Fatalf("envelope: %+v %v", env, err)
	}
}

// §2 and the review's path-traversal finding.
func TestValidators(t *testing.T) {
	for _, id := range []string{"bjob_1", "ls-1", "A"} {
		if !ValidID(id) {
			t.Errorf("ValidID(%q) = false", id)
		}
	}
	for _, id := range []string{"", ".", "..", "a/b", "a.b", "a:b", "-a", strings.Repeat("a", 129)} {
		if ValidID(id) {
			t.Errorf("ValidID(%q) = true", id)
		}
	}
	for _, p := range []string{"a.png", "src/a.ts", "DEF-1/shot.png"} {
		if !ValidRelPath(p) {
			t.Errorf("ValidRelPath(%q) = false", p)
		}
	}
	for _, p := range []string{"", "/etc/passwd", "../x", "a/../b", "a/./b", "a//b", "a\\b", "a\x00b", "a b", "~/x"} {
		if ValidRelPath(p) {
			t.Errorf("ValidRelPath(%q) = true", p)
		}
	}
	if !ValidSHA256(strings.Repeat("0", 64)) || ValidSHA256(strings.Repeat("A", 64)) || ValidSHA256("abc") {
		t.Error("ValidSHA256")
	}
	for _, m := range []string{"", "sonnet", "claude-sonnet-5-5", "org/model.v1"} {
		if !ValidModel(m) {
			t.Errorf("ValidModel(%q) = false", m)
		}
	}
	for _, m := range []string{"-p", "--dangerously-skip-permissions", "a b", "a;b"} {
		if ValidModel(m) {
			t.Errorf("ValidModel(%q) = true", m)
		}
	}
	bad := Control{Header: NewHeader(TypeControl), JobID: "bjob_1", RequestID: "rq_1",
		Control: dexec.Control{Kind: dexec.ControlShip, JobID: "bjob_1", Repos: []dexec.ShipRepo{{Repo: "webapp", Files: []string{"../../.ssh/id_rsa"}}}}}
	if bad.Validate() == nil {
		t.Fatal("a ship file escaping the worktree must fail")
	}
	mismatch := Progress{Header: NewHeader(TypeProgress), JobID: "bjob_1", LeaseID: "ls_1", Seq: 1, Events: []dexec.Event{{Kind: dexec.EventLog, JobID: "bjob_2"}}}
	if mismatch.Validate() == nil {
		t.Fatal("an event for another job must fail")
	}
}
