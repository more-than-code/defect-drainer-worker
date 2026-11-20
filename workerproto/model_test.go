package workerproto

import "testing"

func TestModelAllowed(t *testing.T) {
	for _, c := range []struct {
		allowed        []string
		adapter, model string
		ok             bool
	}{
		{nil, "claude", "opus", true},
		{[]string{}, "claude", "", true}, // empty list: any model
		{[]string{"sonnet"}, "claude", "sonnet", true},
		{[]string{"sonnet"}, "claude", "Sonnet", false}, // case-sensitive
		{[]string{"sonnet"}, "claude", "claude-sonnet-4-5", false},
		{[]string{"sonnet"}, "claude", "", false},
		{[]string{"sonnet"}, "antigravity", "", false},
		{[]string{"sonnet"}, "grok", "", true},
	} {
		if got := ModelAllowed(c.allowed, c.adapter, c.model); got != c.ok {
			t.Errorf("%v %s %q = %v", c.allowed, c.adapter, c.model, got)
		}
	}
}
