package sandbox

import "golang.org/x/sys/unix"

func archTable() seccompTable {
	return seccompTable{
		auditArch:   unix.AUDIT_ARCH_X86_64,
		x32Bit:      0x40000000, // __X32_SYSCALL_BIT
		clone:       unix.SYS_CLONE,
		socket:      unix.SYS_SOCKET,
		personality: unix.SYS_PERSONALITY,
		eperm: []uint32{
			unix.SYS_IOPL, unix.SYS_IOPERM,
			unix.SYS_CREATE_MODULE, unix.SYS_GET_KERNEL_SYMS, unix.SYS_QUERY_MODULE,
			unix.SYS_NFSSERVCTL, unix.SYS_USELIB, unix.SYS_USTAT, unix.SYS_SYSFS,
			unix.SYS__SYSCTL, unix.SYS_MODIFY_LDT,
		},
	}
}
