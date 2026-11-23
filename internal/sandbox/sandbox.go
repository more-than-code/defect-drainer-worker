// Package sandbox runs one verify command inside bubblewrap.
//
// The layout: the host read-only, with DD's state, the host owner's secrets,
// the socket dirs, $HOME and /tmp hidden behind empty, size-capped tmpfs
// mounts; the job's own worktrees and a per-run tmp and cache dir bound back
// writable; each clone's .git read-only; fresh /proc and /dev. No network of
// its own: a socat bridge inside forwards 127.0.0.1:ProxyPort to the egress
// proxy's Unix socket, bound in read-only. The environment is built by the
// caller, never inherited.
//
// Every bound path is checked first: no symlink anywhere on it, inside one
// of the run's Roots, and never equal to or holding a hidden path. A path
// that fails is a layout the sandbox refuses (ErrUnavailable), never a
// mount: a symlink planted by the agent can't bind a secret in.
//
// The command runs in a new PID namespace whose first process is bwrap
// itself, with --die-with-parent: killing the launcher kills every process
// the command started. bwrap loads a seccomp filter (seccomp.go) before the
// wrapper runs: no mounts, namespaces, kernel code, BPF, perf or keyrings.
package sandbox

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// ProxyPort is where the egress bridge listens inside the sandbox. The
// sandbox has its own network namespace, so the port is always free.
const ProxyPort = 18080

// ProxyURL is the value of HTTP(S)_PROXY inside the sandbox.
var ProxyURL = "http://127.0.0.1:" + strconv.Itoa(ProxyPort)

// sockDir is where the proxy's socket dir appears inside the sandbox.
const sockDir = "/run/dd-verify"

// FixedHidden are hidden in every sandbox: the socket dirs and /tmp.
var FixedHidden = []string{"/run", "/var/run", "/tmp"}

// hiddenSize caps each hiding tmpfs (bwrap --size): writes there are
// thrown away, and must not fill the host's memory.
const hiddenSize = 64 << 20

// Per-command limits, set by the wrapper inside the sandbox (soft and hard,
// so the command can't raise them): open files, processes and the largest
// file it may write (in 512-byte blocks: 8 GiB).
const (
	limitNoFile = 8192
	limitNProc  = 2048
	limitFSize  = 16 << 20
)

// waitDelay bounds how long Wait waits for the output pipes after the
// sandbox is killed.
const waitDelay = 5 * time.Second

// ErrUnavailable wraps every reason the sandbox cannot start.
var ErrUnavailable = errors.New("verify sandbox unavailable")

// Launcher is the host's bwrap and socat, and the bwrap options it has.
type Launcher struct {
	Bwrap string
	Socat string
	// Size: bwrap takes --size (caps a tmpfs). DisableUserns: bwrap takes
	// --disable-userns (no further user namespaces inside).
	Size          bool
	DisableUserns bool
	// Seccomp is the filter bwrap loads with --seccomp (seccompFilter). A
	// launcher without one refuses to run: the sandbox fails closed.
	Seccomp []byte
}

// seccompFD is the fd bwrap reads the filter from (fd 3 is "ready").
const seccompFD = 4

// Find looks the tools up on PATH. A host without them, or not Linux, has
// no sandbox, and verify is skipped there.
func Find() (Launcher, error) {
	if runtime.GOOS != "linux" {
		return Launcher{}, fmt.Errorf("%w: bwrap: not supported on %s", ErrUnavailable, runtime.GOOS)
	}
	bw, err := exec.LookPath("bwrap")
	if err != nil {
		return Launcher{}, fmt.Errorf("%w: bwrap: not found", ErrUnavailable)
	}
	so, err := exec.LookPath("socat")
	if err != nil {
		return Launcher{}, fmt.Errorf("%w: socat: not found", ErrUnavailable)
	}
	filter, err := seccompFilter()
	if err != nil {
		return Launcher{}, fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	l := Launcher{Bwrap: bw, Socat: so, Seccomp: filter}
	if help, err := exec.Command(bw, "--help").CombinedOutput(); err == nil {
		l.Size = strings.Contains(string(help), "--size")
		l.DisableUserns = strings.Contains(string(help), "--disable-userns")
	}
	return l, nil
}

// Run is one command and the layout it runs in. Paths are host paths.
type Run struct {
	// Command runs as `sh -c Command` in Dir.
	Command string
	Dir     string
	// Home is hidden like Hide; HOME should point at it.
	Home string
	// Hide are paths hidden behind an empty tmpfs (a dir) or /dev/null (a
	// file). FixedHidden and Home are added. A path that does not exist
	// is skipped: there is nothing to hide.
	Hide []string
	// Roots are where every Writable and ReadOnly path must lie (the
	// worker's job dir and clones dir).
	Roots []string
	// Writable are bound back read-write after the hiding: the job's
	// worktrees and the run's tmp and cache dirs.
	Writable []string
	// ReadOnly are bound read-only last, over anything writable: each
	// clone's .git and each worktree's .git file.
	ReadOnly []string
	// Env is the whole environment (KEY=VALUE).
	Env []string
	// ProxySock is the egress proxy's socket ("": no network at all).
	ProxySock string
}

// Args is the bwrap argument list for r (after the bwrap binary), or an
// ErrUnavailable when a bound path fails the layout checks.
func (l Launcher) Args(r Run) ([]string, error) {
	args := []string{
		"--unshare-all", "--unshare-net", "--die-with-parent", "--new-session", "--cap-drop", "ALL",
	}
	if l.DisableUserns {
		args = append(args, "--unshare-user", "--disable-userns")
	}
	args = append(args,
		"--ro-bind", "/", "/",
		"--proc", "/proc",
		"--dev", "/dev",
	)
	// Hide parents first, and skip what an earlier tmpfs already hides.
	type hidden struct {
		path string
		dir  bool
	}
	var hides []hidden
	for _, p := range append(append(append([]string{}, FixedHidden...), r.Home), r.Hide...) {
		if real, fi, ok := resolve(p); ok && real != "/" {
			hides = append(hides, hidden{real, fi.IsDir()})
		}
	}
	slices.SortStableFunc(hides, func(a, b hidden) int { return len(a.path) - len(b.path) })
	var done, allHidden []string
	for _, h := range hides {
		allHidden = append(allHidden, h.path)
		if under(h.path, done) {
			continue
		}
		done = append(done, h.path)
		switch {
		case h.dir && l.Size:
			args = append(args, "--size", strconv.Itoa(hiddenSize), "--tmpfs", h.path)
		case h.dir:
			args = append(args, "--tmpfs", h.path)
		default:
			args = append(args, "--ro-bind", "/dev/null", h.path)
		}
	}
	for _, b := range []struct {
		flag  string
		paths []string
	}{{"--bind", r.Writable}, {"--ro-bind", r.ReadOnly}} {
		for _, p := range b.paths {
			real, err := checkBind(p, r.Roots, allHidden)
			if err != nil {
				return nil, err
			}
			args = append(args, b.flag, real, real)
		}
	}
	if r.ProxySock != "" {
		args = append(args, "--ro-bind", filepath.Dir(r.ProxySock), sockDir)
	}
	if r.Dir != "" {
		args = append(args, "--chdir", r.Dir)
	}
	if len(l.Seccomp) > 0 {
		args = append(args, "--seccomp", strconv.Itoa(seccompFD))
	}
	args = append(args, "--clearenv")
	for _, kv := range r.Env {
		k, v, ok := strings.Cut(kv, "=")
		if ok && k != "" {
			args = append(args, "--setenv", k, v)
		}
	}
	socat, inner := "-", "-"
	if r.ProxySock != "" {
		socat, inner = l.Socat, sockDir+"/"+filepath.Base(r.ProxySock)
	}
	return append(args, "--", "/bin/sh", "-c", wrapperScript, "dd-verify", socat, strconv.Itoa(ProxyPort), inner, r.Command), nil
}

// checkBind is the layout rule for one bound path, and its real path: an
// absolute clean path inside one of roots, with no symlink at or below the
// root (where the agent may have planted one; the root itself is the
// worker's and may sit under a system link such as /var), there, and never
// a hidden path or one holding a hidden path.
func checkBind(p string, roots, hidden []string) (string, error) {
	refuse := func(why string) (string, error) {
		return "", fmt.Errorf("%w: layout: %s %s", ErrUnavailable, why, p)
	}
	if p == "" || !filepath.IsAbs(p) || filepath.Clean(p) != p {
		return refuse("not an absolute clean path:")
	}
	root := ""
	for _, r := range roots {
		if r != "" && under(p, []string{filepath.Clean(r)}) && len(r) > len(root) {
			root = filepath.Clean(r)
		}
	}
	if root == "" {
		return refuse("outside the job's dirs:")
	}
	rootReal, err := filepath.EvalSymlinks(root)
	if err != nil {
		return refuse("outside the job's dirs:")
	}
	fi, err := os.Lstat(p)
	if err != nil {
		return refuse("missing:")
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		return refuse("a symlink:")
	}
	want := rootReal + strings.TrimPrefix(p, root)
	if real, err := filepath.EvalSymlinks(p); err != nil || real != want {
		return refuse("a path through a symlink:")
	}
	for _, h := range hidden {
		if h == want || strings.HasPrefix(h, want+"/") {
			return refuse("a hidden path, or one holding it:")
		}
	}
	return want, nil
}

// wrapperScript sets the limits, starts the egress bridge when there is a
// proxy and waits until it answers, says "ready" on fd 3, then runs the
// command. $1 socat ("-": no proxy), $2 port, $3 socket, $4 command.
// Without "ready" on fd 3 the sandbox never got to the command: a setup
// failure, not the check's result.
var wrapperScript = fmt.Sprintf(`ulimit -n %d 2>/dev/null
ulimit -u %d 2>/dev/null
ulimit -f %d 2>/dev/null
if [ "$1" != - ]; then
	"$1" TCP-LISTEN:"$2",bind=127.0.0.1,fork,reuseaddr UNIX-CONNECT:"$3" 2>/dev/null 3>&- &
	i=0
	until "$1" -u OPEN:/dev/null TCP:127.0.0.1:"$2" 2>/dev/null 3>&-; do
		i=$((i+1))
		if [ "$i" -ge 200 ]; then echo "dd-verify: the egress bridge did not start" >&2; exit 125; fi
		sleep 0.05
	done
fi
echo ready >&3
exec 3>&-
exec /bin/sh -c "$4"`, limitNoFile, limitNProc, limitFSize)

// Proc is one sandboxed command.
type Proc struct {
	Cmd    *exec.Cmd
	readyR *os.File
	readyW *os.File
	filter *os.File
	ready  chan bool
}

// Command is the sandboxed command. Cancelling ctx kills the whole sandbox.
func (l Launcher) Command(ctx context.Context, r Run) (*Proc, error) {
	if l.Bwrap == "" || (r.ProxySock != "" && l.Socat == "") {
		return nil, fmt.Errorf("%w: bwrap: not found", ErrUnavailable)
	}
	if len(l.Seccomp) == 0 {
		return nil, fmt.Errorf("%w: seccomp: no filter", ErrUnavailable)
	}
	args, err := l.Args(r)
	if err != nil {
		return nil, err
	}
	filter, err := filterFile(l.Seccomp)
	if err != nil {
		return nil, fmt.Errorf("%w: seccomp: %v", ErrUnavailable, err)
	}
	readyR, readyW, err := os.Pipe()
	if err != nil {
		_ = filter.Close()
		return nil, fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	cmd := exec.CommandContext(ctx, l.Bwrap, args...)
	cmd.Dir = "/"
	cmd.Env = []string{}
	cmd.ExtraFiles = []*os.File{readyW, filter} // fd 3, fd seccompFD
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		// The group is bwrap; the command itself is in its own session
		// inside the PID namespace, which dies with bwrap.
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		return cmd.Process.Kill()
	}
	cmd.WaitDelay = waitDelay
	return &Proc{Cmd: cmd, readyR: readyR, readyW: readyW, filter: filter, ready: make(chan bool, 1)}, nil
}

// filterFile is the filter as a read end of a pipe holding all of it: a
// few hundred bytes, well under the pipe's buffer, so the write never
// blocks. bwrap reads it to EOF.
func filterFile(prog []byte) (*os.File, error) {
	r, w, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	_, err = w.Write(prog)
	if cerr := w.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		_ = r.Close()
		return nil, err
	}
	return r, nil
}

// Start starts the sandbox.
func (p *Proc) Start() error {
	err := p.Cmd.Start()
	_ = p.readyW.Close()
	_ = p.filter.Close()
	if err != nil {
		_ = p.readyR.Close()
		p.ready <- false
		return err
	}
	go func() {
		line, _ := bufio.NewReader(p.readyR).ReadString('\n')
		_ = p.readyR.Close()
		p.ready <- line == "ready\n"
	}()
	return nil
}

// Reached reports, after Wait, whether the sandbox got as far as the
// command. False is a setup failure: bwrap or the bridge failed to start.
func (p *Proc) Reached() bool {
	select {
	case ok := <-p.ready:
		p.ready <- ok
		return ok
	case <-time.After(waitDelay):
		return false
	}
}

func resolve(p string) (string, os.FileInfo, bool) {
	if p == "" || !filepath.IsAbs(p) {
		return "", nil, false
	}
	real, err := filepath.EvalSymlinks(p)
	if err != nil {
		return "", nil, false
	}
	fi, err := os.Stat(real)
	if err != nil {
		return "", nil, false
	}
	return real, fi, true
}

// under reports whether p is one of dirs or inside one.
func under(p string, dirs []string) bool {
	for _, d := range dirs {
		if p == d || strings.HasPrefix(p, d+"/") {
			return true
		}
	}
	return false
}
