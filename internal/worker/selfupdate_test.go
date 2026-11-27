package worker

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/more-than-code/defect-drainer-worker/internal/selfupdate"
	"github.com/more-than-code/defect-drainer-worker/workerproto"
)

const autoUpdatePolicy = defaultPolicy + "auto_update = \"server\"\n"

func withVersion(t *testing.T, v string) {
	t.Helper()
	old := Version
	Version = v
	t.Cleanup(func() { Version = old })
}

func fastSelfUpdateTick(t *testing.T) {
	t.Helper()
	old := selfUpdateTick
	selfUpdateTick = 50 * time.Millisecond
	t.Cleanup(func() { selfUpdateTick = old })
}

func lastSelfUpdate(h *harness) *workerproto.SelfUpdateStatus {
	var out *workerproto.SelfUpdateStatus
	for _, f := range h.fs.frames(workerproto.TypeStatus) {
		var st workerproto.Status
		if json.Unmarshal(f.raw, &st) == nil && st.SelfUpdate != nil {
			out = st.SelfUpdate
		}
	}
	return out
}

func requestFile(h *harness) string {
	return filepath.Join(h.paths.StateDir, selfupdate.RequestDirName, selfupdate.RequestName)
}

func TestSelfUpdateOffByDefault(t *testing.T) {
	withVersion(t, "0.2.0")
	fastSelfUpdateTick(t)
	h := newHarness(t, harnessOpts{noStart: true})
	h.fs.targetRelease = "0.2.1"
	h.start(Timing{})
	hello := h.fs.hellos[len(h.fs.hellos)-1]
	if !slices.Contains(hello.Capabilities, workerproto.CapSelfUpdateV1) {
		t.Fatalf("capabilities %v", hello.Capabilities)
	}
	if hello.SelfUpdate == nil || hello.SelfUpdate.Mode != workerproto.SelfUpdateOff || hello.SelfUpdate.Last != nil {
		t.Fatalf("hello self_update %+v", hello.SelfUpdate)
	}
	eventually(t, 5*time.Second, "status says off", func() bool {
		su := lastSelfUpdate(h)
		return su != nil && su.State == workerproto.SelfUpdateIdle && su.Reason == "off" && su.Target == "0.2.1"
	})
	time.Sleep(200 * time.Millisecond)
	if _, err := os.Stat(requestFile(h)); err == nil {
		t.Fatal("an opted-out worker wrote a request")
	}
}

func TestSelfUpdateRequestsAndDrains(t *testing.T) {
	withVersion(t, "0.2.0")
	fastSelfUpdateTick(t)
	h := newHarness(t, harnessOpts{policy: autoUpdatePolicy, noStart: true})
	h.fs.targetRelease = "0.2.1"
	h.start(Timing{})
	eventually(t, 5*time.Second, "request written", func() bool {
		b, err := os.ReadFile(requestFile(h))
		return err == nil && string(b) == "0.2.1\n"
	})
	eventually(t, 5*time.Second, "status updating", func() bool {
		su := lastSelfUpdate(h)
		return su != nil && su.State == workerproto.SelfUpdateUpdating && su.Mode == workerproto.SelfUpdateServer
	})
	// While the update is with the root step, offers are declined busy.
	h.offerJob(fixSpec("job_su1"), nil)
	eventually(t, 5*time.Second, "declined busy", func() bool { return h.fs.declineFor("job_su1") == workerproto.DeclineBusy })
	if !strings.Contains(h.log.String(), "updating dd-worker") {
		t.Fatal("decline does not say why")
	}

	// The root step refuses (say, the host turned busy) without a restart:
	// it deletes the request and writes result.json.
	root := filepath.Join(h.tmp, "su-root")
	os.MkdirAll(root, 0o755)
	os.Remove(requestFile(h))
	res, _ := json.Marshal(workerproto.SelfUpdateResult{From: "0.2.0", To: "0.2.1", Result: workerproto.SelfUpdateRefused, Reason: "busy", At: time.Now().UTC().Format(time.RFC3339)})
	os.WriteFile(filepath.Join(root, selfupdate.ResultName), res, 0o644)
	eventually(t, 5*time.Second, "cooldown", func() bool {
		su := lastSelfUpdate(h)
		return su != nil && su.State == workerproto.SelfUpdateCooldown && su.Last != nil && su.Last.Reason == "busy"
	})
	if h.w.drainingForUpdate() {
		t.Fatal("still draining after the refusal")
	}
	// Cooled down: no new request for the same target.
	time.Sleep(200 * time.Millisecond)
	if _, err := os.Stat(requestFile(h)); err == nil {
		t.Fatal("asked again during the cool-down")
	}
	h.offerJob(fixSpec("job_su2"), nil)
	eventually(t, 10*time.Second, "accepted after the cool-down began", func() bool { return h.fs.accepted("job_su2") })
}

func TestSelfUpdateRules(t *testing.T) {
	cases := []struct {
		name, version, target, policy, reason string
	}{
		{"not newer", "0.2.1", "0.2.1", autoUpdatePolicy, "not_newer"},
		{"downgrade", "0.2.1", "0.2.0", autoUpdatePolicy, "not_newer"},
		{"cross minor", "0.2.1", "0.3.0", autoUpdatePolicy, "cross_minor"},
		{"no target", "0.2.1", "", autoUpdatePolicy, reasonNoTarget},
		{"dev build", "0.1.0-dirty", "0.2.0", autoUpdatePolicy, "not_allowed"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			withVersion(t, c.version)
			fastSelfUpdateTick(t)
			h := newHarness(t, harnessOpts{policy: c.policy, noStart: true})
			h.fs.targetRelease = c.target
			h.start(Timing{})
			eventually(t, 5*time.Second, "idle "+c.reason, func() bool {
				su := lastSelfUpdate(h)
				return su != nil && su.State == workerproto.SelfUpdateIdle && su.Reason == c.reason
			})
			if _, err := os.Stat(requestFile(h)); err == nil {
				t.Fatal("request written")
			}
		})
	}
}

func TestSelfUpdateGivesUpWithoutARootStep(t *testing.T) {
	w := &Worker{offers: map[string]*pendingOffer{}, jobs: map[string]*job{}, logOut: &syncBuf{}}
	tmp := t.TempDir()
	w.opts.Paths = Paths{StateDir: filepath.Join(tmp, "state")}
	w.opts.SelfUpdateRootDir = filepath.Join(tmp, "root")
	os.MkdirAll(w.opts.Paths.StateDir, 0o700)
	withVersion(t, "0.2.0")
	w.policy = Policy{Valid: true, AutoUpdate: workerproto.SelfUpdateServer}
	w.su.target = "0.2.1"
	now := time.Date(2026, 11, 27, 3, 0, 0, 0, time.UTC)

	// A pending offer holds the request back.
	w.offers["job_x"] = &pendingOffer{}
	w.advanceSelfUpdate(now)
	if w.su.state != workerproto.SelfUpdateDraining || !w.su.draining || selfupdate.RequestPending(w.opts.Paths.StateDir) {
		t.Fatalf("with an offer: %+v", w.su)
	}
	delete(w.offers, "job_x")
	w.advanceSelfUpdate(now)
	if w.su.state != workerproto.SelfUpdateUpdating || !selfupdate.RequestPending(w.opts.Paths.StateDir) {
		t.Fatalf("idle: %+v", w.su)
	}
	// Nobody reads it for selfUpdateNoAnswer: give up and cool down.
	w.advanceSelfUpdate(now.Add(selfUpdateNoAnswer + time.Second))
	if w.su.state != workerproto.SelfUpdateCooldown || w.su.reason != reasonNoRootStep || w.su.draining || selfupdate.RequestPending(w.opts.Paths.StateDir) {
		t.Fatalf("no answer: %+v", w.su)
	}
	// A new target clears the cool-down.
	w.su.target = "0.2.2"
	w.su.state = workerproto.SelfUpdateIdle
	w.advanceSelfUpdate(now.Add(selfUpdateNoAnswer + 2*time.Second))
	if w.su.state != workerproto.SelfUpdateUpdating {
		t.Fatalf("new target: %+v", w.su)
	}
	// The window closes the door.
	w2 := &Worker{offers: map[string]*pendingOffer{}, jobs: map[string]*job{}, logOut: &syncBuf{}, opts: w.opts}
	win, _ := selfupdate.ParseWindow("04:00-05:00")
	w2.policy = Policy{Valid: true, AutoUpdate: workerproto.SelfUpdateServer, AutoUpdateWindow: win}
	w2.su.target = "0.2.1"
	os.Remove(filepath.Join(w.opts.Paths.StateDir, selfupdate.RequestDirName, selfupdate.RequestName))
	w2.advanceSelfUpdate(now)
	if w2.su.state != workerproto.SelfUpdateIdle || w2.su.reason != selfupdate.ReasonOutsideWindow {
		t.Fatalf("window: %+v", w2.su)
	}
}

func TestSelfUpdateReportsTheLastResultAtStart(t *testing.T) {
	withVersion(t, "0.2.1")
	h := newHarness(t, harnessOpts{policy: autoUpdatePolicy, noStart: true})
	root := filepath.Join(h.tmp, "su-root")
	os.MkdirAll(root, 0o755)
	want := workerproto.SelfUpdateResult{From: "0.2.0", To: "0.2.1", Result: workerproto.SelfUpdateSucceeded, At: "2026-11-27T03:00:00Z"}
	b, _ := json.Marshal(want)
	os.WriteFile(filepath.Join(root, selfupdate.ResultName), b, 0o644)
	h.fs.targetRelease = "0.2.1"
	h.start(Timing{})
	hello := h.fs.hellos[len(h.fs.hellos)-1]
	if hello.SelfUpdate == nil || hello.SelfUpdate.Last == nil || *hello.SelfUpdate.Last != want {
		t.Fatalf("hello self_update %+v", hello.SelfUpdate)
	}
}
