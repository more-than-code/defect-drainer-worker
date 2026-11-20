package agentadapter

import (
	"reflect"
	"testing"
)

func TestAddCost(t *testing.T) {
	// Nothing to add leaves dst as is (nil stays nil).
	if got := AddCost(nil, nil, 0); got != nil {
		t.Fatalf("empty = %+v", got)
	}
	// A cost without a split goes under unknown.
	got := AddCost(nil, nil, 0.25)
	if !reflect.DeepEqual(got, map[string]ModelCost{UnknownModel: {CostUSD: 0.25}}) {
		t.Fatalf("no split = %+v", got)
	}
	// A split is summed per model, field by field; the total is ignored.
	got = AddCost(got, map[string]ModelCost{"sonnet": {CostUSD: 1, InputTokens: 10, OutputTokens: 100}}, 99)
	got = AddCost(got, map[string]ModelCost{"sonnet": {CostUSD: 2, InputTokens: 1, OutputTokens: 5}, "haiku": {CostUSD: 0.5}}, 2.5)
	want := map[string]ModelCost{
		UnknownModel: {CostUSD: 0.25},
		"sonnet":     {CostUSD: 3, InputTokens: 11, OutputTokens: 105},
		"haiku":      {CostUSD: 0.5},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("summed = %+v", got)
	}
}

func TestCopyCosts(t *testing.T) {
	if CopyCosts(map[string]ModelCost{}) != nil {
		t.Fatal("empty copy is not nil")
	}
	src := map[string]ModelCost{"sonnet": {CostUSD: 1}}
	cp := CopyCosts(src)
	cp["sonnet"] = ModelCost{CostUSD: 9}
	if src["sonnet"].CostUSD != 1 {
		t.Fatal("copy aliases the source")
	}
}
