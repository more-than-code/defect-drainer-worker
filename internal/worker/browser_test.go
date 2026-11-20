package worker

import (
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	dexec "github.com/more-than-code/defect-drainer-worker/exec"
	"github.com/more-than-code/defect-drainer-worker/workerproto"
)

// fakeBrowser writes an executable that answers --version like Chromium.
func fakeBrowser(t *testing.T, dir string) string {
	t.Helper()
	p := filepath.Join(dir, "chromium")
	writeFile(t, p, "#!/bin/sh\n[ \"$1\" = --version ] && echo 'Chromium 154.0.8037.92 built on Debian'\nexit 0\n", 0o755)
	return p
}

// Decision B1: agent_browser and agent_deny_sockets, their refusals, and
// the four Wire fields.
func TestPolicyBrowserKeys(t *testing.T) {
	dir := t.TempDir()
	browser := fakeBrowser(t, dir)
	path := filepath.Join(dir, "policy.toml")
	writeFile(t, path, policyBase+"agent_browser = \""+browser+"\"\nagent_deny_sockets = [\"/run/example\", \"/run/other.sock\"]\nagent_allowed_domains = [\"registry.npmjs.org\"]\nmax_cost_usd = 7\n", 0o644)
	p := LoadPolicy(path, false, rootUID)
	if !p.Valid || p.AgentBrowser != browser || strings.Join(p.DenySockets, ",") != "/run/example,/run/other.sock" {
		t.Fatalf("policy %+v", p)
	}
	w := p.Wire()
	if w.Browser != browser || w.MaxCostUSD != 7 || strings.Join(w.AllowedDomains, ",") != "registry.npmjs.org" || w.MaskedSockets == nil || len(w.MaskedSockets) != 0 {
		t.Fatalf("wire %+v", w)
	}
	// Absent: off, and the wire lists stay non-nil.
	writeFile(t, path, policyBase, 0o644)
	if w := LoadPolicy(path, false, rootUID).Wire(); w.Browser != "" || w.AllowedDomains == nil || w.MaskedSockets == nil {
		t.Fatalf("wire without browser %+v", w)
	}
	// A browser that does not exist yet leaves the policy valid
	// (readiness says missing).
	writeFile(t, path, policyBase+"agent_browser = \"/usr/lib/no-such/chromium\"\n", 0o644)
	if p := LoadPolicy(path, false, rootUID); !p.Valid {
		t.Fatalf("missing browser invalidated the policy: %s", p.Reason)
	}
	many := make([]string, MaxDenySockets+1)
	for i := range many {
		many[i] = "\"/run/s" + strings.Repeat("x", i+1) + "\""
	}
	groupW := filepath.Join(dir, "groupw")
	writeFile(t, groupW, "#!/bin/sh\n", 0o775)
	if err := os.Chmod(groupW, 0o775); err != nil {
		t.Fatal(err)
	}
	notExec := filepath.Join(dir, "plain")
	writeFile(t, notExec, "x", 0o644)
	for body, reason := range map[string]string{
		"agent_browser = \"chromium\"\n":                                 "bad_agent_browser",
		"agent_browser = \"/usr/bin/../bin/chromium\"\n":                 "bad_agent_browser",
		"agent_browser = \"" + groupW + "\"\n":                           "bad_agent_browser",
		"agent_browser = \"" + notExec + "\"\n":                          "bad_agent_browser",
		"agent_browser = \"" + dir + "\"\n":                              "bad_agent_browser",
		"agent_deny_sockets = [\"run/x\"]\n":                             "bad_agent_deny_sockets",
		"agent_deny_sockets = [\"/\"]\n":                                 "bad_agent_deny_sockets",
		"agent_deny_sockets = [\"/run/x/\"]\n":                           "bad_agent_deny_sockets",
		"agent_deny_sockets = [\"/run/x\", \"/run/x\"]\n":                "bad_agent_deny_sockets",
		"agent_deny_sockets = [" + strings.Join(many, ", ") + "]\n":      "bad_agent_deny_sockets",
		"agent_deny_sockets = [" + strings.Join(many[:32], ", ") + "]\n": "",
		"agent_browser = \"" + browser + "\"\nagent_deny_sockets = []\n": "",
	} {
		writeFile(t, path, policyBase+body, 0o644)
		p := LoadPolicy(path, false, rootUID)
		if p.Reason != reason || p.Valid != (reason == "") {
			t.Errorf("%q: valid=%v reason=%q", body, p.Valid, p.Reason)
		}
		if reason != "" && (p.Wire().Browser != "" || len(p.Wire().AllowedDomains) != 0) {
			t.Errorf("%q: an invalid policy's wire carries values: %+v", body, p.Wire())
		}
	}
	// Not root's: refused, unless the check is syntax-only.
	writeFile(t, path, policyBase+"agent_browser = \""+browser+"\"\n", 0o644)
	notRoot := func(os.FileInfo) (uint32, bool) { return 1000, true }
	if r := browserFileReason(browser, notRoot); r != workerproto.ReasonUnsafe {
		t.Fatalf("non-root browser: %q", r)
	}
	if p := LoadPolicy(path, true, nil); !p.Valid {
		t.Fatalf("syntax-only check refused the browser: %s", p.Reason)
	}
}

// scanRoot makes a short temp root (unix socket paths are limited to about
// 100 bytes) and returns it with a listener factory.
func scanRoot(t *testing.T) (string, func(path string) string) {
	t.Helper()
	root, err := os.MkdirTemp("/tmp", "dds")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	root, _ = filepath.EvalSymlinks(root)
	listen := func(rel string) string {
		p := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		l, err := net.Listen("unix", p)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = l.Close() })
		return p
	}
	return root, listen
}

// Decision B2, as built: which sockets count, what is masked, and the
// bounds. Ownership and access are injected: a test user cannot create a
// socket owned by someone else.
func TestSocketScan(t *testing.T) {
	root, listen := scanRoot(t)
	listen("example/agent.sock")    // foreign, in a dir → the dir
	listen("top.sock")              // foreign, directly in the root → the file
	listen("own/mine.sock")         // the worker's own → ignored
	listen("private/locked.sock")   // not writable → ignored
	listen("deep/a/b/c/d/e/f.sock") // below the depth bound → the bound dir
	if err := os.MkdirAll(filepath.Join(root, "xonly"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(root, "example"), filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}
	state := filepath.Join(root, "state", "dd")
	listen("state/dd/x.sock") // would hide the worker's state → dropped
	const own = 4242
	scan := socketScan{
		roots: []string{root, filepath.Join(root, "missing")}, own: own, protect: []string{state},
		extra:    []string{"/run/example-extra", filepath.Join(root, "example", "sub")},
		maxDepth: 4, maxEntries: 1000, budget: time.Minute,
		uidOf: func(fi os.FileInfo) (uint32, bool) {
			if strings.HasPrefix(fi.Name(), "own") || strings.HasPrefix(fi.Name(), "mine") {
				return own, true
			}
			return 0, true
		},
		access: func(path string, mode uint32) bool {
			switch {
			case strings.Contains(path, "locked") && mode == accessW:
				return false
			case strings.HasSuffix(path, "xonly") && mode == accessR:
				return false
			}
			return true
		},
	}
	masked, dropped, complete := scan.run()
	want := []string{
		"/run/example-extra",
		filepath.Join(root, "deep", "a", "b", "c"),
		filepath.Join(root, "example"),
		filepath.Join(root, "top.sock"),
		filepath.Join(root, "xonly"),
	}
	slices.Sort(want)
	if !complete || strings.Join(masked, "\n") != strings.Join(want, "\n") {
		t.Fatalf("complete=%v masked:\n%s\nwant:\n%s", complete, strings.Join(masked, "\n"), strings.Join(want, "\n"))
	}
	if strings.Join(dropped, ",") != filepath.Join(root, "state") && strings.Join(dropped, ",") != state {
		t.Fatalf("dropped %v", dropped)
	}
	// The entry bound and the time budget leave the scan incomplete.
	scan.maxEntries = 3
	if _, _, complete := scan.run(); complete {
		t.Fatal("entry bound: complete")
	}
	scan.maxEntries, scan.budget = 1000, 0
	clock := time.Now()
	scan.now = func() time.Time { clock = clock.Add(time.Second); return clock }
	if _, _, complete := scan.run(); complete {
		t.Fatal("time budget: complete")
	}
}

// A root that is a symlink is walked once, under its real path.
func TestSocketScanResolvesRoots(t *testing.T) {
	root, listen := scanRoot(t)
	listen("d/s.sock")
	alias := root + "-alias"
	if err := os.Symlink(root, alias); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Remove(alias) })
	scan := socketScan{roots: []string{alias, root}, own: 4242, maxDepth: 4, maxEntries: 100, budget: time.Minute,
		uidOf: func(os.FileInfo) (uint32, bool) { return 0, true }, access: func(string, uint32) bool { return true }}
	masked, _, complete := scan.run()
	if !complete || strings.Join(masked, ",") != filepath.Join(root, "d") {
		t.Fatalf("masked %v complete %v", masked, complete)
	}
}

func TestCollapse(t *testing.T) {
	got := collapse([]string{"/run/a/x", "/run/a-b", "/run/a", "/run/a", "/tmp/s.sock"})
	if strings.Join(got, ",") != "/run/a,/run/a-b,/tmp/s.sock" {
		t.Fatalf("collapse %v", got)
	}
}

// B4: request ∩ policy; no request keeps the policy's list (rc2).
func TestIntersectHosts(t *testing.T) {
	allowed := []string{"registry.npmjs.org", "api.example.com"}
	if keep, dropped := intersectHosts(nil, allowed); strings.Join(keep, ",") != "registry.npmjs.org,api.example.com" || dropped != nil {
		t.Fatalf("no request: %v %v", keep, dropped)
	}
	keep, dropped := intersectHosts([]string{"api.example.com", "evil.example.org"}, allowed)
	if strings.Join(keep, ",") != "api.example.com" || strings.Join(dropped, ",") != "evil.example.org" {
		t.Fatalf("request: %v %v", keep, dropped)
	}
	if keep, _ := intersectHosts([]string{"evil.example.org"}, nil); len(keep) != 0 {
		t.Fatalf("no policy hosts, yet %v", keep)
	}
}

func TestValidatePreview(t *testing.T) {
	ok := []dexec.AppSettings{
		{},
		{Browser: true},
		{PreviewCmd: "pnpm dev --port 5173 --strictPort", PreviewURL: "http://127.0.0.1:5173"},
		{PreviewCmd: "x", PreviewURL: "http://localhost:3000/"},
		{PreviewCmd: "x", PreviewURL: "http://[::1]:8080"},
		{AgentHosts: []string{"registry.npmjs.org"}},
	}
	for _, a := range ok {
		if err := validatePreview(a); err != nil {
			t.Errorf("%+v: %v", a, err)
		}
	}
	bad := []dexec.AppSettings{
		{PreviewCmd: "x"},
		{PreviewURL: "http://127.0.0.1:5173"},
		{PreviewCmd: "a\nb", PreviewURL: "http://127.0.0.1:5173"},
		{PreviewCmd: strings.Repeat("x", 513), PreviewURL: "http://127.0.0.1:5173"},
		{PreviewCmd: "x", PreviewURL: "https://127.0.0.1:5173"},
		{PreviewCmd: "x", PreviewURL: "http://example.com:5173"},
		{PreviewCmd: "x", PreviewURL: "http://127.0.0.1"},
		{PreviewCmd: "x", PreviewURL: "http://127.0.0.1:05173"},
		{PreviewCmd: "x", PreviewURL: "http://127.0.0.1:70000"},
		{PreviewCmd: "x", PreviewURL: "http://u@127.0.0.1:5173"},
		{PreviewCmd: "x", PreviewURL: "http://127.0.0.1:5173/app"},
		{PreviewCmd: "x", PreviewURL: "http://127.0.0.1:5173/?a=1"},
		{AgentHosts: []string{"*.npmjs.org"}},
	}
	for _, a := range bad {
		if err := validatePreview(a); err == nil {
			t.Errorf("%+v accepted", a)
		}
	}
}

// Readiness: the browser tool only with agent_browser; browser.v1 always.
func TestBrowserReadiness(t *testing.T) {
	dir := t.TempDir()
	browser := fakeBrowser(t, dir)
	h := newHarness(t, harnessOpts{policy: defaultPolicy + "agent_browser = \"" + browser + "\"\n", noStart: true})
	h.start(Timing{})
	hello := h.fs.frames(workerproto.TypeHello)
	var f workerproto.Hello
	if err := json.Unmarshal(hello[len(hello)-1].raw, &f); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(f.Capabilities, workerproto.CapBrowserV1) || f.Policy.Browser != browser {
		t.Fatalf("hello caps %v policy %+v", f.Capabilities, f.Policy)
	}
	if err := f.Validate(); err != nil {
		t.Fatalf("hello invalid: %v", err)
	}
	var bt *workerproto.ToolStatus
	for i := range f.Tools {
		if f.Tools[i].Name == workerproto.ToolBrowser {
			bt = &f.Tools[i]
		}
	}
	if bt == nil {
		t.Fatalf("no browser tool: %+v", f.Tools)
	}
	switch runtime.GOOS {
	case "linux":
		// The real scan of this machine's roots may or may not finish; the
		// version comes either way.
		if bt.Version != "154.0.8037" || (!bt.Ready && bt.Reason != workerproto.ReasonScanIncomplete) {
			t.Fatalf("browser %+v", bt)
		}
	default:
		if bt.Ready || bt.Reason != workerproto.ReasonUnsupportedOS {
			t.Fatalf("browser on %s: %+v", runtime.GOOS, bt)
		}
	}
	// checkBrowser itself: missing and unsafe files, a failing --version.
	if runtime.GOOS == "linux" {
		ctx := context.Background()
		if st := checkBrowser(ctx, filepath.Join(dir, "none"), rootUID, true); st.Ready || st.Reason != workerproto.ReasonMissing {
			t.Fatalf("missing %+v", st)
		}
		if st := checkBrowser(ctx, browser, func(os.FileInfo) (uint32, bool) { return 1000, true }, true); st.Ready || st.Reason != workerproto.ReasonUnsafe {
			t.Fatalf("unsafe %+v", st)
		}
		broken := filepath.Join(dir, "broken")
		writeFile(t, broken, "#!/bin/sh\nexit 3\n", 0o755)
		if st := checkBrowser(ctx, broken, rootUID, true); st.Ready || st.Reason != workerproto.ReasonMissing {
			t.Fatalf("broken %+v", st)
		}
		if st := checkBrowser(ctx, browser, rootUID, false); st.Ready || st.Reason != workerproto.ReasonScanIncomplete || st.Version == "" {
			t.Fatalf("incomplete scan %+v", st)
		}
		if st := checkBrowser(ctx, browser, rootUID, true); !st.Ready || st.Version != "154.0.8037" {
			t.Fatalf("ready %+v", st)
		}
	}
}

// browserAgent is a fake claude that records its settings file, its preview
// env and its stdin, then writes the fix artifacts.
func browserAgent(t *testing.T) string {
	return "while [ $# -gt 0 ]; do [ \"$1\" = --settings ] && cp \"$2\" settings.json; shift; done\n" +
		"env | grep '^DD_PREVIEW_' | sort > preview-env.txt\n" +
		"readlink /proc/self/fd/0 > stdin.txt 2>/dev/null\n" +
		"if read -r _; then echo data > stdin-read.txt; else echo eof > stdin-read.txt; fi\n" +
		fixArtifacts(testDefect) + "cat '" + claudeFixture(t, "c1-basic") + "'\nexit 0\n"
}

// readyBrowser makes the harness's worker see a browser in the given state
// and scan socketRoot instead of the machine's roots (a test user cannot
// plant a foreign socket, and only Linux runs the real check).
func readyBrowser(h *harness, socketRoot string, ready bool) {
	st := workerproto.ToolStatus{Name: workerproto.ToolBrowser, Version: "154.0.8037", Ready: true}
	if !ready {
		st = workerproto.ToolStatus{Name: workerproto.ToolBrowser, Reason: workerproto.ReasonMissing}
	}
	h.w.mu.Lock()
	h.w.tools = slices.DeleteFunc(h.w.tools, func(t workerproto.ToolStatus) bool { return t.Name == workerproto.ToolBrowser })
	h.w.tools = append(h.w.tools, st)
	h.w.scanOverride = func(s socketScan) socketScan {
		s.roots = []string{socketRoot}
		s.own = 4242
		s.uidOf = func(os.FileInfo) (uint32, bool) { return 0, true }
		return s
	}
	h.w.mu.Unlock()
}

// B1–B7 through the real worker loop: a browser job on a host that allows
// it gets the settings switch and masks, dd-shot, the Screenshots brief,
// the preview env and the narrowed hosts; its agent's stdin is /dev/null.
func TestBrowserJobEndToEnd(t *testing.T) {
	browser := fakeBrowser(t, t.TempDir())
	root, listen := scanRoot(t)
	listen("example/agent.sock")
	h := newHarness(t, harnessOpts{agent: browserAgent(t),
		policy: defaultPolicy + "agent_browser = \"" + browser + "\"\nagent_allowed_domains = [\"registry.npmjs.org\", \"api.example.com\"]\n"})
	readyBrowser(h, root, true)
	spec := fixSpec("bjob_wb_on")
	spec.App.Browser = true
	spec.App.PreviewCmd, spec.App.PreviewURL = "pnpm dev --port 5173 --strictPort", "http://127.0.0.1:5173"
	spec.App.AgentHosts = []string{"api.example.com", "evil.example.org"}
	h.offerJob(spec, nil)
	h.waitDone(spec.JobID, 60*time.Second)
	log := joinLines(h.fs.jobEvents(spec.JobID))
	if !strings.Contains(log, "host policy: network host dropped: evil.example.org") || strings.Contains(log, "browser not ready") {
		t.Fatalf("job log:\n%s", log)
	}
	handoff := h.paths.Handoff(spec.JobID)
	settings, err := os.ReadFile(filepath.Join(handoff, "settings.json"))
	if err != nil {
		t.Fatal(err)
	}
	var s struct {
		Sandbox struct {
			Filesystem struct {
				DenyRead []string `json:"denyRead"`
			} `json:"filesystem"`
			Network map[string]any `json:"network"`
		} `json:"sandbox"`
	}
	if err := json.Unmarshal(settings, &s); err != nil {
		t.Fatal(err)
	}
	if s.Sandbox.Network["allowAllUnixSockets"] != true || !slices.Contains(s.Sandbox.Filesystem.DenyRead, "/"+filepath.Join(root, "example")) {
		t.Fatalf("settings:\n%s", settings)
	}
	if d, _ := json.Marshal(s.Sandbox.Network["allowedDomains"]); string(d) != `["api.example.com"]` {
		t.Fatalf("domains %s", d)
	}
	env, _ := os.ReadFile(filepath.Join(handoff, "preview-env.txt"))
	if string(env) != "DD_PREVIEW_CMD=pnpm dev --port 5173 --strictPort\nDD_PREVIEW_URL=http://127.0.0.1:5173\n" {
		t.Fatalf("preview env %q", env)
	}
	shot, err := os.ReadFile(filepath.Join(handoff, dexec.DDShotName))
	if err != nil || !strings.Contains(string(shot), "dd_browser='"+browser+"'") {
		t.Fatalf("dd-shot: %v", err)
	}
	brief, _ := os.ReadFile(filepath.Join(handoff, "BRIEF.md"))
	if !strings.Contains(string(brief), "## Screenshots") || !strings.Contains(string(brief), "Only if `dd-shot` failed") {
		t.Fatalf("BRIEF.md:\n%s", brief)
	}
	argv, _ := os.ReadFile(h.argv)
	if !strings.Contains(string(argv), "SCREENSHOTS (this host has a headless browser)") {
		t.Fatalf("prompt:\n%s", argv)
	}
	if in, _ := os.ReadFile(filepath.Join(handoff, "stdin-read.txt")); string(in) != "eof\n" {
		t.Fatalf("agent stdin read %q", in)
	}
	if runtime.GOOS == "linux" {
		if in, _ := os.ReadFile(filepath.Join(handoff, "stdin.txt")); string(in) != "/dev/null\n" {
			t.Fatalf("agent stdin is %q", in)
		}
	}
}

// B4, B6: each reason the worker drops a Browser request, with its note,
// and a job without Browser keeps the policy's whole host list (rc2).
func TestBrowserDropped(t *testing.T) {
	browser := fakeBrowser(t, t.TempDir())
	cases := []struct {
		name, policy, note string
		ready              bool
		mutate             func(*dexec.JobSpec)
	}{
		{"no agent_browser", defaultPolicy, "host policy: browser off (no agent_browser)", true, nil},
		{"not ready", defaultPolicy + "agent_browser = \"" + browser + "\"\n", "browser not ready: missing", false, nil},
		{"test run", defaultPolicy + "agent_browser = \"" + browser + "\"\n", "browser: fix jobs only", true, func(s *dexec.JobSpec) {
			s.Kind, s.Fix = dexec.KindTestRun, nil
			s.Worktrees[0].Branch = ""
			s.TestRun = &dexec.TestRunSpec{RunID: "run1"}
		}},
	}
	for i, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := newHarness(t, harnessOpts{policy: c.policy})
			root, _ := scanRoot(t)
			readyBrowser(h, root, c.ready)
			spec := fixSpec("bjob_wb_drop" + string(rune('a'+i)))
			spec.App.Browser = true
			if c.mutate != nil {
				c.mutate(&spec)
			}
			d := h.w.applyPolicy(spec, "", "", "")
			if d.reason != "" {
				t.Fatalf("declined %s %s", d.reason, d.detail)
			}
			if d.spec.App.Browser || d.browser != "" || !slices.ContainsFunc(d.notes, func(n string) bool { return strings.HasPrefix(n, c.note) }) {
				t.Fatalf("decision browser=%v %q notes %v", d.spec.App.Browser, d.browser, d.notes)
			}
		})
	}
	h := newHarness(t, harnessOpts{policy: defaultPolicy + "agent_allowed_domains = [\"registry.npmjs.org\"]\n"})
	d := h.w.applyPolicy(fixSpec("bjob_wb_plain"), "", "", "")
	if d.reason != "" || strings.Join(d.domains, ",") != "registry.npmjs.org" || len(d.notes) != 0 {
		t.Fatalf("plain job: %+v", d)
	}
	bad := fixSpec("bjob_wb_bad")
	bad.App.PreviewCmd = "pnpm dev"
	if d := h.w.applyPolicy(bad, "", "", ""); d.reason != workerproto.DeclineBadSpec {
		t.Fatalf("preview without url: %+v", d)
	}
}
