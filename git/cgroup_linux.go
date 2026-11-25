package git

import "syscall"

// IntoCgroup makes a child start in the cgroup v2 dir open at fd
// (CLONE_INTO_CGROUP, Linux 5.7): the child is in the job's cgroup from its
// first instruction, so nothing it forks escapes the limits. fd <= 0 leaves
// the child in the caller's cgroup.
func IntoCgroup(attr *syscall.SysProcAttr, fd int) {
	if attr == nil || fd <= 0 {
		return
	}
	attr.UseCgroupFD = true
	attr.CgroupFD = fd
}
