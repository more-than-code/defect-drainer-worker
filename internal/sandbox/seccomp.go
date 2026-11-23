package sandbox

import (
	"encoding/binary"
	"fmt"
)

// The verify sandbox's seccomp filter (DEF-…-f2rb): bwrap loads it with
// --seccomp just before it runs the wrapper, so the wrapper, the egress
// bridge and the command all run under it, and nothing inside can lift it.
//
// It is a deny list, like Docker's default profile in effect: the syscalls a
// build or test never needs and that widen the kernel surface. Denied calls
// fail with EPERM; clone3 and io_uring fail with ENOSYS, so libc and libuv
// fall back to clone and to the thread pool. A call from another
// architecture's table (i386 or x32 on amd64, arm32 on arm64) is denied.
//
// Inside the sandbox nothing may mount, change root, join or create a
// namespace (clone with a CLONE_NEW* flag, unshare, setns), load kernel
// code, use BPF, perf, keyrings, userfaultfd or file handles, or change the
// clock, swap, quotas, accounting or the machine's power state. ptrace and
// process_vm_* stay allowed: the sandbox has its own PID namespace, and some
// test tools (sanitizers, debuggers) need them.

// BPF opcodes and seccomp_data offsets (linux/filter.h, linux/seccomp.h).
const (
	bpfLdWAbs  = 0x20 // BPF_LD | BPF_W | BPF_ABS
	bpfJeqK    = 0x15 // BPF_JMP | BPF_JEQ | BPF_K
	bpfJgeK    = 0x35 // BPF_JMP | BPF_JGE | BPF_K
	bpfJsetK   = 0x45 // BPF_JMP | BPF_JSET | BPF_K
	bpfRetK    = 0x06 // BPF_RET | BPF_K
	offNr      = 0
	offArch    = 4
	offArg0Low = 16 // low 32 bits of args[0] (both supported arches are little-endian)

	retAllow  = 0x7fff0000 // SECCOMP_RET_ALLOW
	retErrno  = 0x00050000 // SECCOMP_RET_ERRNO
	errEPERM  = 1
	errENOSYS = 38
)

// cloneNewMask is every CLONE_NEW* flag clone takes in its low 32 bits:
// NEWNS, NEWCGROUP, NEWUTS, NEWIPC, NEWUSER, NEWPID, NEWNET (NEWTIME is
// clone3 and unshare only, both denied whole).
const cloneNewMask = 0x00020000 | 0x02000000 | 0x04000000 | 0x08000000 | 0x10000000 | 0x20000000 | 0x40000000

// seccompTable is one architecture's part of the filter.
type seccompTable struct {
	auditArch uint32
	// x32Bit: syscall numbers at or above it are another ABI on the same
	// audit arch (x32 on amd64); 0 means none.
	x32Bit uint32
	// eperm and enosys are denied whole, with that errno.
	eperm, enosys []uint32
	// clone is the clone syscall: allowed unless its flags ask for a new
	// namespace.
	clone uint32
}

type sockFilter struct {
	code   uint16
	jt, jf uint8
	k      uint32
}

// program is t as a classic BPF program in the kernel's struct sock_filter
// layout (8 bytes each, native little-endian), as bwrap --seccomp reads it.
func (t seccompTable) program() ([]byte, error) {
	// Layout: checks first, then three shared returns at the end (deny
	// EPERM, deny ENOSYS, allow). Jumps are forward and relative.
	type ins struct {
		sockFilter
		// to: -1 none; else a target label resolved after layout.
		jtTo, jfTo int
	}
	const (
		lNext = iota - 1 // fall through (offset 0)
		lEPERM
		lENOSYS
		lAllow
	)
	var p []ins
	add := func(code uint16, k uint32, jt, jf int) {
		p = append(p, ins{sockFilter{code: code, k: k}, jt, jf})
	}
	add(bpfLdWAbs, offArch, lNext, lNext)
	add(bpfJeqK, t.auditArch, lNext, lEPERM)
	add(bpfLdWAbs, offNr, lNext, lNext)
	if t.x32Bit != 0 {
		add(bpfJgeK, t.x32Bit, lEPERM, lNext)
	}
	for _, nr := range t.eperm {
		add(bpfJeqK, nr, lEPERM, lNext)
	}
	for _, nr := range t.enosys {
		add(bpfJeqK, nr, lENOSYS, lNext)
	}
	add(bpfJeqK, t.clone, lNext, lAllow)
	add(bpfLdWAbs, offArg0Low, lNext, lNext)
	add(bpfJsetK, cloneNewMask, lEPERM, lAllow)
	ret := map[int]int{lEPERM: len(p), lENOSYS: len(p) + 1, lAllow: len(p) + 2}
	add(bpfRetK, retErrno|errEPERM, lNext, lNext)
	add(bpfRetK, retErrno|errENOSYS, lNext, lNext)
	add(bpfRetK, retAllow, lNext, lNext)

	out := make([]byte, 0, 8*len(p))
	for i, in := range p {
		f := in.sockFilter
		for _, j := range []struct {
			to  int
			dst *uint8
		}{{in.jtTo, &f.jt}, {in.jfTo, &f.jf}} {
			if j.to == lNext {
				continue
			}
			off := ret[j.to] - (i + 1)
			if off < 0 || off > 255 {
				return nil, fmt.Errorf("seccomp: jump out of range at %d", i)
			}
			*j.dst = uint8(off)
		}
		out = binary.LittleEndian.AppendUint16(out, f.code)
		out = append(out, f.jt, f.jf)
		out = binary.LittleEndian.AppendUint32(out, f.k)
	}
	return out, nil
}
