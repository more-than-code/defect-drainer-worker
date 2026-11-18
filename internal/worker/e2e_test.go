package worker

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	dexec "github.com/more-than-code/defect-drainer-worker/exec"
	"github.com/more-than-code/defect-drainer-worker/workerproto"
)

// T7: one real internal/exec batch fix runs under the worker against the
// fake server, with the fake claude replaying a recorded stream. Harvest
// images go up as blobs before the event that names them, and the remote
// event carries no inline data.
func TestEndToEndFixJob(t *testing.T) {
	h := newHarness(t, harnessOpts{})
	evidence := []byte("\x89PNG\r\n\x1a\nevidence-bytes-0000")
	sha := hashHex(evidence)
	spec := fixSpec("bjob_e2e1")
	spec.Fix.Evidence = []dexec.EvidenceRef{{Path: "evidence/" + testDefect + "/shot.png", SHA256: sha}}
	fj := h.offerJob(spec, map[string][]byte{sha: evidence})

	d := h.waitDone("bjob_e2e1", 60*time.Second)
	events := h.fs.jobEvents("bjob_e2e1")
	if d.State != "completed" {
		t.Fatalf("done = %+v\nlog:\n%s\nworker log:\n%s", d, joinLines(events), h.log.String())
	}
	if !h.fs.accepted("bjob_e2e1") {
		t.Fatal("no accept frame")
	}
	// Evidence was materialised under the job's evidence root.
	got, err := os.ReadFile(filepath.Join(h.paths.Evidence("bjob_e2e1"), "evidence", testDefect, "shot.png"))
	if err != nil || string(got) != string(evidence) {
		t.Fatalf("evidence not materialised: %v", err)
	}
	// The harvest names the fix image by hash, without inline data, and the
	// blob was PUT before the event that named it was applied.
	var harvest *dexec.FixHarvest
	for _, e := range events {
		if e.Harvest != nil {
			harvest = e.Harvest
		}
	}
	if harvest == nil || len(harvest.Defects) != 1 || len(harvest.Defects[0].Images) != 1 {
		t.Fatalf("harvest = %+v\n%s", harvest, joinLines(events))
	}
	img := harvest.Defects[0].Images[0]
	if img.Data != nil || !workerproto.ValidSHA256(img.SHA256) || img.Size == 0 {
		t.Fatalf("image = %+v", img)
	}
	h.fs.mu.Lock()
	put, applied := fj.putOrder[img.SHA256], fj.appliedAt[img.SHA256]
	ct := fj.blobTypes[img.SHA256]
	h.fs.mu.Unlock()
	if put == 0 || applied < put || ct != "image/png" {
		t.Fatalf("blob put=%d applied=%d type=%q", put, applied, ct)
	}
	// Seqs were contiguous, nothing re-sent, nothing lost; done is last.
	if events[len(events)-1].Kind != dexec.EventDone {
		t.Fatalf("last event %s", events[len(events)-1].Kind)
	}
	h.fs.mu.Lock()
	dup := fj.duplicates
	h.fs.mu.Unlock()
	if dup != 0 {
		t.Fatalf("%d duplicate frames", dup)
	}
	// Worker-derived paths (§8.2).
	wt := filepath.Join(h.paths.WorktreeRoot("bjob_e2e1"), "app")
	if _, err := os.Stat(filepath.Join(wt, "README.md")); err != nil {
		t.Fatalf("worktree not cut at %s: %v", wt, err)
	}
	if _, err := os.Stat(filepath.Join(h.paths.Handoff("bjob_e2e1"), "BRIEF.md")); err != nil {
		t.Fatalf("handoff not at the worker path: %v", err)
	}
	// Once done is acked the spool is empty and the job is no longer busy.
	eventually(t, 5*time.Second, "spool drained", func() bool {
		items, _ := os.ReadDir(filepath.Join(h.paths.SpoolRoot("bjob_e2e1"), "spool"))
		return len(items) == 0
	})
	if ids := h.w.busyIDs(); len(ids) != 0 {
		t.Fatalf("busy after done: %v", ids)
	}
	if os.Getenv("W2A_DEBUG") != "" {
		t.Log(joinLines(events))
	}
	if !strings.Contains(joinLines(events), "adapter claude") {
		t.Fatalf("no worker start line:\n%s", joinLines(events))
	}
}
