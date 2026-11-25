package cgroup

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/more-than-code/defect-drainer-worker/git"
)

// scratch is a fresh cgroup v2 dir this test may write, standing in for
// the unit's delegated dir, or a skip.
func scratch(t *testing.T) string {
	mnt, err := cgroup2Mount()
	if err != nil {
		t.Skip(err)
	}
	dir := filepath.Join(mnt, "dd-cgroup-test-"+strings.ReplaceAll(t.Name(), "/", "_"))
	_ = destroy(filepath.Join(dir, supervisor))
	_ = os.Remove(dir)
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Skipf("no writable cgroup2 dir: %v", err)
	}
	t.Cleanup(func() {
		ents, _ := os.ReadDir(dir)
		for _, e := range ents {
			if e.IsDir() {
				_ = destroy(filepath.Join(dir, e.Name()))
			}
		}
		_ = os.Remove(dir)
	})
	return dir
}

func TestOpenAtRefusesNonCgroup(t *testing.T) {
	if _, err := openAt(t.TempDir()); err == nil {
		t.Fatal("a plain dir was taken as a cgroup")
	}
}

// TestJobHoldsTheWholeTree starts a shell in a job cgroup the way the
// worker starts an agent (CLONE_INTO_CGROUP), checks it and a grandchild
// are inside, and that Close kills both and removes the cgroup.
func TestJobHoldsTheWholeTree(t *testing.T) {
	dir := scratch(t)
	// A stale job from "before" is removed at open.
	stale := filepath.Join(dir, jobPrefix+"old")
	if err := os.Mkdir(stale, 0o755); err != nil {
		t.Fatal(err)
	}
	m, err := openAt(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Fatalf("stale job cgroup kept: %v", err)
	}
	j, err := m.Job("bjob_1/2", Limits{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(filepath.Base(j.Dir()), "job-bjob_1_2-") {
		t.Fatalf("name %s", j.Dir())
	}
	cmd := exec.Command("/bin/sh", "-c", "sleep 300 & echo $!; wait")
	cmd.SysProcAttr = &syscall.SysProcAttr{}
	git.IntoCgroup(cmd.SysProcAttr, j.FD())
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 32)
	n, _ := out.Read(buf)
	grandchild := strings.TrimSpace(string(buf[:n]))
	for _, pid := range []string{strconv.Itoa(cmd.Process.Pid), grandchild} {
		data, err := os.ReadFile("/proc/" + pid + "/cgroup")
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(data), "/"+filepath.Base(j.Dir())+"\n") {
			t.Fatalf("pid %s is in %s", pid, data)
		}
	}
	if j.Empty() {
		t.Fatal("Empty with two processes in it")
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	if err := j.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the shell outlived Close")
	}
	if _, err := os.Stat(j.Dir()); !os.IsNotExist(err) {
		t.Fatalf("cgroup kept: %v", err)
	}
	// The fd stays open after Close: a late spawn with it fails on the
	// removed cgroup rather than landing anywhere else.
	late := exec.Command("/bin/true")
	late.SysProcAttr = &syscall.SysProcAttr{}
	git.IntoCgroup(late.SysProcAttr, j.FD())
	if err := late.Run(); err == nil {
		t.Fatal("a spawn into the removed cgroup started")
	}
	j.Release()
	if _, err := os.Stat("/proc/" + grandchild); err == nil {
		if data, _ := os.ReadFile("/proc/" + grandchild + "/stat"); !strings.Contains(string(data), ") Z ") {
			t.Fatalf("grandchild %s survived", grandchild)
		}
	}
}

func TestJobNeedsItsControllers(t *testing.T) {
	m := &Manager{dir: t.TempDir(), available: []string{"pids"}}
	if _, err := m.Job("x", Limits{MemoryBytes: 1 << 30}); err == nil || !strings.Contains(err.Error(), "memory controller") {
		t.Fatalf("%v", err)
	}
}

func TestJobNamesDoNotCollide(t *testing.T) {
	long := strings.Repeat("a", 120)
	a, b := jobName(long+"-1"), jobName(long+"-2")
	if a == b || len(a) > 80 || !strings.HasPrefix(a, jobPrefix) {
		t.Fatalf("%q %q", a, b)
	}
	if got := jobName("bjob/1"); !strings.HasPrefix(got, "job-bjob_1-") {
		t.Fatalf("%q", got)
	}
}

func TestJobRefusesALiveDir(t *testing.T) {
	dir := scratch(t)
	m, err := openAt(dir)
	if err != nil {
		t.Fatal(err)
	}
	j, err := m.Job("same", Limits{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = j.Close(); j.Release() }()
	if _, err := m.Job("same", Limits{}); err == nil {
		t.Fatal("a second job took a live job's cgroup")
	}
	if _, err := os.Stat(j.Dir()); err != nil {
		t.Fatalf("the live job's cgroup was removed: %v", err)
	}
}
