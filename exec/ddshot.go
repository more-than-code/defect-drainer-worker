package exec

import (
	_ "embed"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// ddShotScript is dd-shot (worker-browser B3), shellchecked as a tracked
// file; WriteDDShot fills its dd_* lines. Lines ending in stackMarker are
// the job-stack variant (S4): kept only when the app has services,
// so an app without them gets rc5's dd-shot byte for byte.
//
//go:embed ddshot.sh
var ddShotScript string

// DDShotName is the helper's file name in the handoff.
const DDShotName = "dd-shot"

// stackMarker ends every line of dd-shot's job-stack variant.
const stackMarker = " # dd-stack"

// DDShot is what dd-shot needs: the host browser (policy agent_browser), the
// worktree the preview runs in, and the app's preview command and URL.
// Stack runs the preview inside dd-stack (written next to it), waiting
// TimeoutSec for the preview URL instead of 90 s.
type DDShot struct {
	Browser    string
	Dir        string
	PreviewCmd string
	PreviewURL string
	Stack      bool
	TimeoutSec int
}

// WriteDDShot writes dd-shot into handoff with c's values baked in, so it
// works even where the env does not reach the agent's shell. None is a
// secret: the agent can read every one of them anyway.
func WriteDDShot(handoff string, c DDShot) error {
	body := stackLines(ddShotScript, c.Stack)
	if c.Stack {
		line := "dd_wait=''" + stackMarker + "\n"
		if strings.Count(body, line) != 1 {
			return fmt.Errorf("dd-shot: template line %q", line)
		}
		body = strings.Replace(body, line, fmt.Sprintf("dd_wait=%d%s\n", c.TimeoutSec, stackMarker), 1)
	}
	for _, kv := range [][2]string{
		{"dd_browser", c.Browser}, {"dd_dir", c.Dir}, {"dd_cmd", c.PreviewCmd}, {"dd_url", c.PreviewURL},
	} {
		line := kv[0] + "=''\n"
		if strings.Count(body, line) != 1 {
			return fmt.Errorf("dd-shot: template line %q", line)
		}
		body = strings.Replace(body, line, kv[0]+"="+shQuote(kv[1])+"\n", 1)
	}
	return os.WriteFile(filepath.Join(handoff, DDShotName), []byte(body), 0o755)
}

// stackLines keeps the lines ending in stackMarker when stack, and drops
// them otherwise.
func stackLines(script string, stack bool) string {
	if stack {
		return script
	}
	lines := strings.SplitAfter(script, "\n")
	kept := lines[:0]
	for _, l := range lines {
		if !strings.HasSuffix(strings.TrimSuffix(l, "\n"), stackMarker) {
			kept = append(kept, l)
		}
	}
	return strings.Join(kept, "")
}

// shQuote single-quotes s for POSIX sh.
func shQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
