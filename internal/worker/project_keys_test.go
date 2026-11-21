package worker

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	dexec "github.com/more-than-code/defect-drainer-worker/exec"
	"github.com/more-than-code/defect-drainer-worker/workerproto"
)

// §18: the hello lists project_keys.v1.
func TestHelloListsProjectKeys(t *testing.T) {
	h := newHarness(t, harnessOpts{noStart: true})
	h.start(Timing{})
	hello := h.fs.frames(workerproto.TypeHello)
	var f workerproto.Hello
	if err := json.Unmarshal(hello[len(hello)-1].raw, &f); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(f.Capabilities, workerproto.CapProjectKeysV1) {
		t.Fatalf("hello caps %v", f.Capabilities)
	}
}

// A spec as an rc6-era server sends it (appId, app, app_id only) runs end to
// end, and the brief and log say "Project".
func TestOldKeySpecRunsEndToEnd(t *testing.T) {
	h := newHarness(t, harnessOpts{})
	spec := fixSpec("bjob_pk_old")
	raw := oldKeys(t, spec)
	for _, k := range []string{`"projectId"`, `"project"`, `"project_id"`} {
		if strings.Contains(string(raw), k) {
			t.Fatalf("old-key spec has %s: %s", k, raw)
		}
	}
	h.offerRaw(spec, raw)

	d := h.waitDone(spec.JobID, 60*time.Second)
	events := h.fs.jobEvents(spec.JobID)
	if d.State != "completed" {
		t.Fatalf("done = %+v\nlog:\n%s\nworker log:\n%s", d, joinLines(events), h.log.String())
	}
	if !strings.Contains(joinLines(events), "project app1 grok_sandbox=strict") {
		t.Fatalf("no project log line:\n%s", joinLines(events))
	}
	brief, err := os.ReadFile(filepath.Join(h.paths.Handoff(spec.JobID), "BRIEF.md"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(brief), "## Project\n`app1`") || strings.Contains(string(brief), "## App\n") {
		t.Fatalf("brief does not name the project:\n%s", brief)
	}
}

// A key and its alias with different values: decline bad_spec, naming both.
func TestConflictingProjectKeysDeclineBadSpec(t *testing.T) {
	h := newHarness(t, harnessOpts{})
	spec := fixSpec("bjob_pk_clash")
	b, err := json.Marshal(spec)
	if err != nil {
		t.Fatal(err)
	}
	raw := []byte(strings.Replace(string(b), `"projectId":"app1"`, `"projectId":"app1","appId":"app2"`, 1))
	h.offerRaw(spec, raw)

	eventually(t, 30*time.Second, "decline", func() bool { return h.fs.declineFor(spec.JobID) != "" })
	if got := h.fs.declineFor(spec.JobID); got != workerproto.DeclineBadSpec {
		t.Fatalf("decline %q, want bad_spec", got)
	}
	if !strings.Contains(h.log.String(), "conflicting keys: projectId and appId differ") {
		t.Fatalf("worker log lacks the reason:\n%s", h.log.String())
	}
}

// offerRaw registers spec, serves raw as its spec body, and offers it.
func (h *harness) offerRaw(spec dexec.JobSpec, raw []byte) {
	j := h.fs.addJob(spec, nil)
	h.fs.mu.Lock()
	j.rawSpec = raw
	h.fs.mu.Unlock()
	h.fs.offer(spec.JobID, "ls_"+spec.JobID, "dd_wjt_tok_"+spec.JobID, "")
}

// oldKeys is spec's JSON with the keys an rc6-era server used.
func oldKeys(t *testing.T, spec dexec.JobSpec) []byte {
	t.Helper()
	b, err := json.Marshal(spec)
	if err != nil {
		t.Fatal(err)
	}
	var tree any
	if err := json.Unmarshal(b, &tree); err != nil {
		t.Fatal(err)
	}
	old := map[string]string{"projectId": "appId", "project": "app", "project_id": "app_id"}
	var rename func(v any) any
	rename = func(v any) any {
		switch t := v.(type) {
		case map[string]any:
			out := map[string]any{}
			for k, x := range t {
				if o, ok := old[k]; ok {
					k = o
				}
				out[k] = rename(x)
			}
			return out
		case []any:
			for i := range t {
				t[i] = rename(t[i])
			}
		}
		return v
	}
	out, err := json.Marshal(rename(tree))
	if err != nil {
		t.Fatal(err)
	}
	return out
}
