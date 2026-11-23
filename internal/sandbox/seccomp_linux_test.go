//go:build linux && (amd64 || arm64)

package sandbox

import (
	"errors"
	"os"
	"os/exec"
	"syscall"
	"testing"
	"unsafe"

	"golang.org/x/sys/unix"
)

// TestSeccompFilterInKernel loads the filter in a child test process (the
// kernel checks the program) and makes the calls it must deny and allow.
func TestSeccompFilterInKernel(t *testing.T) {
	if os.Getenv("DD_SECCOMP_CHILD") == "1" {
		seccompChild(t)
		return
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestSeccompFilterInKernel$", "-test.v")
	cmd.Env = append(os.Environ(), "DD_SECCOMP_CHILD=1")
	out, err := cmd.CombinedOutput()
	t.Logf("child:\n%s", out)
	if err != nil {
		t.Fatalf("child: %v", err)
	}
}

func seccompChild(t *testing.T) {
	prog, err := seccompFilter()
	if err != nil {
		t.Fatal(err)
	}
	fprog := unix.SockFprog{Len: uint16(len(prog) / 8), Filter: (*unix.SockFilter)(unsafe.Pointer(&prog[0]))}
	if err := unix.Prctl(unix.PR_SET_NO_NEW_PRIVS, 1, 0, 0, 0); err != nil {
		t.Fatalf("no_new_privs: %v", err)
	}
	if _, _, e := unix.Syscall(unix.SYS_SECCOMP, unix.SECCOMP_SET_MODE_FILTER, unix.SECCOMP_FILTER_FLAG_TSYNC, uintptr(unsafe.Pointer(&fprog))); e != 0 {
		t.Fatalf("seccomp: %v", e)
	}
	raw := func(nr uintptr, a ...uintptr) syscall.Errno {
		var args [6]uintptr
		copy(args[:], a)
		_, _, e := unix.Syscall6(nr, args[0], args[1], args[2], args[3], args[4], args[5])
		return e
	}
	for name, c := range map[string]struct {
		nr   uintptr
		args []uintptr
		want syscall.Errno
	}{
		"unshare":         {unix.SYS_UNSHARE, []uintptr{unix.CLONE_NEWNS}, unix.EPERM},
		"setns":           {unix.SYS_SETNS, []uintptr{0, 0}, unix.EPERM},
		"mount":           {unix.SYS_MOUNT, nil, unix.EPERM},
		"keyctl":          {unix.SYS_KEYCTL, nil, unix.EPERM},
		"add_key":         {unix.SYS_ADD_KEY, nil, unix.EPERM},
		"bpf":             {unix.SYS_BPF, nil, unix.EPERM},
		"perf_event_open": {unix.SYS_PERF_EVENT_OPEN, nil, unix.EPERM},
		"userfaultfd":     {unix.SYS_USERFAULTFD, nil, unix.EPERM},
		"open_by_handle":  {unix.SYS_OPEN_BY_HANDLE_AT, nil, unix.EPERM},
		"init_module":     {unix.SYS_INIT_MODULE, nil, unix.EPERM},
		"reboot":          {unix.SYS_REBOOT, nil, unix.EPERM},
		"clone3":          {unix.SYS_CLONE3, nil, unix.ENOSYS},
		"io_uring_setup":  {unix.SYS_IO_URING_SETUP, nil, unix.ENOSYS},
	} {
		if got := raw(c.nr, c.args...); got != c.want {
			t.Errorf("%s: got %v (%d), want %v", name, got, got, c.want)
		}
	}
	if raw(unix.SYS_GETPID) != 0 {
		t.Error("getpid was denied")
	}
	// clone stays allowed, but not with a namespace flag.
	if err := exec.Command("/bin/true").Run(); err != nil {
		t.Errorf("plain fork+exec: %v", err)
	}
	for _, flag := range []uintptr{unix.CLONE_NEWUSER, unix.CLONE_NEWNS, unix.CLONE_NEWNET, unix.CLONE_NEWPID} {
		c := exec.Command("/bin/true")
		c.SysProcAttr = &syscall.SysProcAttr{Cloneflags: flag}
		if err := c.Run(); !errors.Is(err, unix.EPERM) {
			t.Errorf("clone with %#x: %v, want EPERM", flag, err)
		}
	}
}
