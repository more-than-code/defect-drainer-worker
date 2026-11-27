//go:build linux

package selfupdate

import (
	"errors"
	"io"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

// takeRequest is the root step's read of the worker's request: it opens
// {state}, then update/ and request with O_NOFOLLOW from there (so a link
// the worker planted is never followed), requires update/ and request to
// belong to the state dir's owner and request to be a small regular file,
// reads it and deletes it. A missing request is os.ErrNotExist.
func takeRequest(stateDir string) ([]byte, error) {
	sfd, err := unix.Open(stateDir, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	defer unix.Close(sfd)
	var sst unix.Stat_t
	if err := unix.Fstat(sfd, &sst); err != nil {
		return nil, err
	}
	dfd, err := unix.Openat(sfd, RequestDirName, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		if errors.Is(err, unix.ENOENT) {
			return nil, os.ErrNotExist
		}
		return nil, err
	}
	defer unix.Close(dfd)
	var dst unix.Stat_t
	if err := unix.Fstat(dfd, &dst); err != nil {
		return nil, err
	}
	if dst.Uid != sst.Uid {
		return nil, errors.New("update dir has another owner")
	}
	fd, err := unix.Openat(dfd, RequestName, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		if errors.Is(err, unix.ENOENT) {
			return nil, os.ErrNotExist
		}
		// A link or anything else that does not open: remove it all the
		// same, so the path unit does not fire on it again.
		clearRequest(dfd)
		return nil, err
	}
	f := os.NewFile(uintptr(fd), RequestName)
	defer f.Close()
	// Delete it whatever it holds: a bad request is not retried.
	clearRequest(dfd)
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		return nil, err
	}
	if st.Mode&unix.S_IFMT != unix.S_IFREG || st.Uid != sst.Uid || st.Size > MaxRequest {
		return nil, errors.New("request is not a small regular file of the worker's")
	}
	return io.ReadAll(io.LimitReader(f, MaxRequest+1))
}

// clearRequest removes whatever is at update/request: a file or link, or
// an empty directory. Never followed: unlinkat works on the entry itself.
func clearRequest(dfd int) {
	if err := unix.Unlinkat(dfd, RequestName, 0); errors.Is(err, unix.EISDIR) || errors.Is(err, unix.EPERM) {
		_ = unix.Unlinkat(dfd, RequestName, unix.AT_REMOVEDIR)
	}
}

// discardRequest removes the request without reading it (the root step
// could not run, so nothing is retried on a request it never answered).
func discardRequest(stateDir string) {
	dfd, err := unix.Open(filepath.Join(stateDir, RequestDirName), unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return
	}
	defer unix.Close(dfd)
	clearRequest(dfd)
}

// errLocked: another run holds the lock.
var errLocked = errors.New("another update-run holds the lock")

// holdLock takes an exclusive flock on path without waiting.
func holdLock(path string) (func(), error) {
	fd, err := unix.Open(path, unix.O_RDWR|unix.O_CREAT|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0o600)
	if err != nil {
		return nil, err
	}
	if err := unix.Flock(fd, unix.LOCK_EX|unix.LOCK_NB); err != nil {
		unix.Close(fd)
		if errors.Is(err, unix.EWOULDBLOCK) {
			return nil, errLocked
		}
		return nil, err
	}
	return func() { unix.Close(fd) }, nil
}
