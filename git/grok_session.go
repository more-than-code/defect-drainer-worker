package git

import (
	"github.com/more-than-code/defect-drainer-worker/agentadapter/grok"
)

// GrokSessionsDir is where Grok Build keeps the sessions for one cwd:
// <GrokHome>/sessions/<url-escaped absolute cwd>/<uuid>/ (verified on 1.0.40).
// GrokHome honours $GROK_HOME, which redirects the sessions store too.
func GrokSessionsDir(cwd string) (string, error) {
	home, err := GrokHome()
	if err != nil {
		return "", err
	}
	return grok.SessionsDir(home, cwd)
}

// LatestGrokSession returns the newest session for cwd that has a chat
// history, which is what `grok -c` would continue. ok is false when there is
// none, including when Grok's layout has moved: Resume then fails closed.
func LatestGrokSession(cwd string) (id string, ok bool) {
	home, err := GrokHome()
	if err != nil {
		return "", false
	}
	return grok.LatestSession(home, cwd)
}
