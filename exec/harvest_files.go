package exec

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"
)

// Host-side file reads for the harvest and the driver loop's completion
// check. Every read is confined to real files under the handoff (see
// ReadRegularFile).

var ImageExts = map[string]bool{
	".png": true, ".jpg": true, ".jpeg": true,
	".webp": true, ".gif": true, ".heic": true,
}

// HarvestFileLimit matches api.maxMultipartMem: a harvested file is stored
// and served like an upload, so it cannot be larger than an upload.
const HarvestFileLimit = 26 << 20

// ReadRegularFile reads p only when p is a regular file reached without
// traversing a symlink. The agent that populates the handoff directory is
// sandboxed precisely so it cannot read host secrets; harvest is not. Following
// a link the agent planted would make this process read the secret on its
// behalf, so every harvest read is confined to real files under root.
//
// tooLarge is true when the file exceeds HarvestFileLimit; callers must skip
// and log rather than persist a truncated prefix. A mismatched birthtime
// (inode created before notBefore) is a silent skip — same as Nlink/Dev.
func ReadRegularFile(root, p string, notBefore time.Time) (data []byte, tooLarge, ok bool) {
	if !underRoot(root, p) {
		return nil, false, false
	}
	fi, err := os.Lstat(p)
	if err != nil || !fi.Mode().IsRegular() {
		return nil, false, false
	}
	f, err := os.OpenFile(p, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return nil, false, false
	}
	defer f.Close()
	// Re-check after the open so a swap between Lstat and open cannot win.
	st, err := f.Stat()
	if err != nil || !st.Mode().IsRegular() {
		return nil, false, false
	}
	sys, okSys := st.Sys().(*syscall.Stat_t)
	if !okSys || sys.Nlink > 1 {
		// A hardlink is a regular file and passes O_NOFOLLOW; it still shares
		// the inode of a host secret the agent could not read. Refuse it.
		return nil, false, false
	}
	dirInfo, err := os.Stat(root)
	if err != nil {
		return nil, false, false
	}
	dirSys, okDir := dirInfo.Sys().(*syscall.Stat_t)
	if !okDir || sys.Dev != dirSys.Dev {
		return nil, false, false
	}
	if !notBefore.IsZero() {
		if born, hasBtime := FileBirthTime(f, sys); hasBtime && born.Before(notBefore) {
			return nil, false, false
		}
	}
	if st.Size() > HarvestFileLimit {
		return nil, true, false
	}
	b, err := io.ReadAll(io.LimitReader(f, HarvestFileLimit+1))
	if err != nil {
		return nil, false, false
	}
	if int64(len(b)) > HarvestFileLimit {
		return nil, true, false
	}
	return b, false, true
}

// underRoot reports whether p resolves to a location inside root, with every
// symlink on the way already resolved. A missing path is judged on its cleaned
// form so callers can probe candidates that do not exist.
func underRoot(root, p string) bool {
	realRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return false
	}
	target := p
	if resolved, err := filepath.EvalSymlinks(p); err == nil {
		target = resolved
	} else {
		target = filepath.Clean(p)
	}
	rel, err := filepath.Rel(realRoot, target)
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// IsRealDir reports whether p is a directory itself, not a symlink to one.
func IsRealDir(p string) bool {
	fi, err := os.Lstat(p)
	return err == nil && fi.IsDir()
}

func ReadFirstNote(root string, notBefore time.Time, cands ...string) (note string, oversized []string) {
	for _, p := range cands {
		b, tooLarge, ok := ReadRegularFile(root, p, notBefore)
		if tooLarge {
			oversized = append(oversized, p)
			continue
		}
		if !ok {
			continue
		}
		if s := strings.TrimSpace(string(b)); s != "" {
			return s, oversized
		}
	}
	return "", oversized
}

func UnixSignalName(sig os.Signal) string {
	if sig == nil {
		return "null"
	}
	// syscall.Signal.String() is "terminated" / "interrupt"; TS uses SIGTERM / SIGINT.
	switch sig {
	case syscall.SIGTERM:
		return "SIGTERM"
	case syscall.SIGINT:
		return "SIGINT"
	case syscall.SIGKILL:
		return "SIGKILL"
	default:
		return sig.String()
	}
}

// TruncateUTF8 cuts s to at most max bytes without splitting a rune.
func TruncateUTF8(s string, max int) string {
	if len(s) <= max {
		return s
	}
	cut := max
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut]
}
