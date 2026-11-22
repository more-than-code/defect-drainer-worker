package exec

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
)

// ShotSignInName is the Stack sign-in file dd-shot hands `dd-worker shot`
// (S10), in the handoff next to dd-shot.
const ShotSignInName = "dd-shot-signin.json"

// ShotSignIn is the Stack sign-in as `dd-worker shot` runs it: URLs made
// absolute on the app's URL and the HTTP body rendered, so the driver fills
// nothing in itself. It holds the seed user's password (dev-only by S10);
// the run redacts it from all output.
type ShotSignIn struct {
	Method      string        `json:"method"`
	URL         string        `json:"url"`
	Email       string        `json:"email"`
	Password    string        `json:"password"`
	Form        *SignInFields `json:"form,omitempty"`
	HTTPMethod  string        `json:"httpMethod,omitempty"`
	HTTPURL     string        `json:"httpUrl,omitempty"`
	HTTPBody    string        `json:"httpBody,omitempty"`
	SuccessURL  string        `json:"successUrl,omitempty"`
	SuccessPath string        `json:"successPath,omitempty"`
}

// NewShotSignIn renders a's Stack sign-in with vals (the dd-stack
// placeholder values, {signin.*} included). The body's placeholder values
// are JSON-escaped, since the body is JSON.
func NewShotSignIn(a ProjectSettings, vals map[string]string) ShotSignIn {
	si := a.JobStackSignIn
	origin := a.AppOrigin()
	out := ShotSignIn{Method: si.Method, URL: origin + si.URL, Email: si.Email, Password: si.PasswordFor(vals[PlaceholderRandom]), Form: si.Form}
	if si.HTTP != nil {
		out.HTTPMethod, out.HTTPURL = si.HTTP.Method, origin+si.HTTP.Path
		out.HTTPBody = fillPlaceholders(si.HTTP.BodyTemplate, vals, jsonEscape)
	}
	if si.SuccessPath != "" {
		out.SuccessURL, out.SuccessPath = origin+si.SuccessPath, si.SuccessPath
	}
	return out
}

// jsonEscape is s as the inside of a JSON string.
func jsonEscape(s string) string {
	b, _ := json.Marshal(s)
	return strings.TrimSuffix(strings.TrimPrefix(string(b), `"`), `"`)
}

// WriteShotSignIn writes c into handoff, readable by the job's user only.
func WriteShotSignIn(handoff string, c ShotSignIn) error {
	b, err := json.Marshal(c)
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(handoff, ShotSignInName), b, 0o600)
}

// ReadShotSignIn reads a file WriteShotSignIn wrote.
func ReadShotSignIn(path string) (ShotSignIn, error) {
	var c ShotSignIn
	b, err := os.ReadFile(path)
	if err != nil {
		return c, err
	}
	err = json.Unmarshal(b, &c)
	return c, err
}
