package sandbox

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
)

func TestArgsLayout(t *testing.T) {
	root := t.TempDir()
	root, _ = filepath.EvalSymlinks(root)
	state := filepath.Join(root, "state")
	wt := filepath.Join(state, "jobs", "j1", "worktrees", "app")
	gitDir := filepath.Join(state, "repos", "app", ".git")
	secret := filepath.Join(root, "secrets.env")
	home := filepath.Join(root, "home")
	for _, d := range []string{wt, gitDir, home} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(secret, []byte("K=v"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wt, ".git"), []byte("gitdir: x"), 0o644); err != nil {
		t.Fatal(err)
	}
	sock := filepath.Join(root, "egress", "s")
	l := Launcher{Bwrap: "/usr/bin/bwrap", Socat: "/usr/bin/socat"}
	args, err := l.Args(Run{
		Command: "make test", Dir: wt, Home: home,
		Hide:      []string{state, secret, filepath.Join(root, "missing")},
		Roots:     []string{filepath.Join(state, "jobs", "j1"), filepath.Join(state, "repos")},
		Writable:  []string{wt},
		ReadOnly:  []string{gitDir, filepath.Join(wt, ".git")},
		Env:       []string{"PATH=/usr/bin", "CI=1", "bad"},
		ProxySock: sock,
	})
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(args, " ")
	want := []string{
		"--unshare-all --unshare-net --die-with-parent --new-session --cap-drop ALL --ro-bind / / --proc /proc --dev /dev",
		"--bind " + wt + " " + wt,
		"--ro-bind " + gitDir + " " + gitDir,
		"--ro-bind " + filepath.Join(wt, ".git") + " " + filepath.Join(wt, ".git"),
		"--ro-bind " + filepath.Join(root, "egress") + " /run/dd-verify",
		"--chdir " + wt,
		"--clearenv --setenv PATH /usr/bin --setenv CI 1 --",
	}
	last := -1
	for _, w := range want {
		i := strings.Index(joined, w)
		if i < 0 {
			t.Fatalf("missing %q in\n%s", w, joined)
		}
		if i < last {
			t.Fatalf("%q out of order in\n%s", w, joined)
		}
		last = i
	}
	// Hidden by a tmpfs on the path or on an ancestor (under /tmp, as on
	// Linux, the /tmp tmpfs covers them and no nested one is added).
	for _, p := range []string{home, state, secret} {
		if !hiddenIn(args, p) {
			t.Errorf("%s not hidden in\n%s", p, joined)
		}
	}
	if strings.Contains(joined, "missing") {
		t.Errorf("a path that does not exist was mounted: %s", joined)
	}
	if strings.Contains(joined, "bad") {
		t.Errorf("an env entry without = was passed: %s", joined)
	}
	if got := args[len(args)-4:]; !slices.Equal(got, []string{"/usr/bin/socat", "18080", "/run/dd-verify/s", "make test"}) {
		t.Errorf("bridge args %v", got)
	}
	for _, fixed := range FixedHidden {
		if real, err := filepath.EvalSymlinks(fixed); err == nil {
			if !strings.Contains(joined, "--tmpfs "+real) {
				t.Errorf("%s not hidden", fixed)
			}
		}
	}
}

func TestArgsNoProxyNoBridge(t *testing.T) {
	args, err := Launcher{Bwrap: "bwrap"}.Args(Run{Command: "true"})
	if err != nil {
		t.Fatal(err)
	}
	if got := args[len(args)-4:]; !slices.Equal(got, []string{"-", "18080", "-", "true"}) {
		t.Fatalf("tail %v", got)
	}
}

// M5, L9: with a bwrap that has them, every hiding tmpfs is size-capped
// and no user namespace can be made inside.
func TestArgsSizeAndUserns(t *testing.T) {
	args, err := Launcher{Bwrap: "bwrap", Size: true, DisableUserns: true}.Args(Run{Command: "true"})
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "--unshare-user --disable-userns") {
		t.Errorf("no --disable-userns: %s", joined)
	}
	for i, a := range args {
		if a == "--tmpfs" && (i < 2 || args[i-2] != "--size") {
			t.Errorf("tmpfs %s without --size", args[i+1])
		}
	}
	if !strings.Contains(wrapperScript, "ulimit -n 8192") || !strings.Contains(wrapperScript, "ulimit -u 2048") || !strings.Contains(wrapperScript, "ulimit -f 16777216") {
		t.Errorf("limits: %s", wrapperScript)
	}
}

// H1/H2: a bound path with a symlink on it, outside the roots, or holding
// a hidden path is refused, never mounted.
func TestArgsRefusesBadBinds(t *testing.T) {
	root, _ := filepath.EvalSymlinks(t.TempDir())
	job := filepath.Join(root, "jobs", "j1")
	wt := filepath.Join(job, "worktrees", "app")
	secret := filepath.Join(root, "credential")
	for _, d := range []string{wt, secret, filepath.Join(job, "real")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	link := filepath.Join(wt, ".git")
	if err := os.Symlink(secret, link); err != nil {
		t.Fatal(err)
	}
	via := filepath.Join(job, "via")
	if err := os.Symlink(filepath.Join(job, "real"), via); err != nil {
		t.Fatal(err)
	}
	l := Launcher{Bwrap: "bwrap"}
	for _, c := range []struct {
		name     string
		writable []string
		readOnly []string
		hide     []string
		why      string
	}{
		{"symlink", nil, []string{link}, nil, "a symlink"},
		{"through a symlink", []string{filepath.Join(via, "x")}, nil, nil, "missing"},
		{"through a symlinked dir", []string{via}, nil, nil, "a symlink"},
		{"outside the roots", []string{secret}, nil, nil, "outside the job's dirs"},
		{"holds a hidden path", []string{job}, nil, []string{filepath.Join(job, "real")}, "a hidden path, or one holding it"},
		{"is a hidden path", []string{wt}, nil, []string{wt}, "a hidden path, or one holding it"},
		{"relative", []string{"jobs/j1"}, nil, nil, "not an absolute clean path"},
	} {
		_, err := l.Args(Run{Command: "true", Roots: []string{job}, Writable: c.writable, ReadOnly: c.readOnly, Hide: c.hide})
		if !errors.Is(err, ErrUnavailable) || !strings.Contains(err.Error(), c.why) {
			t.Errorf("%s: %v, want %q", c.name, err, c.why)
		}
	}
	if err := os.Mkdir(filepath.Join(job, "real", "x"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := l.Args(Run{Command: "true", Roots: []string{job}, Writable: []string{filepath.Join(via, "x")}}); err == nil || !strings.Contains(err.Error(), "through a symlink") {
		t.Errorf("via a symlinked parent: %v", err)
	}
	if _, err := l.Args(Run{Command: "true", Roots: []string{job}, Writable: []string{wt}}); err != nil {
		t.Errorf("a good bind refused: %v", err)
	}
	// A root under a system link (as /var is on macOS) is the worker's:
	// allowed, and bound by its real path.
	sysLink := filepath.Join(root, "sys")
	if err := os.Symlink(root, sysLink); err != nil {
		t.Fatal(err)
	}
	args, err := l.Args(Run{Command: "true", Roots: []string{filepath.Join(sysLink, "jobs", "j1")}, Writable: []string{filepath.Join(sysLink, "jobs", "j1", "worktrees", "app")}})
	if err != nil || !strings.Contains(strings.Join(args, " "), "--bind "+wt+" "+wt) {
		t.Errorf("root under a link: %v %v", err, args)
	}
}

func TestFindOffLinux(t *testing.T) {
	if runtime.GOOS == "linux" {
		t.Skip("linux")
	}
	_, err := Find()
	if !errors.Is(err, ErrUnavailable) || Reason(err) != "bwrap: not supported on "+runtime.GOOS {
		t.Fatalf("%v / %q", err, Reason(err))
	}
}

func TestCommandWithoutBwrapRefuses(t *testing.T) {
	_, err := Launcher{}.Command(t.Context(), Run{Command: "true"})
	if !errors.Is(err, ErrUnavailable) {
		t.Fatalf("%v", err)
	}
	if err := (Launcher{}).SelfTest(t.Context()); !errors.Is(err, ErrUnavailable) || Reason(err) != "bwrap: not found" {
		t.Fatalf("%v", err)
	}
}

// Nested hidden paths get one tmpfs, on the outermost.
func TestArgsNestedHideOnce(t *testing.T) {
	root, _ := filepath.EvalSymlinks(t.TempDir())
	inner := filepath.Join(root, "a", "b")
	if err := os.MkdirAll(inner, 0o755); err != nil {
		t.Fatal(err)
	}
	args, err := Launcher{Bwrap: "bwrap"}.Args(Run{Command: "true", Hide: []string{inner, filepath.Join(root, "a")}})
	if err != nil {
		t.Fatal(err)
	}
	if slices.Contains(args, inner) {
		t.Fatalf("nested %s mounted again: %v", inner, args)
	}
	if !hiddenIn(args, inner) {
		t.Fatalf("%s not hidden: %v", inner, args)
	}
}

// hiddenIn reports whether args (from Args) hide p: a tmpfs on p or on one
// of its parents, or /dev/null over p.
func hiddenIn(args []string, p string) bool {
	for i := 0; i+1 < len(args); i++ {
		switch {
		case args[i] == "--tmpfs" && under(p, []string{args[i+1]}):
			return true
		case args[i] == "--ro-bind" && args[i+1] == "/dev/null" && i+2 < len(args) && args[i+2] == p:
			return true
		}
	}
	return false
}

func TestSeccompArgAndFailClosed(t *testing.T) {
	args, err := Launcher{Bwrap: "bwrap", Seccomp: []byte{0}}.Args(Run{Command: "true"})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(args, "--seccomp") || args[slices.Index(args, "--seccomp")+1] != "4" {
		t.Fatalf("no --seccomp 4 in %q", args)
	}
	_, err = Launcher{Bwrap: "/bin/true"}.Command(t.Context(), Run{Command: "true"})
	if !errors.Is(err, ErrUnavailable) || Reason(err) != "seccomp: no filter" {
		t.Fatalf("a launcher without a filter: %v", err)
	}
}

func TestFilterLoadedNeedsTheProbe(t *testing.T) {
	// A personality the verify filter refuses got through: no filter.
	if filterLoaded("F:99\nM:2\nP:allowed\n") {
		t.Fatal("an allowed personality passed")
	}
	if filterLoaded("F:99\nM:0\nP:denied\n") {
		t.Fatal("seccomp mode 0 passed")
	}
}
