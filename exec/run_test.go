package exec

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/more-than-code/defect-drainer-worker/agentadapter"
)

type recordingSink struct{ events []Event }

func (s *recordingSink) Apply(e Event) error { s.events = append(s.events, e); return nil }
func (s *recordingSink) Gone() bool          { return false }
func (s *recordingSink) Cancelled() bool     { return false }
func (s *recordingSink) Track(*Proc) bool    { return true }
func (s *recordingSink) Untrack(*Proc)       {}

// The orchestrator redacts too, so only an executor-level test shows that
// nothing the executor emits carries the password over a transport.
func TestEmitRedactsTheSecret(t *testing.T) {
	const secret = "S3cr3t-Value-42"
	sink := &recordingSink{}
	r := &run{spec: JobSpec{JobID: "bjob_1"}, sink: sink, secret: secret}
	_ = r.emit(Event{Kind: EventLog, Lines: []LogLine{{Text: "typed " + secret, Level: "info", Source: "Grok"}}})
	_ = r.emit(Event{Kind: EventStatus, Status: &Status{Phase: PhaseAgentExit, Failed: true, Error: "login failed for " + secret}, Lines: []LogLine{guarded("agent said "+secret, "error")}})
	_ = r.emit(Event{Kind: EventDone, Done: &Done{State: "failed", Line: secret}})
	// RF-a: a BLOCKED: reason is agent-authored too.
	_ = r.emit(Event{Kind: EventAgentRun, AgentRun: &AgentRun{StoppedBy: StoppedByBlocked, StopReason: "login with " + secret + " refused"}})
	raw, err := json.Marshal(sink.events)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), secret) {
		t.Fatalf("secret in emitted events: %s", raw)
	}
	if got := sink.events[0].Lines[0].Text; got != "typed [redacted]" {
		t.Fatalf("line = %q", got)
	}
	if sink.events[0].JobID != "bjob_1" {
		t.Fatalf("job id = %q", sink.events[0].JobID)
	}
}

func TestCollectFixHarvestRefusesPathIDs(t *testing.T) {
	handoff := t.TempDir()
	outside := filepath.Join(filepath.Dir(handoff), "fix-evidence")
	got := CollectFixHarvest(handoff, nil, []string{"../x", "a/b", "..", "DEF-1"}, timeZero, timeZero)
	if len(got.Defects) != 1 || got.Defects[0].DefectID != "DEF-1" {
		t.Fatalf("collected %+v (outside %s)", got.Defects, outside)
	}
}

func TestHostDenyReadAndCredential(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no home")
	}
	h := Host{
		DataRoot: "/data", DefectsRoot: "/inv", EnvFiles: []string{"/etc/dd.env", "relative.env"},
		Credential:      agentadapter.Credential{Mode: agentadapter.Subscription, ConfigDir: "/home/x/.claude-cfg"},
		PerJobConfigDir: true,
	}
	deny := h.DenyRead()
	for _, want := range []string{
		filepath.Join(home, ".ssh"), filepath.Join(home, ".grok"), "/data/defect-drainer.db", "/data/defect-drainer.db-wal",
		"/data/agent-state", "/data/clones", "/data/batch-jobs", "/inv/evidence", "/etc/dd.env", "/home/x/.claude-cfg",
	} {
		if !slices.Contains(deny, want) {
			t.Errorf("deny-read lacks %s: %v", want, deny)
		}
	}
	if slices.Contains(deny, "relative.env") {
		t.Errorf("a relative path is listed: %v", deny)
	}
	if c := h.CredentialFor("bjob_1"); c.ConfigDir != "/data/agent-state/bjob_1/cli-config" {
		t.Fatalf("per-job config dir = %q", c.ConfigDir)
	}
}

var timeZero = parseCutoff("")
