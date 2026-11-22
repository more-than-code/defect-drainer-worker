package worker

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"slices"

	"github.com/more-than-code/defect-drainer-worker/exec"
	"github.com/more-than-code/defect-drainer-worker/internal/botagent/config"
)

// Job stack values after the run (§17, signin_random.v1). A run's {random}
// value, which is also a generated sign-in password, exists only in that
// run, but the diff and ship controls come later, maybe after a restart.
// Each run hands its values over before dd-stack is written
// (exec.Hooks.StackSecrets), and the worker keeps every run's values in the
// job dir, next to job.json, until the job is removed. A rerun's worktree
// still holds older runs' changes, so none is dropped.

// recordStackSecrets adds values to the job's stack-secrets.json (0600).
func (j *job) recordStackSecrets(values []string) error {
	j.stackMu.Lock()
	defer j.stackMu.Unlock()
	path := j.w.opts.Paths.JobStackSecrets(j.id)
	all, err := readStackSecrets(path)
	if err != nil {
		return err
	}
	for _, v := range values {
		if v != "" && !slices.Contains(all, v) {
			all = append(all, v)
		}
	}
	b, err := json.Marshal(all)
	if err != nil {
		return err
	}
	return config.WriteFileAtomic(path, b, 0o600, 0o700)
}

// stackSecrets are the values every run of the job recorded.
func (j *job) stackSecrets() ([]string, error) {
	j.stackMu.Lock()
	defer j.stackMu.Unlock()
	return readStackSecrets(j.w.opts.Paths.JobStackSecrets(j.id))
}

// signInPasswords are the Stack sign-in passwords the job's runs used: the
// typed one, or each run's generated one (its {random} value).
func (j *job) signInPasswords(spec exec.JobSpec) ([]string, error) {
	si := spec.Project.JobStackSignIn
	switch {
	case si == nil || si.Password == "":
		return nil, nil
	case !si.GeneratedPassword():
		return []string{si.Password}, nil
	}
	// A generated password's job records only {random} values.
	return j.stackSecrets()
}

// readStackSecrets reads path; a job that never wrote one has none.
func readStackSecrets(path string) ([]string, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var all []string
	if err := json.Unmarshal(b, &all); err != nil {
		return nil, err
	}
	return all, nil
}
