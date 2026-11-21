package workerproto

import (
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"unicode"
)

var (
	idRe      = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,127}$`)
	sha256Re  = regexp.MustCompile(`^[0-9a-f]{64}$`)
	segmentRe = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)
	modelRe   = regexp.MustCompile(`^[A-Za-z0-9._/-]{0,64}$`)
)

// ValidID reports whether s is a valid job, lease, request or worker id (§2):
// no '.', ':' or '/', so it is safe as a path component.
func ValidID(s string) bool { return idRe.MatchString(s) }

// ValidSHA256 reports whether s is 64 lowercase hex characters (§2).
func ValidSHA256(s string) bool { return sha256Re.MatchString(s) }

// ValidModel reports whether s may be passed to an agent CLI as a model (§6.5).
func ValidModel(s string) bool { return modelRe.MatchString(s) && !strings.HasPrefix(s, "-") }

// ValidManagedURL reports whether s may be policy managed_url (§4.1): an
// https URL with a host, at most MaxManagedURL bytes, no user or password,
// and no space or control character. The worker refuses any other value and
// a server drops one before showing it as a link.
func ValidManagedURL(s string) bool {
	if s == "" || len(s) > MaxManagedURL || strings.ContainsFunc(s, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }) {
		return false
	}
	u, err := url.Parse(s)
	return err == nil && u.Scheme == "https" && u.Host != "" && u.User == nil && u.Opaque == ""
}

// ModelAllowed is the allowed_models rule, shared by the worker (which
// enforces it) and a scheduler (which can skip a worker that would decline).
// A nil or empty list allows any model. Otherwise the model must be listed
// exactly (case-sensitive; an alias such as "sonnet" does not cover a full
// "claude-…" id). An empty model, the CLI's own default, is refused for the
// adapters that take a model and allowed for grok, which has none.
func ModelAllowed(allowed []string, adapter, model string) bool {
	if len(allowed) == 0 {
		return true
	}
	if model == "" {
		return adapter == "grok"
	}
	for _, m := range allowed {
		if m == model {
			return true
		}
	}
	return false
}

// ValidRelPath reports whether p is a safe relative path (§2): not empty, not
// absolute, no backslash or NUL, no "." or ".." segment, and every segment
// made of [A-Za-z0-9._-].
func ValidRelPath(p string) bool {
	if p == "" || strings.HasPrefix(p, "/") || strings.ContainsAny(p, "\\\x00") {
		return false
	}
	for _, seg := range strings.Split(p, "/") {
		if seg == "" || seg == "." || seg == ".." || !segmentRe.MatchString(seg) {
			return false
		}
	}
	return true
}

// repoFileSegmentRe is one segment of a file path inside a repo: also the
// + @ ( ) [ ] ~ that route-file conventions use (SvelteKit +page.svelte,
// [id], (group); Next.js @slot).
var repoFileSegmentRe = regexp.MustCompile(`^[A-Za-z0-9._+@()\[\]~-]+$`)

// ValidRepoFilePath reports whether p is a file path inside a repo, as a
// ship control's repos[].files lists it: ValidRelPath's rules (not empty,
// not absolute, no backslash or NUL, no empty, "." or ".." segment) with
// repoFileSegmentRe's wider segments, none starting with "-". Repo names,
// evidence names and handoff paths keep ValidRelPath.
func ValidRepoFilePath(p string) bool {
	if p == "" || strings.HasPrefix(p, "/") || strings.ContainsAny(p, "\\\x00") {
		return false
	}
	for _, seg := range strings.Split(p, "/") {
		if seg == "" || seg == "." || seg == ".." || strings.HasPrefix(seg, "-") || !repoFileSegmentRe.MatchString(seg) {
			return false
		}
	}
	return true
}

// errMissing names a required key that is absent or empty.
func errMissing(key string) error { return fmt.Errorf("missing %s", key) }

func checkHeader(h Header, want string) error {
	if h.V != Version {
		return fmt.Errorf("unsupported v %d", h.V)
	}
	if h.Type != want {
		return fmt.Errorf("type %q, want %q", h.Type, want)
	}
	return nil
}

func checkID(key, v string) error {
	if v == "" {
		return errMissing(key)
	}
	if !ValidID(v) {
		return fmt.Errorf("invalid %s", key)
	}
	return nil
}

// Validate checks the required keys of each frame (§2). A receiver answers a
// failure with Error{Code: CodeBadFrame, RefType: frame type, Detail: err}.

func (f Enroll) Validate() error {
	if err := checkHeader(f.Header, TypeEnroll); err != nil {
		return err
	}
	if f.Hostname == "" {
		return errMissing("hostname")
	}
	if !ValidSHA256(f.MachineIDHash) {
		return errors.New("invalid machine_id_hash")
	}
	if f.Version == "" || f.OS == "" || f.Arch == "" {
		return errMissing("version, os or arch")
	}
	return nil
}

func (f Hello) Validate() error {
	if err := checkHeader(f.Header, TypeHello); err != nil {
		return err
	}
	if err := checkID("worker_id", f.WorkerID); err != nil {
		return err
	}
	if f.Protocol < 1 {
		return errMissing("protocol")
	}
	if f.MaxJobs < 1 {
		return errMissing("max_jobs")
	}
	if len(f.Adapters) > MaxAdapters {
		return fmt.Errorf("more than %d adapters", MaxAdapters)
	}
	if err := ValidateTools(f.Tools); err != nil {
		return err
	}
	for _, id := range f.BusyJobIDs {
		if !ValidID(id) {
			return errors.New("invalid busy_job_ids entry")
		}
	}
	return nil
}

// ValidateTools checks hello's and status's optional tools (§4.1): at most
// MaxTools entries, each named git, gh, browser or postgres.
func ValidateTools(tools []ToolStatus) error {
	if len(tools) > MaxTools {
		return fmt.Errorf("more than %d tools", MaxTools)
	}
	for _, t := range tools {
		if t.Name != ToolGit && t.Name != ToolGh && t.Name != ToolBrowser && t.Name != ToolPostgres {
			return errors.New("invalid tools[].name")
		}
	}
	return nil
}

func (f Offer) Validate() error {
	if err := checkHeader(f.Header, TypeOffer); err != nil {
		return err
	}
	if err := checkID("job_id", f.JobID); err != nil {
		return err
	}
	if err := checkID("lease_id", f.LeaseID); err != nil {
		return err
	}
	if f.SpecURL == "" || f.JobToken == "" || f.LeaseUntil == "" {
		return errMissing("spec_url, job_token or lease_until")
	}
	if !ValidModel(f.Model) {
		return errors.New("invalid model")
	}
	return nil
}

func (f Accept) Validate() error {
	if err := checkHeader(f.Header, TypeAccept); err != nil {
		return err
	}
	if err := checkID("job_id", f.JobID); err != nil {
		return err
	}
	return checkID("lease_id", f.LeaseID)
}

func (f Decline) Validate() error {
	if err := checkHeader(f.Header, TypeDecline); err != nil {
		return err
	}
	if err := checkID("job_id", f.JobID); err != nil {
		return err
	}
	if err := checkID("lease_id", f.LeaseID); err != nil {
		return err
	}
	if f.Reason == "" {
		return errMissing("reason")
	}
	return nil
}

func (f Progress) Validate() error {
	if err := checkHeader(f.Header, TypeProgress); err != nil {
		return err
	}
	if err := checkID("job_id", f.JobID); err != nil {
		return err
	}
	if err := checkID("lease_id", f.LeaseID); err != nil {
		return err
	}
	if f.Seq < 1 {
		return errMissing("seq")
	}
	for _, e := range f.Events {
		if e.JobID != f.JobID {
			return errors.New("event jobId differs from frame job_id")
		}
	}
	return nil
}

func (f Control) Validate() error {
	if err := checkHeader(f.Header, TypeControl); err != nil {
		return err
	}
	if err := checkID("job_id", f.JobID); err != nil {
		return err
	}
	if err := checkID("request_id", f.RequestID); err != nil {
		return err
	}
	if f.Control.Kind == "" {
		return errMissing("control.kind")
	}
	if f.Control.JobID != f.JobID {
		return errors.New("control jobId differs from frame job_id")
	}
	if f.Control.Repo != "" && !ValidRelPath(f.Control.Repo) {
		return errors.New("invalid control.repo")
	}
	for _, r := range f.Control.Repos {
		if !ValidRelPath(r.Repo) {
			return errors.New("invalid control.repos[].repo")
		}
		for _, file := range r.Files {
			if !ValidRepoFilePath(file) {
				return errors.New("invalid control.repos[].files entry")
			}
		}
	}
	return nil
}

func (f ControlResult) Validate() error {
	if err := checkHeader(f.Header, TypeControlResult); err != nil {
		return err
	}
	if err := checkID("job_id", f.JobID); err != nil {
		return err
	}
	return checkID("request_id", f.RequestID)
}

func (f Ack) Validate() error {
	if err := checkHeader(f.Header, TypeAck); err != nil {
		return err
	}
	if err := checkID("job_id", f.JobID); err != nil {
		return err
	}
	if f.Seq < 1 {
		return errMissing("seq")
	}
	return nil
}
