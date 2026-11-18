// Package config loads and rewrites the dd-worker credential env file.
//
// Vendored from bot-agent internal/agent/config (Apache-2.0, see ../NOTICE).
// DD changes: the keys are DD_WORKER_* and the paths /etc/dd-worker (host
// owner's config, never written by the worker) and /var/lib/dd-worker (state,
// with the credential in credential/, a 0700 dir); the server is an https origin (http only on loopback
// with the insecure-dev flag) from which the worker derives the socket URL;
// ids are worker protocol ids, not UUIDs; there is no process-environment
// overlay (the credential is read from the file only); the join token is not
// stored, because enrolment dials at once; ParseEnv is exported for
// secrets.env. The env-file parser and writer,
// the atomic write, the join-token reader and SanitizeHostname are kept.
package config

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"regexp"
	"strings"

	"github.com/more-than-code/defect-drainer-worker/workerproto"
)

const (
	DefaultConfigDir = "/etc/dd-worker"
	DefaultStateDir  = "/var/lib/dd-worker"

	keyURL      = "DD_WORKER_SERVER"
	keyWorker   = "DD_WORKER_ID"
	keyOrg      = "DD_WORKER_ORG_ID"
	keyCred     = "DD_WORKER_CREDENTIAL"
	keyInsecure = "DD_WORKER_INSECURE_DEV"
)

// Config is one credential env file (env or env.prev).
type Config struct {
	Path        string
	ServerURL   string
	WorkerID    string
	OrgID       string
	Credential  string
	InsecureDev bool
}

// Load reads path, requires a regular file with mode 0600, and checks the
// values. Error strings never contain the credential.
func Load(path string) (Config, error) {
	fi, err := os.Lstat(path)
	if err != nil {
		return Config{}, fmt.Errorf("env file: %w", err)
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		return Config{}, errors.New("env file must not be a symlink")
	}
	if !fi.Mode().IsRegular() {
		return Config{}, errors.New("env file must be a regular file")
	}
	if fi.Mode().Perm() != 0o600 {
		return Config{}, errors.New("env file mode is not 0600")
	}
	body, err := os.ReadFile(path)
	if err != nil {
		return Config{}, fmt.Errorf("env file: %w", err)
	}
	disk, _, err := parseEnv(body)
	if err != nil {
		return Config{}, err
	}
	cfg := Config{
		Path:       path,
		ServerURL:  disk[keyURL],
		WorkerID:   disk[keyWorker],
		OrgID:      disk[keyOrg],
		Credential: disk[keyCred],
	}
	if v := disk[keyInsecure]; v != "" {
		b, err := parseBool(v)
		if err != nil {
			return Config{}, errors.New("insecure dev flag is not valid")
		}
		cfg.InsecureDev = b
	}
	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

// Validate checks the server origin, ids and credential shape.
func (c Config) Validate() error {
	if err := ValidateServerURL(c.ServerURL, c.InsecureDev); err != nil {
		return err
	}
	if !workerproto.ValidID(c.WorkerID) {
		return errors.New("worker id is not valid")
	}
	if c.OrgID != "" && !workerproto.ValidID(c.OrgID) {
		return errors.New("org id is not valid")
	}
	if !strings.HasPrefix(c.Credential, workerproto.PrefixCredential) || !secretOK(c.Credential) {
		return errors.New("credential is not usable")
	}
	return nil
}

// Write stores c at path atomically, mode 0600.
func Write(path string, c Config) error {
	if err := c.Validate(); err != nil {
		return err
	}
	vals := map[string]string{keyURL: c.ServerURL, keyWorker: c.WorkerID, keyCred: c.Credential}
	order := []string{keyURL, keyWorker, keyOrg, keyCred, keyInsecure}
	if c.OrgID != "" {
		vals[keyOrg] = c.OrgID
	}
	if c.InsecureDev {
		vals[keyInsecure] = "true"
	}
	return writeEnv(path, vals, order)
}

// ValidateServerURL accepts an https origin, and http only on loopback with
// insecureDev (protocol v1 §1: the Mac dev loop with AUTH=none). No path,
// query, fragment or userinfo.
func ValidateServerURL(raw string, insecureDev bool) error {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || u.User != nil {
		return errors.New("server url is not valid")
	}
	if (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" {
		return errors.New("server url must be an origin")
	}
	switch u.Scheme {
	case "https":
		return nil
	case "http":
		host := strings.ToLower(u.Hostname())
		if insecureDev && (host == "127.0.0.1" || host == "localhost" || host == "::1") {
			return nil
		}
		return errors.New("server url must use https")
	default:
		return errors.New("server url must use https")
	}
}

// ReadJoinToken reads one trimmed line from r (stdin, never argv).
func ReadJoinToken(r io.Reader) (string, error) {
	if r == nil {
		return "", errors.New("join token is not usable")
	}
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 256), 512)
	if !sc.Scan() {
		return "", errors.New("join token is not usable")
	}
	return strings.TrimSpace(sc.Text()), nil
}

// JoinTokenOK reports whether v looks like a dd_wjn_ join token.
func JoinTokenOK(v string) bool {
	return joinTokenRE.MatchString(v)
}

// ParseEnv parses an env file body (KEY=value lines, # comments, optional
// "export ", double-quoted values). DD: exported for secrets.env.
func ParseEnv(body []byte) (map[string]string, error) {
	m, _, err := parseEnv(body)
	return m, err
}

func parseEnv(body []byte) (map[string]string, []string, error) {
	out := map[string]string{}
	var order []string
	sc := bufio.NewScanner(bytes.NewReader(body))
	sc.Buffer(make([]byte, 0, 64*1024), 64*1024)
	lineNo := 0
	for sc.Scan() {
		lineNo++
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimPrefix(line, "export ")
		k, v, found := strings.Cut(line, "=")
		if !found {
			return nil, nil, fmt.Errorf("env file line %d", lineNo)
		}
		k = strings.TrimSpace(k)
		if k == "" || strings.ContainsAny(k, " \t") {
			return nil, nil, fmt.Errorf("env file line %d", lineNo)
		}
		v = strings.TrimSpace(v)
		if len(v) >= 2 && v[0] == '"' && v[len(v)-1] == '"' {
			u, err := unquote(v)
			if err != nil {
				return nil, nil, fmt.Errorf("env file line %d", lineNo)
			}
			v = u
		}
		if _, exists := out[k]; !exists {
			order = append(order, k)
		}
		out[k] = v
	}
	if err := sc.Err(); err != nil {
		return nil, nil, errors.New("env file is not readable")
	}
	return out, order, nil
}

func unquote(v string) (string, error) {
	inner := v[1 : len(v)-1]
	var b strings.Builder
	esc := false
	for _, r := range inner {
		if esc {
			b.WriteRune(r)
			esc = false
			continue
		}
		if r == '\\' {
			esc = true
			continue
		}
		b.WriteRune(r)
	}
	if esc {
		return "", errors.New("escape")
	}
	return b.String(), nil
}

func writeEnv(path string, vals map[string]string, order []string) error {
	var b strings.Builder
	seen := map[string]bool{}
	for _, k := range order {
		v, ok := vals[k]
		if !ok {
			continue
		}
		if err := writeKV(&b, k, v); err != nil {
			return err
		}
		seen[k] = true
	}
	for k, v := range vals {
		if seen[k] {
			continue
		}
		if err := writeKV(&b, k, v); err != nil {
			return err
		}
	}
	// DD: the credential dir ({state}/credential) is 0700 (worker-w3.md §3.2).
	return atomicWrite(path, []byte(b.String()), 0o600, 0o700)
}

func writeKV(b *strings.Builder, k, v string) error {
	if strings.ContainsAny(k, "=\r\n #") || strings.ContainsAny(v, "\r\n") {
		return errors.New("env value is not usable")
	}
	b.WriteString(k)
	b.WriteByte('=')
	if strings.ContainsAny(v, " \t#\"\\") {
		b.WriteByte('"')
		b.WriteString(strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(v))
		b.WriteByte('"')
	} else {
		b.WriteString(v)
	}
	b.WriteByte('\n')
	return nil
}

func parseBool(v string) (bool, error) {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "1", "true", "yes", "on":
		return true, nil
	case "0", "false", "no", "off":
		return false, nil
	default:
		return false, errors.New("bool")
	}
}

func secretOK(v string) bool {
	if v == "" || len(v) > 4096 {
		return false
	}
	for _, r := range v {
		if r < 0x21 || r > 0x7e {
			return false
		}
	}
	return true
}

// SanitizeHostname lowercases and replaces characters outside [a-z0-9-]
// so the enroll frame matches the contract. The result is 1–63 characters.
func SanitizeHostname(s string) (string, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", errors.New("hostname is empty")
	}
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' {
			b.WriteRune(r)
		} else {
			b.WriteByte('-')
		}
	}
	out := b.String()
	if len(out) > 63 {
		out = out[:63]
	}
	if !hostnameRE.MatchString(out) {
		return "", errors.New("hostname is not valid")
	}
	return out, nil
}

var hostnameRE = regexp.MustCompile(`^[a-z0-9-]{1,63}$`)

var joinTokenRE = regexp.MustCompile(`^dd_wjn_[A-Za-z0-9_-]{16,128}$`)
