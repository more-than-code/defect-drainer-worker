package exec

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unicode"

	"github.com/more-than-code/defect-drainer-worker/git"
)

// ProgressFingerprint is what an iteration could have changed: per worktree
// one `git status --porcelain=v2 --branch` (HEAD, every changed and
// untracked path with its index state) plus a stamp of each of those paths;
// and a stamp of NOTES.md, fix-notes/* and fix-evidence/** in the handoff.
// A stamp is the name and the SHA-256 of the content, so a file re-saved
// with the same bytes is no progress and a second edit to a changed or new
// file is (spec §4.1.6, amended 2026-11-20); a file over 10 MB falls back to
// name, size and mtime. Nothing else in the handoff (logs, agent state) is
// stamped. Two equal fingerprints in a row mean the iteration did nothing
// (stop no_progress).
//
// Git runs as the worker, outside the agent's sandbox, through the pinned
// binding (never the worktree's own .git): one call per worktree, whose cost
// is mostly the pinning itself. Any git error makes the result unique, so an
// error can never look like "no progress".
func ProgressFingerprint(handoff string, wts []git.WorktreeBinding) string {
	h := sha256.New()
	for _, w := range wts {
		out, err := WorktreeGit(w, "status", "--porcelain=v2", "--branch", "-z", "--untracked-files=all")
		if err != nil {
			return "unique:" + strconv.FormatInt(time.Now().UnixNano(), 10)
		}
		fmt.Fprintf(h, "wt\x00%s\x00%s\x00", w.Repo, out)
		for i, rel := range statusPaths(out) {
			if i >= maxStampedPaths {
				break
			}
			stampFile(h, "p", rel, filepath.Join(w.WorktreeAbs, filepath.FromSlash(rel)))
		}
	}
	stampFile(h, "f", "NOTES.md", filepath.Join(handoff, "NOTES.md"))
	if ents, err := os.ReadDir(filepath.Join(handoff, "fix-notes")); err == nil {
		for _, e := range ents {
			stampFile(h, "f", "fix-notes/"+e.Name(), filepath.Join(handoff, "fix-notes", e.Name()))
		}
	}
	// WalkDir does not follow symlinks; a link is stamped as itself.
	_ = filepath.WalkDir(filepath.Join(handoff, "fix-evidence"), func(p string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			rel, _ := filepath.Rel(handoff, p)
			stampFile(h, "f", filepath.ToSlash(rel), p)
		}
		return nil
	})
	return hex.EncodeToString(h.Sum(nil))
}

// maxHashedFile is the largest file whose content the fingerprint hashes.
const maxHashedFile = 10 << 20

// stampFile writes one path's stamp into h: for a regular file up to
// maxHashedFile, its name and content hash; for a larger file, its name,
// size and mtime; for a symlink, its name and target (never followed); a
// missing path writes nothing. The open refuses to follow a link swapped in
// after the Lstat.
func stampFile(h io.Writer, kind, rel, abs string) {
	fi, err := os.Lstat(abs)
	if err != nil {
		return
	}
	switch {
	case fi.Mode()&fs.ModeSymlink != 0:
		target, _ := os.Readlink(abs)
		fmt.Fprintf(h, "%s\x00%s\x00link\x00%s\x00", kind, rel, target)
		return
	case !fi.Mode().IsRegular() || fi.Size() > maxHashedFile:
		fmt.Fprintf(h, "%s\x00%s\x00%d\x00%d\x00", kind, rel, fi.Size(), fi.ModTime().UnixNano())
		return
	}
	f, err := os.OpenFile(abs, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		fmt.Fprintf(h, "%s\x00%s\x00unreadable\x00%d\x00", kind, rel, fi.ModTime().UnixNano())
		return
	}
	defer f.Close()
	sum := sha256.New()
	if _, err := io.Copy(sum, io.LimitReader(f, maxHashedFile+1)); err != nil {
		fmt.Fprintf(h, "%s\x00%s\x00unreadable\x00%d\x00", kind, rel, fi.ModTime().UnixNano())
		return
	}
	fmt.Fprintf(h, "%s\x00%s\x00%x\x00", kind, rel, sum.Sum(nil))
}

// maxStampedPaths bounds the changed paths stamped per worktree.
const maxStampedPaths = 10000

// statusPaths are the paths in `git status --porcelain=v2 -z` output: the
// path of each changed (1), renamed or copied (2, whose original path is
// the next record), unmerged (u) and untracked (?) entry.
func statusPaths(out string) []string {
	var paths []string
	recs := strings.Split(out, "\x00")
	for i := 0; i < len(recs); i++ {
		r := recs[i]
		if len(r) < 2 {
			continue
		}
		var fields int
		switch r[0] {
		case '1':
			fields = 9
		case '2':
			fields = 10
			i++ // the original path
		case 'u':
			fields = 11
		case '?':
			paths = append(paths, r[2:])
			continue
		default:
			continue
		}
		if parts := strings.SplitN(r, " ", fields); len(parts) == fields {
			paths = append(paths, parts[fields-1])
		}
	}
	return paths
}

// blockedReasonMax caps agentRun.stopReason.
const blockedReasonMax = 200

// NotesDigest is NOTES.md's content as the loop sees it: its SHA-256, or
// "absent" or "unreadable". It is read like harvest reads it (cutoff is the
// job's harvest cutoff). The loop takes it before iteration 1 spawns.
func NotesDigest(handoff string, cutoff time.Time) string {
	p := filepath.Join(handoff, "NOTES.md")
	if _, err := os.Lstat(p); err != nil {
		return "absent"
	}
	data, _, read := ReadRegularFile(handoff, p, cutoff)
	if !read {
		return "unreadable"
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// BlockedReason is the reason on the first `BLOCKED: <reason>` line of
// NOTES.md. It counts only when NOTES.md's content differs from startDigest
// (NotesDigest before this run's first iteration), so a line left from
// before a Resume or Rerun does not stop that run at once. Content, not
// mtime: Linux stamps mtimes from a coarse clock that lags time.Now (PR #3
// CI). The reason is the agent's text: control characters are dropped and
// it is cut to 200 characters. ok is false when there is no such line.
func BlockedReason(handoff string, cutoff time.Time, startDigest string) (reason string, ok bool) {
	p := filepath.Join(handoff, "NOTES.md")
	data, _, read := ReadRegularFile(handoff, p, cutoff)
	if !read {
		return "", false
	}
	if sum := sha256.Sum256(data); hex.EncodeToString(sum[:]) == startDigest {
		return "", false
	}
	for _, line := range strings.Split(string(data), "\n") {
		rest, found := strings.CutPrefix(strings.TrimSpace(line), "BLOCKED:")
		if !found {
			continue
		}
		rest = strings.TrimSpace(strings.Map(func(r rune) rune {
			if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
				return -1
			}
			return r
		}, rest))
		// An empty reason, or the brief's own "<reason>" placeholder copied
		// as is, is not a declaration.
		if rest == "" || (strings.HasPrefix(rest, "<") && strings.HasSuffix(rest, ">")) {
			continue
		}
		if rs := []rune(rest); len(rs) > blockedReasonMax {
			rest = string(rs[:blockedReasonMax])
		}
		return rest, true
	}
	return "", false
}
