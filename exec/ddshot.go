package exec

import (
	_ "embed"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// ddShotScript is dd-shot (worker-browser B3), shellchecked as a tracked
// file; WriteDDShot fills its four dd_* lines.
//
//go:embed ddshot.sh
var ddShotScript string

// DDShotName is the helper's file name in the handoff.
const DDShotName = "dd-shot"

// DDShot is what dd-shot needs: the host browser (policy agent_browser), the
// worktree the preview runs in, and the app's preview command and URL.
type DDShot struct {
	Browser    string
	Dir        string
	PreviewCmd string
	PreviewURL string
}

// WriteDDShot writes dd-shot into handoff with c's values baked in, so it
// works even where the env does not reach the agent's shell. None is a
// secret: the agent can read every one of them anyway.
func WriteDDShot(handoff string, c DDShot) error {
	body := ddShotScript
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

// shQuote single-quotes s for POSIX sh.
func shQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
