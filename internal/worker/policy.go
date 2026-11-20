package worker

import (
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/more-than-code/defect-drainer-worker/internal/botagent/policy"
	"github.com/more-than-code/defect-drainer-worker/workerproto"
)

// Sandbox levels, strictest first (protocol v1 §6.5).
const (
	SandboxReadOnly  = "read-only"
	SandboxStrict    = "strict"
	SandboxWorkspace = "workspace"
)

// Policy defaults. A key absent from policy.toml takes these.
const (
	DefaultMaxTurns   = 80
	DefaultMaxWallSec = 2700
	DefaultMaxJobs    = 1
	MaxMaxJobs        = 16
	MaxMaxTurns       = 1000
	// MaxAllowedDomains bounds agent_allowed_domains; MaxMaxCostUSD bounds
	// max_cost_usd, the same ceiling the server puts on its own cap.
	MaxAllowedDomains = 16
	MaxMaxCostUSD     = 100
	// MaxDenySockets bounds agent_deny_sockets.
	MaxDenySockets = 32
	// DefaultAgentMaxCostUSD is the per-job cost cap when neither the server
	// nor this policy sets one (spec D5).
	DefaultAgentMaxCostUSD = 5
)

// KnownAdapters are the adapter names a policy may list (§5.1, Q4).
var KnownAdapters = []string{"claude", "grok", "codex", "antigravity"}

var policySchema = policy.Schema{
	"enabled":               policy.Bool,
	"orgs":                  policy.Strings,
	"adapters":              policy.Strings,
	"repos":                 policy.Strings,
	"min_sandbox":           policy.String,
	"max_turns":             policy.Int,
	"max_wall_sec":          policy.Int,
	"max_jobs":              policy.Int,
	"allow_verify_commands": policy.Bool,
	"allow_ship":            policy.Bool,
	"agent_allowed_domains": policy.Strings,
	"max_cost_usd":          policy.Int,
	"agent_browser":         policy.String,
	"agent_deny_sockets":    policy.Strings,
	"allowed_models":        policy.Strings,
}

// Policy is /etc/dd-worker/policy.toml as the worker enforces it. When
// Valid is false every offer is declined policy_denied and Reason says why.
type Policy struct {
	Exists              bool
	Valid               bool
	Reason              string
	Hash                string
	Enabled             bool
	Orgs                []string
	Adapters            []string
	Repos               []string
	MinSandbox          string
	MaxTurns            int
	MaxWallSec          int
	MaxJobs             int
	AllowVerifyCommands bool
	AllowShip           bool
	// AllowedDomains are the exact hosts an agent's sandbox may reach
	// (empty: no network). MaxCostUSD lowers the server's per-job cost cap
	// (0: unset). Whole dollars: the policy file has no decimals.
	AllowedDomains []string
	MaxCostUSD     int
	// AgentBrowser is the host's headless browser ("": no browser jobs).
	// Setting it also gives a browser job's sandbox unix sockets, with every
	// host socket the worker could reach masked (B1, B2). DenySockets are
	// extra paths that sandbox hides.
	AgentBrowser string
	DenySockets  []string
	// AllowedModels are the models a job may run on, matched exactly (an
	// alias does not cover a full id). Nil: any model; the file may not
	// set an empty list.
	AllowedModels []string

	repoPatterns []repoURL
}

// LoadPolicy reads path with bot-agent's file checks (root owner, not group-
// or world-writable, size) unless skipOwner, then applies DD's keys and
// defaults. uid overrides the owner for tests.
func LoadPolicy(path string, skipOwner bool, uid func(os.FileInfo) (uint32, bool)) Policy {
	f := policy.Load(policy.Files{Policy: path, Schema: policySchema, UID: uid, SkipOwner: skipOwner})
	p := Policy{Exists: f.Exists, Hash: f.Hash, Reason: f.Reason, MaxJobs: DefaultMaxJobs}
	if !f.Exists {
		p.Reason = "missing"
		return p
	}
	if !f.Valid {
		return p
	}
	v := f.Values
	p.Enabled = v["enabled"].Bool
	p.Orgs = v["orgs"].Strings
	p.Adapters = append([]string(nil), KnownAdapters...)
	if a, ok := v["adapters"]; ok {
		p.Adapters = a.Strings
	}
	p.Repos = v["repos"].Strings
	p.MinSandbox = SandboxWorkspace
	if s, ok := v["min_sandbox"]; ok {
		p.MinSandbox = s.String
	}
	p.MaxTurns = intOr(v, "max_turns", DefaultMaxTurns)
	p.MaxWallSec = intOr(v, "max_wall_sec", DefaultMaxWallSec)
	p.MaxJobs = intOr(v, "max_jobs", DefaultMaxJobs)
	p.AllowVerifyCommands = v["allow_verify_commands"].Bool
	p.AllowShip = v["allow_ship"].Bool
	p.AllowedDomains = v["agent_allowed_domains"].Strings
	p.AgentBrowser = v["agent_browser"].String
	p.DenySockets = v["agent_deny_sockets"].Strings
	if m, ok := v["allowed_models"]; ok {
		if len(m.Strings) == 0 { // [] would decline every job
			p.Reason = "bad_allowed_models"
			return p
		}
		p.AllowedModels = m.Strings
	}
	if c, ok := v["max_cost_usd"]; ok {
		if c.Int < 1 { // 0 would read as unset
			p.Reason = "bad_max_cost_usd"
			return p
		}
		p.MaxCostUSD = c.Int
	}
	if reason := p.check(); reason != "" {
		p.Reason = reason
		return p
	}
	// A browser that exists must be root's alone (B1). One that is missing
	// leaves the policy valid; readiness reports it missing.
	if p.AgentBrowser != "" && !skipOwner {
		if r := browserFileReason(p.AgentBrowser, uid); r != "" && r != workerproto.ReasonMissing {
			p.Reason = "bad_agent_browser"
			return p
		}
	}
	p.Valid = true
	return p
}

func intOr(v map[string]policy.Value, key string, def int) int {
	if x, ok := v[key]; ok {
		return x.Int
	}
	return def
}

// check validates the values; a bad one invalidates the whole file.
func (p *Policy) check() string {
	if len(p.Orgs) == 0 {
		return "orgs_empty"
	}
	for _, o := range p.Orgs {
		if !workerproto.ValidID(o) {
			return "bad_org"
		}
	}
	if len(p.Adapters) > workerproto.MaxAdapters {
		return "bad_adapters"
	}
	seen := map[string]bool{}
	for _, a := range p.Adapters {
		if !known(a) || seen[a] {
			return "bad_adapters"
		}
		seen[a] = true
	}
	if sandboxRank(p.MinSandbox) < 0 {
		return "bad_min_sandbox"
	}
	if p.MaxTurns < 1 || p.MaxTurns > MaxMaxTurns {
		return "bad_max_turns"
	}
	if p.MaxWallSec < 60 {
		return "bad_max_wall_sec"
	}
	if p.MaxJobs < 1 || p.MaxJobs > MaxMaxJobs {
		return "bad_max_jobs"
	}
	if !validDomains(p.AllowedDomains) {
		return "bad_agent_allowed_domains"
	}
	if p.MaxCostUSD < 0 || p.MaxCostUSD > MaxMaxCostUSD {
		return "bad_max_cost_usd"
	}
	if p.AgentBrowser != "" && !cleanAbs(p.AgentBrowser) {
		return "bad_agent_browser"
	}
	if !validDenySockets(p.DenySockets) {
		return "bad_agent_deny_sockets"
	}
	if !validModels(p.AllowedModels) {
		return "bad_allowed_models"
	}
	p.repoPatterns = nil
	for _, r := range p.Repos {
		u, err := canonicalRepo(r, true)
		if err != nil {
			return "bad_repos"
		}
		p.repoPatterns = append(p.repoPatterns, u)
	}
	return ""
}

// validModels checks allowed_models: at most MaxAllowedModels non-empty
// model names (the offer's model pattern), no duplicates.
func validModels(models []string) bool {
	if len(models) > workerproto.MaxAllowedModels {
		return false
	}
	seen := map[string]bool{}
	for _, m := range models {
		if m == "" || !workerproto.ValidModel(m) || seen[m] {
			return false
		}
		seen[m] = true
	}
	return true
}

// domainLabel is one lowercase DNS label.
var domainLabel = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)

// validDomains checks agent_allowed_domains: at most MaxAllowedDomains
// exact, lowercase hostnames of two or more labels, no duplicates. No
// wildcard, IP address or port can pass the label check, and a last label
// of digits only (an IPv4 address) is refused.
func validDomains(ds []string) bool {
	if len(ds) > MaxAllowedDomains {
		return false
	}
	seen := map[string]bool{}
	for _, d := range ds {
		if len(d) == 0 || len(d) > 253 || seen[d] {
			return false
		}
		seen[d] = true
		labels := strings.Split(d, ".")
		if len(labels) < 2 {
			return false
		}
		for _, l := range labels {
			if !domainLabel.MatchString(l) {
				return false
			}
		}
		if strings.Trim(labels[len(labels)-1], "0123456789") == "" {
			return false
		}
	}
	return true
}

// cleanAbs reports whether p is an absolute, clean path other than "/".
func cleanAbs(p string) bool {
	return filepath.IsAbs(p) && filepath.Clean(p) == p && p != "/" && !strings.ContainsAny(p, "\x00\n\r")
}

// validDenySockets checks agent_deny_sockets: at most MaxDenySockets
// absolute, clean paths, none "/", no duplicates.
func validDenySockets(ps []string) bool {
	if len(ps) > MaxDenySockets {
		return false
	}
	seen := map[string]bool{}
	for _, p := range ps {
		if !cleanAbs(p) || seen[p] {
			return false
		}
		seen[p] = true
	}
	return true
}

func known(a string) bool {
	for _, k := range KnownAdapters {
		if a == k {
			return true
		}
	}
	return false
}

// Wire is the policy as hello and status report it (§4.1). masked_sockets
// is empty here; the worker adds its last scan (Worker.wirePolicy).
func (p Policy) Wire() workerproto.Policy {
	w := workerproto.Policy{
		Valid: p.Valid, Enabled: p.Valid && p.Enabled,
		Orgs: nonNil(p.Orgs), Adapters: nonNil(p.Adapters), Repos: nonNil(p.Repos),
		MinSandbox: p.MinSandbox, MaxTurns: p.MaxTurns, MaxWallSec: p.MaxWallSec,
		AllowVerifyCommands: p.AllowVerifyCommands, AllowShip: p.AllowShip,
		AllowedDomains: nonNil(p.AllowedDomains), MaxCostUSD: p.MaxCostUSD,
		Browser: p.AgentBrowser, MaskedSockets: []string{},
		AllowedModels: p.AllowedModels,
	}
	if !p.Valid {
		w.Orgs, w.Adapters, w.Repos = []string{}, []string{}, []string{}
		w.AllowVerifyCommands, w.AllowShip = false, false
		w.AllowedDomains, w.MaxCostUSD, w.Browser, w.MaskedSockets = []string{}, 0, "", []string{}
		w.AllowedModels = nil
	}
	return w
}

func nonNil(xs []string) []string {
	if xs == nil {
		return []string{}
	}
	return xs
}

// Usable reports whether the policy lets the worker take any job.
func (p Policy) Usable() bool { return p.Valid && p.Enabled }

// HasOrg reports whether org is listed.
func (p Policy) HasOrg(org string) bool {
	for _, o := range p.Orgs {
		if o == org {
			return true
		}
	}
	return false
}

// AdapterAllowed reports whether name is in policy.adapters.
func (p Policy) AdapterAllowed(name string) bool {
	for _, a := range p.Adapters {
		if a == name {
			return true
		}
	}
	return false
}

// ModelAllowed reports whether a job of adapter may run on model under
// allowed_models (workerproto.ModelAllowed).
func (p Policy) ModelAllowed(adapter, model string) bool {
	return workerproto.ModelAllowed(p.AllowedModels, adapter, model)
}

// RepoAllowed canonicalises raw and matches it against policy.repos: the
// host exactly, the path segment by segment, "*" matching one segment.
func (p Policy) RepoAllowed(raw string) bool {
	u, err := canonicalRepo(raw, false)
	if err != nil {
		return false
	}
	for _, pat := range p.repoPatterns {
		if pat.matches(u) {
			return true
		}
	}
	return false
}

// sandboxRank orders sandboxes: higher is stricter. -1 is unknown.
func sandboxRank(s string) int {
	switch s {
	case SandboxWorkspace:
		return 0
	case SandboxStrict:
		return 1
	case SandboxReadOnly:
		return 2
	default:
		return -1
	}
}

// EffectiveSandbox is the stricter of the spec's sandbox ("" is strict, as
// exec treats it) and the host's floor.
func EffectiveSandbox(spec, floor string) string {
	if spec == "" {
		spec = SandboxStrict
	}
	if sandboxRank(spec) < 0 {
		spec = SandboxStrict
	}
	if sandboxRank(floor) > sandboxRank(spec) {
		return floor
	}
	return spec
}

// --- Repo URLs (§6.5) ---

// repoURL is a canonical repo URL: scheme, lowercase host (with port), and
// path segments without a trailing ".git".
type repoURL struct {
	scheme string // "https" or "ssh"
	user   string // "git" for ssh, "" for https
	host   string
	segs   []string
}

var (
	hostRe    = regexp.MustCompile(`^[a-z0-9]([a-z0-9.-]*[a-z0-9])?(:[0-9]{1,5})?$`)
	repoSegRe = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)
	scpRe     = regexp.MustCompile(`^git@([^:/]+):(.+)$`)
)

var errRepoURL = errors.New("repo url refused")

// CanonicalRepoURL returns the canonical form the worker clones and hashes.
func CanonicalRepoURL(raw string) (string, error) {
	u, err := canonicalRepo(raw, false)
	if err != nil {
		return "", err
	}
	return u.String(), nil
}

func (u repoURL) String() string {
	if u.scheme == "ssh" {
		return "ssh://git@" + u.host + "/" + strings.Join(u.segs, "/")
	}
	return "https://" + u.host + "/" + strings.Join(u.segs, "/")
}

func (pat repoURL) matches(u repoURL) bool {
	if pat.host != u.host || len(pat.segs) != len(u.segs) {
		return false
	}
	for i, s := range pat.segs {
		if s != "*" && s != u.segs[i] {
			return false
		}
	}
	return true
}

// canonicalRepo applies §6.5: https or ssh/git@ only; no userinfo (other
// than ssh's "git" user); no ext::, file: or leading "-"; lowercase host;
// strip a trailing ".git". pattern allows "*" segments.
func canonicalRepo(raw string, pattern bool) (repoURL, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" || strings.HasPrefix(raw, "-") || strings.ContainsAny(raw, " \t\r\n\x00\\") {
		return repoURL{}, errRepoURL
	}
	lower := strings.ToLower(raw)
	if strings.HasPrefix(lower, "ext::") || strings.HasPrefix(lower, "file:") || strings.Contains(raw, "::") {
		return repoURL{}, errRepoURL
	}
	var out repoURL
	var path string
	if m := scpRe.FindStringSubmatch(raw); m != nil && !strings.Contains(raw, "://") {
		out = repoURL{scheme: "ssh", user: "git", host: strings.ToLower(m[1])}
		path = m[2]
	} else {
		u, err := url.Parse(raw)
		if err != nil || u.Opaque != "" || u.RawQuery != "" || u.Fragment != "" || u.ForceQuery {
			return repoURL{}, errRepoURL
		}
		switch strings.ToLower(u.Scheme) {
		case "https":
			if u.User != nil {
				return repoURL{}, errRepoURL
			}
			out = repoURL{scheme: "https"}
		case "ssh":
			if u.User != nil {
				if _, hasPw := u.User.Password(); hasPw || u.User.Username() != "git" {
					return repoURL{}, errRepoURL
				}
			}
			out = repoURL{scheme: "ssh", user: "git"}
		default:
			return repoURL{}, errRepoURL
		}
		out.host = strings.ToLower(u.Host)
		path = u.Path
	}
	if !hostRe.MatchString(out.host) {
		return repoURL{}, errRepoURL
	}
	path = strings.Trim(path, "/")
	path = strings.TrimSuffix(path, ".git")
	if path == "" {
		return repoURL{}, errRepoURL
	}
	for _, s := range strings.Split(path, "/") {
		if s == "*" && pattern {
			out.segs = append(out.segs, s)
			continue
		}
		if s == "" || s == "." || s == ".." || strings.HasPrefix(s, "-") || !repoSegRe.MatchString(s) {
			return repoURL{}, errRepoURL
		}
		out.segs = append(out.segs, s)
	}
	return out, nil
}
