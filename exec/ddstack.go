package exec

import (
	"crypto/rand"
	_ "embed"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// ddStackScript is dd-stack (job stack S4), shellchecked as a tracked
// file; WriteDDStack replaces its dd_config line with the stack's
// definition.
//
//go:embed ddstack.sh
var ddStackScript string

// DDStackName is the helper's file name in the handoff.
const DDStackName = "dd-stack"

// DDStack is what dd-stack needs: the app's services and preview env, the
// worktree of each repo, the policy's Postgres binaries ("" when no service
// needs Postgres), the preview timeout and the job run's {random} value.
// SignIn holds the {signin.*} values (ProjectSettings.SignInValues) when the
// app has a Stack sign-in; dd-stack then also exports them as
// DD_SIGNIN_URL, DD_SIGNIN_EMAIL and DD_SIGNIN_PASSWORD (S10).
type DDStack struct {
	Services   []JobStackService
	PreviewEnv map[string]string
	Dirs       map[string]string // repo -> worktree
	Postgres   string
	TimeoutSec int
	Random     string
	SignIn     map[string]string
}

// signInEnv names the env a Stack sign-in exports, by placeholder.
var signInEnv = [][2]string{
	{"DD_SIGNIN_URL", PlaceholderSignInURL},
	{"DD_SIGNIN_EMAIL", PlaceholderSignInEmail},
	{"DD_SIGNIN_PASSWORD", PlaceholderSignInPassword},
}

// NewStackRandom is a {random} value: 32 random bytes, hex (S3). One value
// per job run, the same in every env entry, so a service and the preview
// share it.
func NewStackRandom() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// WriteDDStack writes dd-stack into handoff with c's definition baked in.
// Every value is single-quoted; {postgres.url} becomes "$dd_pg_url", set
// once Postgres is up. The file holds the {random} value: the agent may
// read it (it is dev-only by S3), and the run redacts it from all output.
func WriteDDStack(handoff string, c DDStack) error {
	line := "dd_config() { :; }\n"
	if strings.Count(ddStackScript, line) != 1 {
		return fmt.Errorf("dd-stack: template line %q", line)
	}
	cfg, err := stackConfig(c)
	if err != nil {
		return err
	}
	body := strings.Replace(ddStackScript, line, cfg, 1)
	return os.WriteFile(filepath.Join(handoff, DDStackName), []byte(body), 0o755)
}

// stackConfig is dd-stack's definition: dd_config, then svc_<n>, steps_<n>
// and env_<n> per service, then env_preview.
func stackConfig(c DDStack) (string, error) {
	var b strings.Builder
	ports := make([]string, 0, len(c.Services))
	for _, s := range c.Services {
		ports = append(ports, fmt.Sprint(s.Port))
	}
	pg := ""
	if slices.ContainsFunc(c.Services, func(s JobStackService) bool { return s.NeedsPostgres }) {
		if c.Postgres == "" {
			return "", fmt.Errorf("dd-stack: a service needs Postgres and the host has none")
		}
		pg = c.Postgres
	}
	fmt.Fprintf(&b, "dd_config() {\n\tdd_pg_bin=%s\n\tdd_timeout=%d\n\tdd_n=%d\n\tdd_ports=%s\n}\n",
		shQuote(pg), c.TimeoutSec, len(c.Services), shQuote(strings.Join(ports, " ")))
	vals := c.placeholderValues()
	for i, s := range c.Services {
		n := i + 1
		dir, ok := c.Dirs[s.Repo]
		if !ok {
			return "", fmt.Errorf("dd-stack: service %s: no worktree for repo %s", s.Name, s.Repo)
		}
		fmt.Fprintf(&b, "svc_%d() {\n\ts_name=%s\n\ts_dir=%s\n\ts_port=%d\n\ts_health=%s\n\ts_start=%s\n}\n",
			n, shQuote(s.Name), shQuote(dir), s.Port, shQuote(s.Health), shQuote(s.Start))
		fmt.Fprintf(&b, "steps_%d() {\n\t:\n", n)
		if s.Install != "" {
			fmt.Fprintf(&b, "\tdd_step install %s\n", shQuote(s.Install))
		}
		if s.Migrate != "" {
			fmt.Fprintf(&b, "\tdd_step migrate %s\n", shQuote(s.Migrate))
		}
		for j, seed := range s.Seed {
			fmt.Fprintf(&b, "\tdd_step seed-%d %s\n", j+1, shQuote(seed))
		}
		b.WriteString("}\n")
		writeEnvFunc(&b, fmt.Sprintf("env_%d", n), s.Env, vals, c.SignIn != nil)
	}
	writeEnvFunc(&b, "env_preview", c.PreviewEnv, vals, c.SignIn != nil)
	return b.String(), nil
}

// placeholderValues maps every placeholder known when dd-stack is written:
// {random}, each {services.<name>.url} and the {signin.*} values.
func (c DDStack) placeholderValues() map[string]string {
	vals := map[string]string{PlaceholderRandom: c.Random}
	for _, s := range c.Services {
		vals["{services."+s.Name+".url}"] = ServiceURL(s.Port)
	}
	for k, v := range c.SignIn {
		vals[k] = v
	}
	return vals
}

// writeEnvFunc writes a function that exports the DD_SIGNIN_* env when
// signIn, then env, names sorted, with the placeholders in vals filled and
// {postgres.url} left to the shell.
func writeEnvFunc(b *strings.Builder, name string, env map[string]string, vals map[string]string, signIn bool) {
	fmt.Fprintf(b, "%s() {\n\t:\n", name)
	if signIn {
		for _, e := range signInEnv {
			fmt.Fprintf(b, "\texport %s=%s\n", e[0], shQuote(vals[e[1]]))
		}
	}
	keys := make([]string, 0, len(env))
	for k := range env {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	for _, k := range keys {
		fmt.Fprintf(b, "\texport %s=%s\n", k, stackValue(env[k], vals))
	}
	b.WriteString("}\n")
}

// stackValue is one env value as sh source: single-quoted text joined with
// "$dd_pg_url" where {postgres.url} was.
func stackValue(v string, vals map[string]string) string {
	v = fillPlaceholders(v, vals, func(s string) string { return s })
	parts := strings.Split(v, PlaceholderPostgres)
	out := make([]string, len(parts))
	for i, p := range parts {
		out[i] = shQuote(p)
	}
	return strings.Join(out, `"$dd_pg_url"`)
}

// fillPlaceholders replaces each placeholder in v found in vals with
// enc(value), leaving the others ({postgres.url}) as they are.
func fillPlaceholders(v string, vals map[string]string, enc func(string) string) string {
	return placeholderRe.ReplaceAllStringFunc(v, func(tok string) string {
		if val, ok := vals[tok]; ok {
			return enc(val)
		}
		return tok
	})
}
