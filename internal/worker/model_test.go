package worker

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/more-than-code/defect-drainer-worker/agentadapter"
	dexec "github.com/more-than-code/defect-drainer-worker/exec"
	"github.com/more-than-code/defect-drainer-worker/workerproto"
)

// M3: allowed_models parses, refuses bad values, and goes on the wire only
// when set.
func TestAllowedModelsPolicy(t *testing.T) {
	path := filepath.Join(t.TempDir(), "policy.toml")
	base := "enabled = true\norgs = [\"org_a\"]\n"
	writeFile(t, path, base, 0o644)
	p := LoadPolicy(path, false, rootUID)
	if !p.Valid || p.AllowedModels != nil || p.Wire().AllowedModels != nil {
		t.Fatalf("absent: %+v", p)
	}
	writeFile(t, path, base+"allowed_models = [\"sonnet\", \"claude-haiku-4-5\"]\n", 0o644)
	p = LoadPolicy(path, false, rootUID)
	if !p.Valid || !reflect.DeepEqual(p.Wire().AllowedModels, []string{"sonnet", "claude-haiku-4-5"}) {
		t.Fatalf("set: %+v", p)
	}
	many := make([]string, workerproto.MaxAllowedModels+1)
	for i := range many {
		many[i] = "\"m" + string(rune('a'+i)) + "\""
	}
	for _, v := range []string{
		"[]",                       // would decline every job
		"[\"\"]",                   // empty name
		"[\"-x\"]",                 // flag-like
		"[\"a b\"]",                // outside the pattern
		"[\"sonnet\", \"sonnet\"]", // duplicate
		"[" + strings.Join(many, ", ") + "]",
	} {
		writeFile(t, path, base+"allowed_models = "+v+"\n", 0o644)
		if p := LoadPolicy(path, false, rootUID); p.Valid || p.Reason != "bad_allowed_models" || p.Wire().AllowedModels != nil {
			t.Errorf("%s: %+v", v, p)
		}
	}
}

func TestModelAllowed(t *testing.T) {
	any := Policy{}
	if !any.ModelAllowed("claude", "opus") || !any.ModelAllowed("claude", "") {
		t.Fatal("no allowed_models allows any model")
	}
	p := Policy{AllowedModels: []string{"sonnet"}}
	for _, c := range []struct {
		adapter, model string
		ok             bool
	}{
		{"claude", "sonnet", true},
		{"claude", "opus", false},
		{"claude", "claude-sonnet-4-5", false}, // exact match: an alias does not cover a full id
		{"claude", "", false},                  // D2: the CLI default is unvetted
		{"antigravity", "", false},
		{"grok", "", true}, // grok has no model switch
	} {
		if got := p.ModelAllowed(c.adapter, c.model); got != c.ok {
			t.Errorf("%s %q = %v", c.adapter, c.model, got)
		}
	}
}

// AC2: a host that allows sonnet only declines an opus job with
// model_not_allowed, takes a sonnet job, and says so on the wire.
func TestModelNotAllowedDecline(t *testing.T) {
	h := newHarness(t, harnessOpts{policy: defaultPolicy + "allowed_models = [\"sonnet\"]\n"})
	ok := fixSpec("bjob_mc_ok")
	ok.Model = "sonnet"
	if d := h.w.applyPolicy(ok, "", "", ""); d.reason != "" || d.spec.Model != "sonnet" {
		t.Fatalf("sonnet: %+v", d)
	}
	for _, c := range []struct{ spec, offer, detail string }{
		{"opus", "", "model opus not allowed (policy)"},
		{"sonnet", "opus", "model opus not allowed (policy)"}, // the offer's model wins
		{"", "", "model (CLI default) not allowed (policy)"},
	} {
		s := fixSpec("bjob_mc_no")
		s.Model = c.spec
		if d := h.w.applyPolicy(s, "", c.offer, ""); d.reason != workerproto.DeclineModelNotAllowed || d.detail != c.detail {
			t.Errorf("%+v: %s %q", c, d.reason, d.detail)
		}
	}
	// The frame the server sees.
	spec := fixSpec("bjob_mc_frame")
	spec.Model = "opus"
	h.offerJob(spec, nil)
	eventually(t, 30*time.Second, "decline", func() bool { return h.fs.declineFor(spec.JobID) != "" })
	if got := h.fs.declineFor(spec.JobID); got != workerproto.DeclineModelNotAllowed {
		t.Fatalf("decline %q", got)
	}
}

// AC1 and AC3 through the worker: the model reaches the claude argv, and the
// agent run's costByModel carries across a Rerun, an older prior record's
// total going under "unknown", summing to totalCostUsd.
func TestModelArgvAndCostByModel(t *testing.T) {
	h := newHarness(t, harnessOpts{agent: "cat '" + claudeFixture(t, "c1-basic") + "'\nexit 0\n"})
	spec := fixSpec("bjob_mc_cost")
	spec.Model = "sonnet"
	spec.Caps.AgentMaxIters = 1
	h.offerJob(spec, nil)
	h.waitDone(spec.JobID, 60*time.Second)
	eventually(t, 5*time.Second, "acked", func() bool { return len(h.w.busyIDs()) == 0 })
	events := h.fs.jobEvents(spec.JobID)
	if argv, _ := os.ReadFile(h.argv); !strings.Contains(string(argv), "--strict-mcp-config --model sonnet --add-dir") {
		t.Fatalf("argv:\n%s", argv)
	}
	const c1 = 0.04495095
	haiku := agentadapter.ModelCost{CostUSD: c1, InputTokens: 34, OutputTokens: 810}
	r := lastAgentRun(events)
	if r == nil || !reflect.DeepEqual(r.CostByModel, map[string]agentadapter.ModelCost{"claude-haiku-4-5": haiku}) {
		t.Fatalf("agent run %+v", r)
	}

	// Rerun seeded with a prior record from before costByModel.
	n := len(events)
	h.fs.mu.Lock()
	rs := &h.fs.jobs[spec.JobID].spec
	rs.PriorAgentRun = &dexec.AgentRun{Iterations: 1, TotalCostUSD: 0.5, Runner: "claude"}
	h.fs.mu.Unlock()
	h.fs.offerMode(spec.JobID, "ls_mc_cost2", "dd_wjt_mc_cost2", dexec.ModeRerun)
	eventually(t, 60*time.Second, "second done", func() bool {
		ev := h.fs.jobEvents(spec.JobID)
		return len(ev) > n && lastIsDone(ev)
	})
	r = lastAgentRun(h.fs.jobEvents(spec.JobID)[n:])
	want := map[string]agentadapter.ModelCost{"claude-haiku-4-5": haiku, agentadapter.UnknownModel: {CostUSD: 0.5}}
	if r == nil || !reflect.DeepEqual(r.CostByModel, want) {
		t.Fatalf("rerun agent run %+v", r)
	}
	var sum float64
	for _, c := range r.CostByModel {
		sum += c.CostUSD
	}
	if d := sum - r.TotalCostUSD; d > 1e-9 || d < -1e-9 {
		t.Fatalf("costByModel sums to %v, total %v", sum, r.TotalCostUSD)
	}
}
