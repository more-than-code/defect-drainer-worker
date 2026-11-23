package sandbox

import "golang.org/x/sys/unix"

func archTable() seccompTable {
	return seccompTable{
		auditArch: unix.AUDIT_ARCH_AARCH64,
		clone:     unix.SYS_CLONE,
	}
}
