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

// The env placeholders (S3, S10). Anything else that looks like one is
// refused. The {signin.*} ones exist only with a Stack sign-in.
const (
	PlaceholderRandom         = "{random}"
	PlaceholderPostgres       = "{postgres.url}"
	PlaceholderSignInURL      = "{signin.url}"
	PlaceholderSignInEmail    = "{signin.email}"
	PlaceholderSignInPassword = "{signin.password}"
)

// Stack sign-in methods (S10).
const (
	SignInForm = "form"
	SignInHTTP = "http"
)

// Stack sign-in limits (S10).
const (
	MaxSignInEmail    = 254
	MinSignInPassword = 8 // shorter would redact ordinary words from logs
	MaxSignInPassword = 256
	MaxSignInSelector = 200
	MaxSignInBody     = 2048
)

// StackSignIn is the Stack sign-in (S10): the app's own seed user, valid
// only in the job's throwaway database, never a real credential. URL and
// SuccessPath are paths on the app's URL (PreviewURL).
type StackSignIn struct {
	URL         string        `json:"url"`
	Email       string        `json:"email"`
	Password    string        `json:"password"`
	Method      string        `json:"method"`
	Form        *SignInFields `json:"form,omitempty"`
	HTTP        *SignInCall   `json:"http,omitempty"`
	SuccessPath string        `json:"successPath,omitempty"`
}

// SignInFields is a form sign-in: CSS selectors of the email and password
// fields and of the submit control.
type SignInFields struct {
	EmailSelector    string `json:"emailSelector"`
	PasswordSelector string `json:"passwordSelector"`
	SubmitSelector   string `json:"submitSelector"`
}

// SignInCall is an HTTP sign-in, made from the browser's page so the
// session cookie lands in it: Method (POST, PUT or PATCH) to Path with a
// JSON body from BodyTemplate.
type SignInCall struct {
	Method       string `json:"method"`
	Path         string `json:"path"`
	BodyTemplate string `json:"bodyTemplate,omitempty"`
}

// AppOrigin is the app URL without a trailing slash.
func (a ProjectSettings) AppOrigin() string { return strings.TrimSuffix(a.PreviewURL, "/") }

// SignInValues are the {signin.*} placeholder values, or nil without a
// Stack sign-in. random is the run's {random} value.
func (a ProjectSettings) SignInValues(random string) map[string]string {
	si := a.JobStackSignIn
	if si == nil {
		return nil
	}
	return map[string]string{
		PlaceholderSignInURL:      a.AppOrigin() + si.URL,
		PlaceholderSignInEmail:    si.Email,
		PlaceholderSignInPassword: si.PasswordFor(random),
	}
}

// GeneratedPassword reports whether the password is generated per run
// (signin_random.v1): exactly "{random}". Any other text is a literal.
func (si StackSignIn) GeneratedPassword() bool { return si.Password == PlaceholderRandom }

// PasswordFor is the password a run uses: its {random} value when the
// password is generated, else the literal.
func (si StackSignIn) PasswordFor(random string) string {
	if si.GeneratedPassword() {
		return random
	}
	return si.Password
}

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
func (a ProjectSettings) StackOn() bool { return len(a.JobStack) > 0 }

// NeedsPostgres reports whether any preview service needs Postgres.
func (a ProjectSettings) NeedsPostgres() bool {
	for _, s := range a.JobStack {
		if s.NeedsPostgres {
			return true
		}
	}
	return false
}

// PreviewTimeout is the effective preview timeout in seconds.
func (a ProjectSettings) PreviewTimeout() int {
	if a.PreviewTimeoutSec == 0 {
		return DefaultPreviewTimeoutSec
	}
	return a.PreviewTimeoutSec
}
