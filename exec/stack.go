package exec

import (
	"regexp"
	"strconv"
	"strings"
)

// Job stack (docs/job-stack.md in the control plane, S1–S9):
// the app's job stack, booted by dd-stack inside the agent's sandbox.

// Limits (S6). A worker refuses a spec past them (bad_spec).
const (
	MaxStackServices = 4
	MaxPreviewSeeds  = 4
	MaxPreviewEnv    = 32
	// MaxStackCommand bounds install, migrate, each seed and start.
	MaxStackCommand = 500
	// MaxStackEnvValue bounds one env value, placeholders unexpanded.
	MaxStackEnvValue = 1024
	MinPreviewPort   = 1024
	MaxPreviewPort   = 65535
	// The preview timeout bounds the whole stack and dd-shot's wait for the
	// preview URL; 0 in the spec means the default.
	DefaultPreviewTimeoutSec = 90
	MinPreviewTimeoutSec     = 30
	MaxPreviewTimeoutSec     = 300
	// ServiceHealthSec bounds each service's wait for its health URL.
	ServiceHealthSec = 120
)

// JobStackService is one service of the app's job stack. Commands run in
// the worktree of Repo with Env, in this order: Install, Migrate, each Seed,
// then Start, which must answer http://127.0.0.1:<Port><Health> with a 2xx
// within ServiceHealthSec.
type JobStackService struct {
	Name          string            `json:"name"`
	Repo          string            `json:"repo"`
	Install       string            `json:"install,omitempty"`
	Migrate       string            `json:"migrate,omitempty"`
	Seed          []string          `json:"seed,omitempty"`
	Start         string            `json:"start"`
	Port          int               `json:"port"`
	Health        string            `json:"health"`
	Env           map[string]string `json:"env,omitempty"`
	NeedsPostgres bool              `json:"needsPostgres,omitempty"`
}

// The env placeholders (S3). Anything else that looks like one is refused.
const (
	PlaceholderRandom   = "{random}"
	PlaceholderPostgres = "{postgres.url}"
)

// placeholderRe is any {token} of letters, digits and . _ : -: the shape a
// placeholder has, so a misspelt one, or a form S3 rules out such as
// {secret:NAME} or {env.HOME}, is refused rather than passed on as text.
// JSON such as {"a":1} does not match.
var placeholderRe = regexp.MustCompile(`\{[A-Za-z][A-Za-z0-9._:-]*\}`)

// serviceNameRe is a service name (S3.1).
var serviceNameRe = regexp.MustCompile(`^[a-z][a-z0-9-]{0,31}$`)

// ValidServiceName reports whether s is a preview service name.
func ValidServiceName(s string) bool { return serviceNameRe.MatchString(s) }

// ServiceURL is a service's {services.<name>.url} value.
func ServiceURL(port int) string { return "http://127.0.0.1:" + strconv.Itoa(port) }

// Placeholders returns the placeholder tokens in v, in order.
func Placeholders(v string) []string { return placeholderRe.FindAllString(v, -1) }

// ServicePlaceholder returns the service name of a {services.<name>.url}
// token, or "".
func ServicePlaceholder(tok string) string {
	name, ok := strings.CutPrefix(tok, "{services.")
	if !ok {
		return ""
	}
	name, ok = strings.CutSuffix(name, ".url}")
	if !ok || !ValidServiceName(name) {
		return ""
	}
	return name
}

// StackOn reports whether the app has a job stack.
func (a AppSettings) StackOn() bool { return len(a.JobStack) > 0 }

// NeedsPostgres reports whether any preview service needs Postgres.
func (a AppSettings) NeedsPostgres() bool {
	for _, s := range a.JobStack {
		if s.NeedsPostgres {
			return true
		}
	}
	return false
}

// PreviewTimeout is the effective preview timeout in seconds.
func (a AppSettings) PreviewTimeout() int {
	if a.PreviewTimeoutSec == 0 {
		return DefaultPreviewTimeoutSec
	}
	return a.PreviewTimeoutSec
}
