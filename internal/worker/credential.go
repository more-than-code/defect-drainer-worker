package worker

import (
	"errors"
	"fmt"
	"os"
)

// MigrateCredential is the one-time move of a pre-W3 credential
// ({config}/env, env.prev) to {state}/credential/ (docs/worker-w3.md §3.2).
// It does nothing when there is no old env, or when the new env already
// exists. The move is a rename; when it fails (a root-owned /etc, or another
// filesystem) it logs the exact commands for the host owner and returns an
// error, so dd-worker run exits 1 instead of running from /etc.
func MigrateCredential(p Paths, logf func(string, ...any)) error {
	if err := CheckCredentialDir(p); err != nil {
		return err
	}
	if _, err := os.Lstat(p.LegacyEnv()); err != nil {
		return nil
	}
	if _, err := os.Lstat(p.Env()); err == nil {
		logf("%s is a stale copy: the credential in use is %s", p.LegacyEnv(), p.Env())
		return nil
	}
	if err := os.MkdirAll(p.CredentialDir(), 0o700); err != nil {
		return err
	}
	moves := [][2]string{{p.LegacyEnv(), p.Env()}}
	if _, err := os.Lstat(p.LegacyEnvPrev()); err == nil {
		moves = append([][2]string{{p.LegacyEnvPrev(), p.EnvPrev()}}, moves...)
	}
	for i, m := range moves {
		if err := os.Rename(m[0], m[1]); err != nil {
			logf("the credential moved to %s in this version, and it cannot be moved from %s (%v). As root, run:", p.CredentialDir(), p.ConfigDir, errors.Unwrap(err))
			for _, r := range moves[i:] {
				logf("  mv %s %s", r[0], r[1])
			}
			return fmt.Errorf("credential still in %s: move it as logged, then start again", p.ConfigDir)
		}
	}
	logf("credential moved from %s to %s", p.ConfigDir, p.CredentialDir())
	return nil
}

// CheckCredentialDir refuses a {state}/credential that is a symlink or not
// a directory: the credential is never written or read through a link.
// A missing dir is fine (it is created at mode 0700), and so is an empty one
// install.sh pre-created at 0700; one open to group or others is refused.
func CheckCredentialDir(p Paths) error {
	fi, err := os.Lstat(p.CredentialDir())
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("credential dir: %w", err)
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("%s must not be a symlink", p.CredentialDir())
	}
	if !fi.IsDir() {
		return fmt.Errorf("%s must be a directory", p.CredentialDir())
	}
	if fi.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("%s must not be open to group or others (want mode 0700)", p.CredentialDir())
	}
	return nil
}
