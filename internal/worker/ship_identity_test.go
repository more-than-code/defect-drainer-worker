package worker

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	dexec "github.com/more-than-code/defect-drainer-worker/exec"
	"github.com/more-than-code/defect-drainer-worker/workerproto"
)

// Ship commits as Defect Drainer on every host (W3, the maintainers, 2026-11-18): the
// pushed commit's author and committer are the fixed identity, and a host
// GIT_AUTHOR_*/GIT_COMMITTER_* in the worker's env does not override it.
func TestShipCommitsAsDefectDrainer(t *testing.T) {
	fakeGH(t)
	agent := fixArtifacts(testDefect) + "echo changed >> ../worktrees/app/README.md\n" +
		"cat '" + claudeFixture(t, "c1-basic") + "'\nexit 0\n"
	h := newHarness(t, harnessOpts{agent: agent, noStart: true})
	// newHarness's setupGit already put a host identity in the env.
	if os.Getenv("GIT_AUTHOR_NAME") == "" || os.Getenv("GIT_COMMITTER_EMAIL") == "" {
		t.Fatal("the harness set no host identity to override")
	}
	h.start(Timing{})
	id := "bjob_ident"
	h.offerJob(fixSpec(id), nil)
	h.waitDone(id, 60*time.Second)
	ship := dexec.Control{Kind: dexec.ControlShip, Repos: []dexec.ShipRepo{{Repo: "app", Files: []string{"README.md"}, Message: "Fix", Title: "Fix", Body: "b"}}}
	r := h.controlAndWait(id, "rq_ship", ship, 1)
	var res workerproto.ShipPushResult
	if !r.OK || json.Unmarshal(r.Result, &res) != nil || len(res.Repos) != 1 || !res.Repos[0].Pushed {
		t.Fatalf("ship %+v %s", r, r.Result)
	}
	origin := filepath.Join(h.tmp, "origin", "app.git")
	out, err := exec.Command("git", "-C", origin, "log", "-1", "--format=%an <%ae>|%cn <%ce>", "dd/"+id+"/app").Output()
	if err != nil {
		t.Fatal(err)
	}
	want := commitName + " <" + commitEmail + ">"
	if got := strings.TrimSpace(string(out)); got != want+"|"+want {
		t.Fatalf("pushed commit identity %q, want %q for both", got, want)
	}
}
