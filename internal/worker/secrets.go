package worker

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"syscall"

	"github.com/more-than-code/defect-drainer-worker/agentadapter"
	"github.com/more-than-code/defect-drainer-worker/internal/botagent/config"
	"github.com/more-than-code/defect-drainer-worker/internal/botagent/redact"
)

var envNameRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// ValidEnvName reports whether s is a usable environment variable name.
func ValidEnvName(s string) bool { return envNameRe.MatchString(s) }

// Secrets is /etc/dd-worker/secrets.env (D5): test-account passwords and
// adapter API keys the host owner keeps on this host. Values never leave
// the worker except to the one child that needs them.
type Secrets struct {
	vals map[string]string
}

// LoadSecrets reads path. A missing file is an empty set. The file must be
// a regular file (not a symlink) with no group or world bits, owned by root
// or by the worker's own user. Root-owned is the installed layout: under
// systemd the worker reads the LoadCredential copy instead
// (LoadSecretsCredential), and this path serves dd-worker run by hand and
// older units. uid overrides the owner for tests.
func LoadSecrets(path string, uid func(os.FileInfo) (uint32, bool)) (Secrets, error) {
	return loadSecrets(path, func(fi os.FileInfo) string { return secretsFileReason(fi, uid, true) })
}

// SecretsCredential is the credential ID the unit's
// LoadCredential=secrets.env:/etc/dd-worker/secrets.env hands the service.
const SecretsCredential = "secrets.env"

// LoadSecretsCredential reads the systemd credential copy of secrets.env
// from dir ($CREDENTIALS_DIRECTORY). systemd owns the copy, so only the
// file and mode rules apply, not the owner rule. A missing copy is an
// empty set.
func LoadSecretsCredential(dir string) (Secrets, error) {
	return loadSecrets(filepath.Join(dir, SecretsCredential),
		func(fi os.FileInfo) string { return secretsFileReason(fi, nil, false) })
}

func loadSecrets(path string, rule func(os.FileInfo) string) (Secrets, error) {
	fi, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return Secrets{vals: map[string]string{}}, nil
	}
	if err != nil {
		return Secrets{}, errors.New("secrets.env cannot be read")
	}
	switch rule(fi) {
	case "bad_symlink", "bad_not_regular":
		return Secrets{}, errors.New("secrets.env must be a regular file")
	case "bad_mode":
		return Secrets{}, errors.New("secrets.env must not be group- or world-accessible (chmod 0600)")
	case "bad_owner":
		return Secrets{}, errors.New("secrets.env must be owned by root or the worker user")
	}
	body, err := os.ReadFile(path)
	if err != nil {
		return Secrets{}, errors.New("secrets.env cannot be read")
	}
	vals, err := config.ParseEnv(body)
	if err != nil {
		return Secrets{}, fmt.Errorf("secrets.env: %v", err)
	}
	out := map[string]string{}
	for k, v := range vals {
		if ValidEnvName(k) {
			out[k] = v
		}
	}
	return Secrets{vals: out}, nil
}

// secretsFileReason is the file rule for secrets.env as a check reason, ""
// when it passes: a regular file (not a symlink) with no group or world
// bits, and, when checkOwner, owned by root or by this process's user.
func secretsFileReason(fi os.FileInfo, uid func(os.FileInfo) (uint32, bool), checkOwner bool) string {
	switch {
	case fi.Mode()&os.ModeSymlink != 0:
		return "bad_symlink"
	case !fi.Mode().IsRegular():
		return "bad_not_regular"
	case fi.Mode().Perm()&0o077 != 0:
		return "bad_mode"
	case !checkOwner:
		return ""
	}
	owner, ok := ownerUID(fi, uid)
	if !ok || (owner != 0 && int(owner) != os.Geteuid()) {
		return "bad_owner"
	}
	return ""
}

// MaxSecretsBytes bounds a secrets.env candidate (the parser's line limit).
const MaxSecretsBytes = 64 * 1024

// CheckSecretsData checks a candidate secrets.env's content (dd-worker
// secrets check): it parses, every name is a ValidEnvName, and no name
// repeats. It returns "" or a bad_* reason and never a value.
func CheckSecretsData(data []byte) string {
	if len(data) > MaxSecretsBytes {
		return "bad_format"
	}
	keys, err := config.EnvKeys(data)
	if err != nil {
		return "bad_format"
	}
	seen := map[string]bool{}
	for _, k := range keys {
		if !ValidEnvName(k) {
			return "bad_name"
		}
		if seen[k] {
			return "bad_duplicate"
		}
		seen[k] = true
	}
	return ""
}

// CheckSecretsFile checks path with LoadSecrets' file rule, then its
// content. err is set when the file cannot be read (missing included).
func CheckSecretsFile(path string, uid func(os.FileInfo) (uint32, bool)) (reason string, err error) {
	fi, err := os.Lstat(path)
	if err != nil {
		return "", err
	}
	if r := secretsFileReason(fi, uid, true); r != "" {
		return r, nil
	}
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, MaxSecretsBytes+1))
	if err != nil {
		return "", err
	}
	return CheckSecretsData(data), nil
}

func ownerUID(fi os.FileInfo, override func(os.FileInfo) (uint32, bool)) (uint32, bool) {
	if override != nil {
		return override(fi)
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok || st == nil {
		return 0, false
	}
	return st.Uid, true
}

// Get returns the value of name.
func (s Secrets) Get(name string) (string, bool) {
	v, ok := s.vals[name]
	return v, ok && v != ""
}

// Names are the variable names, sorted.
func (s Secrets) Names() []string {
	out := make([]string, 0, len(s.vals))
	for k := range s.vals {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// Redactor is the exact-value redaction set over every value, plus extra
// values (the worker credential, job tokens).
func (s Secrets) Redactor(extra ...string) redact.Exact {
	vals := append([]string(nil), extra...)
	for _, v := range s.vals {
		vals = append(vals, v)
	}
	return redact.NewExact(vals...)
}

// DenySet is the env names stripped from every child of a job (§6.6, with
// Main Dev's 2026-11-15 clarification): every name in secrets.env except the
// chosen adapter's own key, plus the spec's StripEnv. The job's own SecretEnv
// is stripped too: exec reads that one value in-process and the child gets it
// only as DD_TEST_ACCOUNT_PASSWORD. Invalid names are ignored.
func (s Secrets) DenySet(adapterKeep []string, specStrip []string) []string {
	keep := map[string]bool{}
	for _, k := range adapterKeep {
		keep[k] = true
	}
	set := map[string]bool{}
	for name := range s.vals {
		if !keep[name] {
			set[name] = true
		}
	}
	for _, name := range specStrip {
		if ValidEnvName(name) {
			set[name] = true
		}
	}
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// adapterKeyNames are the secrets.env names that hold an adapter's API key.
// A key present there puts the adapter in api_key mode. Only the Claude
// adapter takes a key through agentadapter.Credential today; grok and agy
// use their own login.
var adapterKeyNames = map[agentadapter.Name]string{
	agentadapter.Claude: "ANTHROPIC_API_KEY",
}
