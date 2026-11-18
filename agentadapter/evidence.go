package agentadapter

import (
	"path/filepath"
	"strings"
)

// PathSet is the worktree paths as given and after EvalSymlinks (/var and
// /private/var on macOS), for matching against a shell command.
func PathSet(paths []string) map[string]struct{} {
	want := map[string]struct{}{}
	for _, p := range paths {
		if p == "" {
			continue
		}
		want[p] = struct{}{}
		if real, err := filepath.EvalSymlinks(p); err == nil && real != "" {
			want[real] = struct{}{}
		}
	}
	return want
}

// IsDiffOf reports whether a shell command is a git diff naming one of want.
func IsDiffOf(cmd string, want map[string]struct{}) bool {
	if !strings.Contains(cmd, "git") || !strings.Contains(cmd, "diff") {
		return false
	}
	for p := range want {
		if strings.Contains(cmd, p) {
			return true
		}
	}
	return false
}
