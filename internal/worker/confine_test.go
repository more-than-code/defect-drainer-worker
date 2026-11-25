package worker

import (
	"strings"
	"testing"

	"github.com/more-than-code/defect-drainer-worker/workerproto"
)

// grok and antigravity do not put the worker's deny lists and egress
// allowlist into their CLI's sandbox, and the agent shares the worker's uid.
// The worker declines them even when policy lists them and they are ready.
func TestUnconfinedAdaptersAreDeclined(t *testing.T) {
	pol := strings.Replace(defaultPolicy, `adapters = ["claude"]`, `adapters = ["grok", "antigravity", "claude"]`, 1)
	h := newHarness(t, harnessOpts{policy: pol})
	h.w.mu.Lock()
	h.w.statuses = []workerproto.AdapterStatus{
		{Name: "claude", Ready: true, MinOK: true},
		{Name: "grok", Ready: true, MinOK: true},
		{Name: "antigravity", Ready: true, MinOK: true},
	}
	h.w.mu.Unlock()

	for _, name := range []string{"grok", "antigravity"} {
		d := h.w.applyPolicy(fixSpec("bjob_unconf_"+name), name, "", "")
		if d.reason != workerproto.DeclinePolicyDenied || !strings.Contains(d.detail, "deny lists") {
			t.Errorf("%s offer: %q %q", name, d.reason, d.detail)
		}
		// A resume of a job some other worker started with it is declined too.
		if d := h.w.applyPolicy(fixSpec("bjob_unconf_r_"+name), "", "", name); d.reason != workerproto.DeclinePolicyDenied {
			t.Errorf("%s resume: %q %q", name, d.reason, d.detail)
		}
	}
	// With no adapter named, the first confining ready adapter is picked,
	// though policy lists grok first.
	d := h.w.applyPolicy(fixSpec("bjob_unconf_any"), "", "", "")
	if d.reason != "" || d.spec.Adapter != "claude" {
		t.Fatalf("auto pick: %q %q adapter=%q", d.reason, d.detail, d.spec.Adapter)
	}
}
