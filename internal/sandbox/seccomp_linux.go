//go:build linux && (amd64 || arm64)

package sandbox

import "golang.org/x/sys/unix"

// commonEPERM are denied on every supported architecture.
var commonEPERM = []uint32{
	// mounts, root and namespaces
	unix.SYS_MOUNT, unix.SYS_UMOUNT2, unix.SYS_PIVOT_ROOT, unix.SYS_CHROOT,
	unix.SYS_OPEN_TREE, unix.SYS_MOVE_MOUNT, unix.SYS_FSOPEN, unix.SYS_FSCONFIG,
	unix.SYS_FSMOUNT, unix.SYS_FSPICK, unix.SYS_MOUNT_SETATTR,
	unix.SYS_UNSHARE, unix.SYS_SETNS,
	// kernel code, BPF, perf, keyrings, userfaultfd
	unix.SYS_INIT_MODULE, unix.SYS_FINIT_MODULE, unix.SYS_DELETE_MODULE,
	unix.SYS_KEXEC_LOAD, unix.SYS_KEXEC_FILE_LOAD,
	unix.SYS_BPF, unix.SYS_PERF_EVENT_OPEN,
	unix.SYS_KEYCTL, unix.SYS_ADD_KEY, unix.SYS_REQUEST_KEY,
	unix.SYS_USERFAULTFD,
	// file handles (open_by_handle_at walks out of a bind mount)
	unix.SYS_OPEN_BY_HANDLE_AT, unix.SYS_NAME_TO_HANDLE_AT,
	// clock, power, swap, quotas, accounting, kernel log
	unix.SYS_SETTIMEOFDAY, unix.SYS_CLOCK_SETTIME, unix.SYS_CLOCK_ADJTIME, unix.SYS_ADJTIMEX,
	unix.SYS_REBOOT, unix.SYS_SWAPON, unix.SYS_SWAPOFF,
	unix.SYS_QUOTACTL, unix.SYS_QUOTACTL_FD, unix.SYS_ACCT,
	unix.SYS_SYSLOG, unix.SYS_VHANGUP, unix.SYS_LOOKUP_DCOOKIE,
	unix.SYS_FANOTIFY_INIT, unix.SYS_KCMP,
}

// commonENOSYS are denied with ENOSYS so callers fall back.
var commonENOSYS = []uint32{
	unix.SYS_CLONE3,
	unix.SYS_IO_URING_SETUP, unix.SYS_IO_URING_ENTER, unix.SYS_IO_URING_REGISTER,
}

// seccompFilter is the verify sandbox's filter for this architecture.
func seccompFilter() ([]byte, error) {
	t := archTable()
	t.eperm = append(append([]uint32{}, commonEPERM...), t.eperm...)
	t.enosys = append(append([]uint32{}, commonENOSYS...), t.enosys...)
	return t.program()
}
