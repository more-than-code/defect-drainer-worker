package claude

import (
	"math"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	aa "github.com/more-than-code/defect-drainer-worker/agentadapter"
)

// rc3Implementer is the rc3 implementer argv for task(Implementer) with no
// resume: a job without a model must still produce exactly this.
var rc3Implementer = []string{"-p", "do it", "--output-format", "stream-json", "--verbose", "--permission-mode", "dontAsk",
	"--settings", "/data/agent-state/j1/claude-settings.json", "--setting-sources", "user", "--strict-mcp-config",
	"--add-dir", "/data/wt/a", "--add-dir", "/data/wt/b", "--max-turns", "40"}

// withModel is rc3Implementer with --model m after --strict-mcp-config.
func withModel(m string) []string {
	i := slices.Index(rc3Implementer, "--strict-mcp-config") + 1
	return slices.Concat(rc3Implementer[:i], []string{"--model", m}, rc3Implementer[i:])
}

func TestModelArgv(t *testing.T) {
	a := New(Config{Bin: "/usr/bin/claude"})
	for _, tc := range []struct {
		model string
		want  []string
	}{
		{"", rc3Implementer},
		{"sonnet", withModel("sonnet")},
		{"claude-opus-4-5-20251101", withModel("claude-opus-4-5-20251101")},
	} {
		tk := task(aa.Implementer, aa.APIKey)
		tk.Model = tc.model
		c, err := a.Command(tk)
		if err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(c.Args, tc.want) {
			t.Errorf("model %q: argv = %q", tc.model, c.Args)
		}
	}
}

// M4: reviewers run on the job's model.
func TestReviewerGetsModel(t *testing.T) {
	tk := task(aa.Reviewer, aa.APIKey)
	tk.Model = "haiku"
	c, err := New(Config{Bin: "/usr/bin/claude"}).Command(tk)
	if err != nil {
		t.Fatal(err)
	}
	i := slices.Index(c.Args, "--model")
	if i < 0 || c.Args[i+1] != "haiku" || !slices.Contains(c.Args, "--disallowedTools") {
		t.Fatalf("reviewer argv = %q", c.Args)
	}
}

func TestInvalidModelRefused(t *testing.T) {
	for _, m := range []string{"-x", "--dangerously-skip-permissions", "a b", "sonnet;rm", strings.Repeat("a", 65)} {
		tk := task(aa.Implementer, aa.APIKey)
		tk.Model = m
		if _, err := New(Config{Bin: "/usr/bin/claude"}).Command(tk); err == nil {
			t.Errorf("model %q accepted", m)
		}
	}
}

func near(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

// M6 on recorded data: c1's single model, keyed by canonicalModel.
func TestCostByModelRecorded(t *testing.T) {
	e := decodeFixture(t, "c1-basic").ends[0]
	want := map[string]aa.ModelCost{"claude-haiku-4-5": {CostUSD: 0.04495095, InputTokens: 34, OutputTokens: 810}}
	if !reflect.DeepEqual(e.CostByModel, want) {
		t.Fatalf("costByModel = %+v", e.CostByModel)
	}
}

// Every recorded result that has a split sums to its total.
func TestCostByModelSumsToTotal(t *testing.T) {
	ents, _ := filepath.Glob("testdata/*.jsonl")
	for _, p := range ents {
		name := strings.TrimSuffix(filepath.Base(p), ".jsonl")
		e := decodeFixture(t, name).ends[0]
		if len(e.CostByModel) == 0 {
			continue
		}
		var sum float64
		for _, c := range e.CostByModel {
			sum += c.CostUSD
		}
		if !near(sum, e.TotalCostUSD) {
			t.Errorf("%s: costByModel sums to %v, total %v", name, sum, e.TotalCostUSD)
		}
	}
}

// testdata/handmade-two-model.jsonl is HAND-MADE, not recorded: no run so far
// used two models. It has a sonnet entry with canonicalModel and a haiku
// entry without one (the map key is used).
func TestCostByModelTwoModelsHandMade(t *testing.T) {
	e := decodeFixture(t, "handmade-two-model").ends[0]
	want := map[string]aa.ModelCost{
		"claude-sonnet-4-5":         {CostUSD: 0.4425, InputTokens: 120, OutputTokens: 4200},
		"claude-haiku-4-5-20251001": {CostUSD: 0.03, InputTokens: 40, OutputTokens: 900},
	}
	if !reflect.DeepEqual(e.CostByModel, want) || !near(e.TotalCostUSD, 0.4725) {
		t.Fatalf("end = %+v", e)
	}
}

func TestCostByModelFallbacks(t *testing.T) {
	end := func(line string) aa.End {
		evs := New(Config{}).NewDecoder().Decode([]byte(line))
		if len(evs) == 0 || evs[0].Kind != aa.EndEvent {
			t.Fatalf("%s: %+v", line, evs)
		}
		return evs[0].End
	}
	// No modelUsage, an empty one, an undecodable one: no split, total kept.
	for _, l := range []string{
		`{"type":"result","subtype":"success","total_cost_usd":0.5}`,
		`{"type":"result","subtype":"success","total_cost_usd":0.5,"modelUsage":{}}`,
		`{"type":"result","subtype":"success","total_cost_usd":0.5,"modelUsage":"x"}`,
		`{"type":"result","subtype":"success","total_cost_usd":0.5,"modelUsage":null}`,
	} {
		if e := end(l); e.CostByModel != nil || e.TotalCostUSD != 0.5 {
			t.Errorf("%s: %+v", l, e)
		}
	}
	// A name that is not a model name goes under unknown.
	e := end(`{"type":"result","total_cost_usd":0.3,"modelUsage":{"ok-model":{"costUSD":0.1},"bad name":{"costUSD":0.2,"outputTokens":7}}}`)
	want := map[string]aa.ModelCost{"ok-model": {CostUSD: 0.1}, aa.UnknownModel: {CostUSD: 0.2, OutputTokens: 7}}
	if !reflect.DeepEqual(e.CostByModel, want) {
		t.Errorf("bad key: %+v", e.CostByModel)
	}
}

// More than maxModels names: the rest fold into unknown, the sum is kept.
func TestCostByModelBounded(t *testing.T) {
	var b strings.Builder
	b.WriteString(`{"type":"result","total_cost_usd":2,"modelUsage":{`)
	for i := 0; i < 20; i++ {
		if i > 0 {
			b.WriteString(",")
		}
		b.WriteString(`"m` + string(rune('a'+i)) + `":{"costUSD":0.1}`)
	}
	b.WriteString(`}}`)
	e := New(Config{}).NewDecoder().Decode([]byte(b.String()))[0].End
	if len(e.CostByModel) != maxModels+1 || !near(e.CostByModel[aa.UnknownModel].CostUSD, 0.4) {
		t.Fatalf("costByModel = %+v", e.CostByModel)
	}
}
