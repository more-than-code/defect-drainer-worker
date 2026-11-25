package exec

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"
)

// The handoff is the agent's writable cwd, but DD writes into it too, as the
// worker user and outside the agent's sandbox. An agent can leave a symlink
// (or a hardlink) where DD will write next, so a plain os.WriteFile there
// could overwrite any file the worker can write. These helpers walk the path
// from the handoff root with openat(2), refuse a symlinked directory on the
// way, unlink whatever sits at the target name and create a fresh file with
// O_EXCL|O_NOFOLLOW. Reads of agent files have the same rule in
// harvest_files.go.

// WriteHandoffFile writes data to root/rel as a new regular file with mode
// perm, creating missing parent directories under root. rel must be relative
// and stay inside root.
func WriteHandoffFile(root, rel string, data []byte, perm os.FileMode) error {
	dir, name, err := splitHandoffRel(rel)
	if err != nil {
		return err
	}
	dfd, err := openHandoffDir(root, dir)
	if err != nil {
		return err
	}
	defer unix.Close(dfd)
	if err := unix.Unlinkat(dfd, name, 0); err != nil && !errors.Is(err, unix.ENOENT) {
		return &os.PathError{Op: "unlink", Path: filepath.Join(root, rel), Err: err}
	}
	fd, err := unix.Openat(dfd, name, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, uint32(perm.Perm()))
	if err != nil {
		return &os.PathError{Op: "create", Path: filepath.Join(root, rel), Err: err}
	}
	f := os.NewFile(uintptr(fd), filepath.Join(root, rel))
	if err := f.Chmod(perm.Perm()); err != nil { // the umask may have narrowed it
		f.Close()
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// EnsureHandoffDir creates root/rel and any missing parents as real
// directories. It fails when a component already exists as a symlink or a
// non-directory.
func EnsureHandoffDir(root, rel string) error {
	fd, err := openHandoffDir(root, rel)
	if err != nil {
		return err
	}
	return unix.Close(fd)
}

func splitHandoffRel(rel string) (dir, name string, err error) {
	clean := filepath.Clean(rel)
	if rel == "" || filepath.IsAbs(clean) || clean == "." || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", "", fmt.Errorf("handoff path %q is not inside the handoff", rel)
	}
	return filepath.Dir(clean), filepath.Base(clean), nil
}

// openHandoffDir opens root, then each component of rel below it, creating
// missing ones. Every open is O_DIRECTORY|O_NOFOLLOW, so a symlinked component
// fails instead of being followed.
func openHandoffDir(root, rel string) (int, error) {
	fd, err := unix.Open(root, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return -1, &os.PathError{Op: "open", Path: root, Err: err}
	}
	clean := filepath.Clean(rel)
	if clean == "." || clean == "" {
		return fd, nil
	}
	if filepath.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		unix.Close(fd)
		return -1, fmt.Errorf("handoff path %q is not inside the handoff", rel)
	}
	at := root
	for _, part := range strings.Split(clean, string(filepath.Separator)) {
		at = filepath.Join(at, part)
		if err := unix.Mkdirat(fd, part, 0o755); err != nil && !errors.Is(err, unix.EEXIST) {
			unix.Close(fd)
			return -1, &os.PathError{Op: "mkdir", Path: at, Err: err}
		}
		next, err := unix.Openat(fd, part, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		unix.Close(fd)
		if err != nil {
			return -1, &os.PathError{Op: "open", Path: at, Err: err}
		}
		fd = next
	}
	return fd, nil
}
