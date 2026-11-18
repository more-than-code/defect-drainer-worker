package workerproto

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
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
// MaxTools entries, each named git or gh.
func ValidateTools(tools []ToolStatus) error {
	if len(tools) > MaxTools {
		return fmt.Errorf("more than %d tools", MaxTools)
	}
	for _, t := range tools {
		if t.Name != ToolGit && t.Name != ToolGh {
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
			if !ValidRelPath(file) {
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
