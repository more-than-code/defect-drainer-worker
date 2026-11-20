package exec

import (
	"math"
	"reflect"
	"testing"

	aa "github.com/more-than-code/defect-drainer-worker/agentadapter"
)

// sameCosts compares within float rounding.
func sameCosts(a, b map[string]aa.ModelCost) bool {
	if len(a) != len(b) {
		return false
	}
	for k, x := range a {
		y, ok := b[k]
		if !ok || math.Abs(x.CostUSD-y.CostUSD) > 1e-9 || x.InputTokens != y.InputTokens || x.OutputTokens != y.OutputTokens {
			return false
		}
	}
	return true
}

// end is what the OnEnd hook does with one run's end.
func end(acc *AgentRunAcc, e aa.End) {
	acc.End(e.StopReason, e.SessionID, e.NumTurns, e.TotalCostUSD)
	acc.EndDetail(e)
}

// M6: per-model cost sums across iterations and a Seed, and Snapshot is a
// copy. The total and the cap still read the total.
func TestAgentRunCostByModel(t *testing.T) {
	acc := &AgentRunAcc{}
	acc.Seed(&AgentRun{TotalCostUSD: 1, CostByModel: map[string]aa.ModelCost{"opus": {CostUSD: 1, InputTokens: 5, OutputTokens: 50}}})
	end(acc, aa.End{TotalCostUSD: 0.5, CostByModel: map[string]aa.ModelCost{"sonnet": {CostUSD: 0.4, OutputTokens: 10}, "haiku": {CostUSD: 0.1}}})
	end(acc, aa.End{TotalCostUSD: 0.2, CostByModel: map[string]aa.ModelCost{"sonnet": {CostUSD: 0.2, OutputTokens: 3}}})
	end(acc, aa.End{TotalCostUSD: 0.05}) // no split (grok): unknown
	snap := acc.Snapshot(3, "", nil)
	want := map[string]aa.ModelCost{
		"opus":          {CostUSD: 1, InputTokens: 5, OutputTokens: 50},
		"sonnet":        {CostUSD: 0.6, OutputTokens: 13},
		"haiku":         {CostUSD: 0.1},
		aa.UnknownModel: {CostUSD: 0.05},
	}
	if !sameCosts(snap.CostByModel, want) {
		t.Fatalf("costByModel = %+v", snap.CostByModel)
	}
	if acc.CostUSD() != snap.TotalCostUSD || math.Abs(snap.TotalCostUSD-1.75) > 1e-9 {
		t.Fatalf("total = %v / %v", acc.CostUSD(), snap.TotalCostUSD)
	}
	snap.CostByModel["opus"] = aa.ModelCost{}
	if acc.Snapshot(3, "", nil).CostByModel["opus"].CostUSD != 1 {
		t.Fatal("Snapshot aliases the accumulator")
	}
	// A run that reported no cost has no map at all (the field is omitted).
	if s := (&AgentRunAcc{}).Snapshot(1, "", nil); s.CostByModel != nil {
		t.Fatalf("empty = %+v", s.CostByModel)
	}
	// A prior record from before costByModel keeps its total under unknown.
	old := &AgentRunAcc{}
	old.Seed(&AgentRun{TotalCostUSD: 4.99})
	if got := old.Snapshot(1, "", nil).CostByModel; !reflect.DeepEqual(got, map[string]aa.ModelCost{aa.UnknownModel: {CostUSD: 4.99}}) {
		t.Fatalf("old seed = %+v", got)
	}
}

// Review: each lens's split is summed; a lens without one counts as unknown.
func TestReviewCostByModel(t *testing.T) {
	rev := ReviewRun{Lenses: []ReviewLens{
		{Lens: "a", OK: true, CostUSD: 0.3, CostByModel: map[string]aa.ModelCost{"sonnet": {CostUSD: 0.3, InputTokens: 2}}},
		{Lens: "b", OK: true, CostUSD: 0.2, CostByModel: map[string]aa.ModelCost{"sonnet": {CostUSD: 0.2, InputTokens: 1}}},
		{Lens: "c", OK: true, CostUSD: 0.1},
	}}
	rev.Finish()
	rev.Finish() // idempotent
	want := map[string]aa.ModelCost{"sonnet": {CostUSD: 0.5, InputTokens: 3}, aa.UnknownModel: {CostUSD: 0.1}}
	if !sameCosts(rev.CostByModel, want) || math.Abs(rev.TotalCostUSD-0.6) > 1e-9 {
		t.Fatalf("review = %+v", rev)
	}
}

// CloneReview gives the clone its own costByModel maps: the control plane
// snapshots jobs through it while the run still writes the original.
func TestCloneReviewCopiesCostByModel(t *testing.T) {
	orig := &ReviewRun{
		CostByModel: map[string]aa.ModelCost{"sonnet": {CostUSD: 1}},
		Lenses:      []ReviewLens{{Lens: "code", CostByModel: map[string]aa.ModelCost{"sonnet": {CostUSD: 1}}}},
	}
	c := CloneReview(orig)
	c.CostByModel["sonnet"] = aa.ModelCost{CostUSD: 9}
	c.CostByModel["opus"] = aa.ModelCost{CostUSD: 9}
	c.Lenses[0].CostByModel["sonnet"] = aa.ModelCost{CostUSD: 9}
	if len(orig.CostByModel) != 1 || orig.CostByModel["sonnet"].CostUSD != 1 || orig.Lenses[0].CostByModel["sonnet"].CostUSD != 1 {
		t.Fatalf("clone shares maps with the original: %+v", orig)
	}
}
