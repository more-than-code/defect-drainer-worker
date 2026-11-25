package git

import (
	"errors"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

// openAgentLog opens dir/agent.log for appending. dir is the job's handoff,
// which the agent can write, and the worker appends to the log as itself:
// a symlink or hardlink the agent planted at that name must not send the
// worker's writes to another file. The name is opened relative to dir with
// O_NOFOLLOW; anything there that is not a regular file with one link is
// unlinked and the log starts again.
func openAgentLog(dir string) (*os.File, error) {
	dfd, err := unix.Open(dir, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: dir, Err: err}
	}
	defer unix.Close(dfd)
	path := filepath.Join(dir, "agent.log")
	const name = "agent.log"
	fd, err := unix.Openat(dfd, name, unix.O_WRONLY|unix.O_APPEND|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err == nil {
		var st unix.Stat_t
		if unix.Fstat(fd, &st) == nil && st.Mode&unix.S_IFMT == unix.S_IFREG && st.Nlink == 1 {
			if err := clearNonblock(fd); err == nil {
				return os.NewFile(uintptr(fd), path), nil
			}
		}
		unix.Close(fd)
	}
	// Missing, or not a plain file of its own (a symlink fails the open with
	// ELOOP; a FIFO, a device or a hardlink fails the checks): replace it.
	if err := unix.Unlinkat(dfd, name, 0); err != nil && !errors.Is(err, unix.ENOENT) {
		return nil, &os.PathError{Op: "unlink", Path: path, Err: err}
	}
	fd, err = unix.Openat(dfd, name, unix.O_WRONLY|unix.O_APPEND|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0o644)
	if err != nil {
		return nil, &os.PathError{Op: "create", Path: path, Err: err}
	}
	return os.NewFile(uintptr(fd), path), nil
}

func clearNonblock(fd int) error {
	return unix.SetNonblock(fd, false)
}
