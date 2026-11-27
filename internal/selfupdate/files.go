package selfupdate

import (
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/more-than-code/defect-drainer-worker/workerproto"
)

// Host paths (ADR 0002 §6). The request lives in the worker's own state
// dir; everything the root step writes lives in RootDir, which only root
// may write and the worker may read.
const (
	RequestDirName = "update"  // {state}/update, the worker's, 0700
	RequestName    = "request" // {state}/update/request
	DefaultRootDir = "/var/lib/dd-worker-update"
	ResultName     = "result.json" // {RootDir}/result.json, root 0644
	DefaultLock    = "/run/dd-worker-update.lock"
	// MaxRequest bounds the request file; MaxResult the result file.
	MaxRequest = 64
	MaxResult  = 4096
)

// requestRE is the only request the root step reads: one version line.
var requestRE = regexp.MustCompile(`^[0-9]{1,4}\.[0-9]{1,4}\.[0-9]{1,4}(-rc[1-9][0-9]{0,3})?$`)

// ValidRequest reports whether b (the request file's bytes, one line with
// an optional newline) names a version, and returns it.
func ValidRequest(b []byte) (string, bool) {
	if len(b) == 0 || len(b) > MaxRequest {
		return "", false
	}
	s := strings.TrimSuffix(string(b), "\n")
	if !requestRE.MatchString(s) {
		return "", false
	}
	return s, true
}

// WriteRequest is the worker's side: it writes version to
// {state}/update/request by write-and-rename, creating the dir (0700) when
// missing. It never follows a link there.
func WriteRequest(stateDir, version string) error {
	if _, ok := ValidRequest([]byte(version)); !ok {
		return errors.New("bad version for an update request")
	}
	dir := filepath.Join(stateDir, RequestDirName)
	if err := os.Mkdir(dir, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
		return err
	}
	fi, err := os.Lstat(dir)
	if err != nil {
		return err
	}
	if !fi.IsDir() {
		return errors.New(dir + " is not a directory")
	}
	tmp, err := os.CreateTemp(dir, ".request-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.WriteString(version + "\n"); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), filepath.Join(dir, RequestName))
}

// RequestPending reports whether the worker's request file still exists
// (the root step deletes it when it reads it).
func RequestPending(stateDir string) bool {
	_, err := os.Lstat(filepath.Join(stateDir, RequestDirName, RequestName))
	return err == nil
}

// ReadResult reads {rootDir}/result.json; nil when there is none or it does
// not parse.
func ReadResult(rootDir string) *workerproto.SelfUpdateResult {
	f, err := os.Open(filepath.Join(rootDir, ResultName))
	if err != nil {
		return nil
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, MaxResult+1))
	if err != nil || len(b) > MaxResult {
		return nil
	}
	var r workerproto.SelfUpdateResult
	if json.Unmarshal(b, &r) != nil || r.Result == "" {
		return nil
	}
	return &r
}

// writeResult writes result.json in rootDir (root's) by write-and-rename,
// mode 0644.
func writeResult(rootDir string, r workerproto.SelfUpdateResult) error {
	b, err := json.Marshal(r)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(rootDir, ".result-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(append(b, '\n')); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(0o644); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), filepath.Join(rootDir, ResultName))
}
