package worker

import (
	"context"
	"errors"
	"io/fs"
	"os"
	osexec "os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/more-than-code/defect-drainer-worker/workerproto"
)

// browserFileReason checks policy agent_browser (B1): "" when it is a
// regular, executable file owned by root that only root can write;
// ReasonMissing when it does not exist; ReasonUnsafe otherwise. uid
// overrides the owner for tests, as for policy.toml.
func browserFileReason(path string, uid func(os.FileInfo) (uint32, bool)) string {
	fi, err := os.Stat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return workerproto.ReasonMissing
	}
	if err != nil || !fi.Mode().IsRegular() || fi.Mode().Perm()&0o111 == 0 || fi.Mode().Perm()&0o022 != 0 {
		return workerproto.ReasonUnsafe
	}
	owner, ok := ownerUID(fi, uid)
	if !ok || owner != 0 {
		return workerproto.ReasonUnsafe
	}
	return ""
}

// checkBrowser is the "browser" tool's readiness: the file check, then
// `<browser> --version` (stdin closed, from /, a timeout; stderr dropped),
// then a complete socket scan. Linux only: the sandbox recipe was proved
// with bubblewrap (probe P2).
func checkBrowser(ctx context.Context, path string, uid func(os.FileInfo) (uint32, bool), scanComplete bool) workerproto.ToolStatus {
	st := workerproto.ToolStatus{Name: workerproto.ToolBrowser}
	if runtime.GOOS != "linux" {
		st.Reason = workerproto.ReasonUnsupportedOS
		return st
	}
	if r := browserFileReason(path, uid); r != "" {
		st.Reason = r
		return st
	}
	out, err := runProbe(ctx, osexec.Command(path, "--version"))
	st.Version = versionRe.FindString(out)
	if err != nil || st.Version == "" {
		st.Version, st.Reason = "", workerproto.ReasonMissing
		return st
	}
	if !scanComplete {
		st.Reason = workerproto.ReasonScanIncomplete
		return st
	}
	st.Ready = true
	return st
}

// socketRoots are where host daemons put the unix sockets a browser job's
// sandbox could reach once it has unix sockets (B2).
var socketRoots = []string{"/run", "/var/run", "/tmp", "/dev/shm"}

// socketScan finds the host sockets the worker user could connect to and
// does not own, and returns what to mask (B2). The zero value of a func
// field means the real check.
type socketScan struct {
	roots []string
	// own is the worker's uid: its own sockets (the sandbox runtime's among
	// them) and directories are left alone.
	own uint32
	// protect are paths a mask must never be or contain (the state dir).
	protect []string
	// extra is policy agent_deny_sockets, masked as given.
	extra []string

	maxDepth   int
	maxEntries int
	budget     time.Duration

	uidOf  func(os.FileInfo) (uint32, bool)
	access func(path string, mode uint32) bool
	now    func() time.Time
}

// Scan bounds: a dir deeper than scanMaxDepth below a root that is not the
// worker's is masked whole; more than scanMaxEntries entries in a root or a
// walk longer than scanBudget leaves the scan incomplete.
const (
	scanMaxDepth   = 6
	scanMaxEntries = 20000
	scanBudget     = 3 * time.Second
)

// access(2) modes.
const (
	accessX = 1
	accessW = 2
	accessR = 4
)

func newSocketScan(p Paths, extra []string) socketScan {
	return socketScan{
		roots: socketRoots, own: uint32(os.Geteuid()), protect: []string{p.StateDir}, extra: extra,
		maxDepth: scanMaxDepth, maxEntries: scanMaxEntries, budget: scanBudget,
	}
}

func realAccess(path string, mode uint32) bool { return syscall.Access(path, mode) == nil }

// run walks the roots. It never follows a symlink and never connects to a
// socket (probing would poke the daemons). A socket counts when the worker
// does not own it and may write it (connect needs write on the socket file).
// A directory the worker may search but not list counts whole: its sockets
// can be reached by name. dropped are masks refused because they are or
// contain a protected path.
func (s socketScan) run() (masked, dropped []string, complete bool) {
	uidOf := s.uidOf
	if uidOf == nil {
		uidOf = func(fi os.FileInfo) (uint32, bool) { return ownerUID(fi, nil) }
	}
	access := s.access
	if access == nil {
		access = realAccess
	}
	now := s.now
	if now == nil {
		now = time.Now
	}
	deadline := now().Add(s.budget)
	complete = true
	found := map[string]bool{}
	roots := resolveRoots(s.roots)
	isRoot := map[string]bool{}
	for _, r := range roots {
		isRoot[r] = true
	}
	for _, root := range roots {
		entries := 0
		err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return skipOnError(d)
			}
			entries++
			if entries > s.maxEntries || now().After(deadline) {
				complete = false
				return filepath.SkipAll
			}
			if path == root {
				return nil
			}
			typ := d.Type()
			switch {
			case typ&fs.ModeSymlink != 0:
				return nil
			case typ.IsDir():
				fi, err := d.Info()
				if err != nil {
					return fs.SkipDir
				}
				if u, ok := uidOf(fi); ok && u == s.own && fi.Mode().Perm()&0o022 == 0 {
					return fs.SkipDir // the worker's own tree, closed to others
				}
				if !access(path, accessX) {
					return fs.SkipDir // unreachable: nothing inside can be connected to
				}
				if !access(path, accessR) || depth(root, path) >= s.maxDepth {
					found[path] = true
					return fs.SkipDir
				}
				return nil
			case typ&fs.ModeSocket != 0:
				fi, err := d.Info()
				if err != nil {
					return nil
				}
				if u, ok := uidOf(fi); ok && u == s.own {
					return nil
				}
				if !access(path, accessW) {
					return nil
				}
				dir := filepath.Dir(path)
				if isRoot[dir] {
					found[path] = true
				} else {
					found[dir] = true
				}
			}
			return nil
		})
		if err != nil {
			complete = false
		}
	}
	for _, e := range s.extra {
		found[e] = true
	}
	var all []string
	for m := range found {
		if isRoot[m] || m == "/" {
			continue
		}
		if containsAnyPath(m, s.protect) {
			dropped = append(dropped, m)
			continue
		}
		all = append(all, m)
	}
	sort.Strings(dropped)
	return collapse(all), dropped, complete
}

// skipOnError keeps walking past an entry that cannot be read.
func skipOnError(d fs.DirEntry) error {
	if d != nil && d.IsDir() {
		return fs.SkipDir
	}
	return nil
}

// resolveRoots resolves symlinked roots (/var/run is usually /run) and drops
// missing ones and duplicates, keeping order.
func resolveRoots(roots []string) []string {
	var out []string
	seen := map[string]bool{}
	for _, r := range roots {
		real, err := filepath.EvalSymlinks(r)
		if err != nil {
			continue
		}
		real = filepath.Clean(real)
		if !seen[real] {
			seen[real] = true
			out = append(out, real)
		}
	}
	return out
}

// depth is how many path elements path is below root.
func depth(root, path string) int {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return 0
	}
	return strings.Count(rel, string(filepath.Separator)) + 1
}

// containsAnyPath reports whether m is one of paths or an ancestor of one.
func containsAnyPath(m string, paths []string) bool {
	for _, p := range paths {
		p = filepath.Clean(p)
		if p == m || strings.HasPrefix(p, m+string(filepath.Separator)) {
			return true
		}
	}
	return false
}

// collapse sorts ms and drops duplicates and every entry under another
// entry. (Byte order puts "/run/a-b" between "/run/a" and "/run/a/x", so
// each entry is checked against every kept one.)
func collapse(ms []string) []string {
	sort.Strings(ms)
	var out []string
	for _, m := range ms {
		if !containsAnyPathOf(out, m) {
			out = append(out, m)
		}
	}
	return out
}

// containsAnyPathOf reports whether one of ancestors is m or an ancestor of m.
func containsAnyPathOf(ancestors []string, m string) bool {
	for _, a := range ancestors {
		if containsAnyPath(a, []string{m}) {
			return true
		}
	}
	return false
}
