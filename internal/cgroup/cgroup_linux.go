package cgroup

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// Manager is the worker's delegated cgroup.
type Manager struct {
	dir       string
	available []string // controllers enabled for the job cgroups
}

// supervisor is the leaf the worker process itself moves into: a cgroup
// that hands controllers to its children may hold no process of its own.
const supervisor = "supervisor"

// jobPrefix starts every job cgroup's name.
const jobPrefix = "job-"

// Open takes over this process's cgroup v2 dir: the worker moves into the
// supervisor leaf, the memory, pids and cpu controllers the dir has are
// enabled for its children, and job cgroups a previous run left are
// removed. It fails, wrapping ErrUnavailable, without a writable
// (delegated) cgroup v2 dir.
//
// Only a systemd service's own cgroup is taken over (INVOCATION_ID set, a
// dir named *.service): a worker started by hand from a shell must not
// move the shell's other processes.
func Open() (*Manager, error) {
	dir, err := ownDir()
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	if os.Getenv("INVOCATION_ID") == "" || !strings.HasSuffix(filepath.Base(dir), ".service") {
		return nil, fmt.Errorf("%w: not running as a systemd service", ErrUnavailable)
	}
	return openAt(dir)
}

// Dir is the delegated dir.
func (m *Manager) Dir() string { return m.dir }

// Controllers are the controllers job cgroups get.
func (m *Manager) Controllers() []string { return append([]string(nil), m.available...) }

// OpenDir is Open on a given cgroup v2 dir, without the systemd checks:
// for tests, with a scratch dir standing in for the unit's.
func OpenDir(dir string) (*Manager, error) { return openAt(dir) }

func openAt(dir string) (*Manager, error) {
	fail := func(format string, a ...any) (*Manager, error) {
		return nil, fmt.Errorf("%w: "+format, append([]any{ErrUnavailable}, a...)...)
	}
	ctl, err := os.ReadFile(filepath.Join(dir, "cgroup.controllers"))
	if err != nil {
		return fail("%s is not a cgroup v2 dir", dir)
	}
	for _, f := range []string{"cgroup.subtree_control", "cgroup.procs"} {
		if unix.Access(filepath.Join(dir, f), unix.W_OK) != nil {
			return fail("%s is not delegated to this user (the unit needs Delegate=)", dir)
		}
	}
	have := strings.Fields(string(ctl))
	m := &Manager{dir: dir}
	for _, c := range []string{"memory", "pids", "cpu"} {
		if slices.Contains(have, c) {
			m.available = append(m.available, c)
		}
	}
	sup := filepath.Join(dir, supervisor)
	if err := os.Mkdir(sup, 0o755); err != nil && !errors.Is(err, os.ErrExist) {
		return fail("%v", err)
	}
	if err := moveAll(dir, sup); err != nil {
		return fail("moving the worker into %s: %v", sup, err)
	}
	m.removeStale()
	if len(m.available) > 0 {
		var b strings.Builder
		for i, c := range m.available {
			if i > 0 {
				b.WriteByte(' ')
			}
			b.WriteString("+" + c)
		}
		if err := os.WriteFile(filepath.Join(dir, "cgroup.subtree_control"), []byte(b.String()), 0); err != nil {
			return fail("enabling %s: %v", b.String(), err)
		}
	}
	if err := m.selfTest(); err != nil {
		return fail("starting a process in a job cgroup: %v", err)
	}
	return m, nil
}

// selfTest starts /bin/true in a throwaway job cgroup the way jobs start
// (CLONE_INTO_CGROUP needs Linux 5.7): a kernel without it would otherwise
// fail every job's agent.
func (m *Manager) selfTest() error {
	j, err := m.Job("selftest", Limits{})
	if err != nil {
		return err
	}
	defer j.Close()
	cmd := exec.Command("/bin/true")
	cmd.SysProcAttr = &syscall.SysProcAttr{UseCgroupFD: true, CgroupFD: j.FD()}
	return cmd.Run()
}

// ownDir is this process's cgroup v2 dir: its "0::" line in
// /proc/self/cgroup under the cgroup2 mount.
func ownDir() (string, error) {
	data, err := os.ReadFile("/proc/self/cgroup")
	if err != nil {
		return "", err
	}
	rel := ""
	for _, l := range strings.Split(string(data), "\n") {
		if p, ok := strings.CutPrefix(l, "0::"); ok {
			rel = p
		}
	}
	if rel == "" || rel == "/" {
		return "", errors.New("no cgroup v2 dir of its own (cgroup v1, or not started by systemd)")
	}
	mnt, err := cgroup2Mount()
	if err != nil {
		return "", err
	}
	// systemd may already have moved us into the supervisor leaf (a
	// restart of Open in the same process): the managed dir is its parent.
	dir := filepath.Join(mnt, filepath.Clean(rel))
	if filepath.Base(dir) == supervisor {
		dir = filepath.Dir(dir)
	}
	return dir, nil
}

// cgroup2Mount is where the cgroup2 file system is mounted.
func cgroup2Mount() (string, error) {
	f, err := os.Open("/proc/self/mountinfo")
	if err != nil {
		return "", err
	}
	defer f.Close()
	s := bufio.NewScanner(f)
	for s.Scan() {
		// … mountpoint … - fstype source options
		pre, post, ok := strings.Cut(s.Text(), " - ")
		fields := strings.Fields(pre)
		if !ok || len(fields) < 5 || !strings.HasPrefix(post, "cgroup2 ") {
			continue
		}
		return fields[4], nil
	}
	return "", errors.New("no cgroup2 mount")
}

// moveAll moves every process of from into to.
func moveAll(from, to string) error {
	for range 10 {
		pids, err := procs(from)
		if err != nil {
			return err
		}
		if len(pids) == 0 {
			return nil
		}
		for _, pid := range pids {
			err := os.WriteFile(filepath.Join(to, "cgroup.procs"), []byte(strconv.Itoa(pid)), 0)
			if err != nil && !errors.Is(err, syscall.ESRCH) {
				return err
			}
		}
	}
	return errors.New("processes keep appearing")
}

func procs(dir string) ([]int, error) {
	data, err := os.ReadFile(filepath.Join(dir, "cgroup.procs"))
	if err != nil {
		return nil, err
	}
	var out []int
	for _, f := range strings.Fields(string(data)) {
		if n, err := strconv.Atoi(f); err == nil {
			out = append(out, n)
		}
	}
	return out, nil
}

// removeStale kills and removes job cgroups left from before.
func (m *Manager) removeStale() {
	ents, _ := os.ReadDir(m.dir)
	for _, e := range ents {
		if e.IsDir() && strings.HasPrefix(e.Name(), jobPrefix) {
			_ = destroy(filepath.Join(m.dir, e.Name()))
		}
	}
}

var unsafeName = regexp.MustCompile(`[^A-Za-z0-9_.-]`)

// Job makes run's cgroup with l and opens it. Every controller l needs must
// be enabled, or the job gets no cgroup and an error.
func (m *Manager) Job(run string, l Limits) (*Job, error) {
	for _, c := range l.controllers() {
		if !slices.Contains(m.available, c) {
			return nil, fmt.Errorf("the %s controller is not delegated", c)
		}
	}
	name := jobPrefix + unsafeName.ReplaceAllString(run, "_")
	if len(name) > 96 {
		name = name[:96]
	}
	dir := filepath.Join(m.dir, name)
	if _, err := os.Stat(dir); err == nil {
		_ = destroy(dir)
	}
	if err := os.Mkdir(dir, 0o755); err != nil {
		return nil, err
	}
	for _, f := range l.files() {
		if err := os.WriteFile(filepath.Join(dir, f[0]), []byte(f[1]), 0); err != nil {
			_ = destroy(dir)
			return nil, fmt.Errorf("%s: %v", f[0], err)
		}
	}
	fd, err := unix.Open(dir, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		_ = destroy(dir)
		return nil, err
	}
	return &Job{dir: dir, fd: fd, Limits: l}, nil
}

// Job is one run's cgroup.
type Job struct {
	dir    string
	fd     int
	Limits Limits
}

// FD is the open dir, for git.IntoCgroup.
func (j *Job) FD() int { return j.fd }

// Dir is the cgroup's dir.
func (j *Job) Dir() string { return j.dir }

// OOMKills is how many of the job's processes the kernel killed for
// reaching its memory limit so far.
func (j *Job) OOMKills() int {
	data, err := os.ReadFile(filepath.Join(j.dir, "memory.events"))
	if err != nil {
		return 0
	}
	for _, l := range strings.Split(string(data), "\n") {
		if v, ok := strings.CutPrefix(l, "oom_kill "); ok {
			n, _ := strconv.Atoi(strings.TrimSpace(v))
			return n
		}
	}
	return 0
}

// Empty reports whether no process is left in the job.
func (j *Job) Empty() bool {
	p, err := procs(j.dir)
	return err != nil || len(p) == 0
}

// Close kills what is left in the job, removes its cgroup and closes the
// dir.
func (j *Job) Close() error {
	err := destroy(j.dir)
	if j.fd > 0 {
		_ = unix.Close(j.fd)
		j.fd = 0
	}
	return err
}

// destroy kills every process in dir (cgroup.kill, Linux 5.14; else
// SIGKILL one by one) and removes it.
func destroy(dir string) error {
	deadline := time.Now().Add(5 * time.Second)
	for {
		if err := os.WriteFile(filepath.Join(dir, "cgroup.kill"), []byte("1"), 0); err != nil {
			if pids, _ := procs(dir); len(pids) > 0 {
				for _, pid := range pids {
					_ = syscall.Kill(pid, syscall.SIGKILL)
				}
			}
		}
		err := os.Remove(dir)
		if err == nil || errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if time.Now().After(deadline) {
			return err
		}
		time.Sleep(50 * time.Millisecond)
	}
}
