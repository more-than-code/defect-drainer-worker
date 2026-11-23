// Package cgroup gives each worker job its own cgroup v2 with memory, task
// and CPU limits (DEF-…-68ru).
//
// The worker's systemd unit delegates its cgroup to the dd-worker user
// (Delegate=memory pids cpu). At start the worker moves itself into a leaf,
// "supervisor", enables the controllers for its children and removes any
// job cgroup a previous run left. Each job then gets "job-<id>" with the
// policy's limits; the job's agent, reviewers and verify sandbox start in it
// (CLONE_INTO_CGROUP, git.IntoCgroup), so everything they fork counts
// against it. When the job's run ends, whatever is left in it is killed and
// the cgroup removed.
//
// The unit's own MemoryMax, TasksMax and CPUWeight still bound the worker as
// a whole. Without delegation (an older unit, cgroup v1, macOS) Open fails
// with the reason, and jobs run with the unit's limits only.
package cgroup

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// ErrUnavailable wraps every reason per-job limits can't be set.
var ErrUnavailable = errors.New("per-job limits unavailable")

// Limits are one job's limits. A zero field is not limited.
type Limits struct {
	// MemoryBytes is memory.max: past it the kernel reclaims, then kills a
	// process of the job (never one outside it).
	MemoryBytes int64
	// Tasks is pids.max: processes and threads.
	Tasks int64
	// CPUs is cpu.max as a number of CPUs (1.5: one and a half).
	CPUs float64
}

// String is l as the job log and `dd-worker status` show it.
func (l Limits) String() string {
	var parts []string
	if l.MemoryBytes > 0 {
		parts = append(parts, "memory "+FormatBytes(l.MemoryBytes))
	}
	if l.CPUs > 0 {
		parts = append(parts, strconv.FormatFloat(l.CPUs, 'f', -1, 64)+" CPUs")
	}
	if l.Tasks > 0 {
		parts = append(parts, strconv.FormatInt(l.Tasks, 10)+" tasks")
	}
	if len(parts) == 0 {
		return "none"
	}
	return strings.Join(parts, ", ")
}

// cpuPeriod is cpu.max's period in microseconds (the kernel's default).
const cpuPeriod = 100000

// files are the control files and values Limits writes, in order.
func (l Limits) files() [][2]string {
	var out [][2]string
	if l.MemoryBytes > 0 {
		out = append(out, [2]string{"memory.max", strconv.FormatInt(l.MemoryBytes, 10)})
	}
	if l.Tasks > 0 {
		out = append(out, [2]string{"pids.max", strconv.FormatInt(l.Tasks, 10)})
	}
	if l.CPUs > 0 {
		quota := int64(l.CPUs * cpuPeriod)
		if quota < 1000 {
			quota = 1000 // the kernel's floor
		}
		out = append(out, [2]string{"cpu.max", fmt.Sprintf("%d %d", quota, cpuPeriod)})
	}
	return out
}

// controllers are the controllers l needs.
func (l Limits) controllers() []string {
	var out []string
	if l.MemoryBytes > 0 {
		out = append(out, "memory")
	}
	if l.Tasks > 0 {
		out = append(out, "pids")
	}
	if l.CPUs > 0 {
		out = append(out, "cpu")
	}
	return out
}

// ParseBytes reads a size: a whole number of bytes, or one with a K, M, G or
// T suffix (powers of 1024; an optional "i" or "iB", as in "2GiB").
func ParseBytes(s string) (int64, error) {
	t := strings.TrimSpace(s)
	t = strings.TrimSuffix(strings.TrimSuffix(t, "B"), "i")
	mult := int64(1)
	if n := len(t); n > 0 {
		switch t[n-1] {
		case 'K', 'k':
			mult = 1 << 10
		case 'M', 'm':
			mult = 1 << 20
		case 'G', 'g':
			mult = 1 << 30
		case 'T', 't':
			mult = 1 << 40
		}
		if mult != 1 {
			t = t[:n-1]
		}
	}
	v, err := strconv.ParseInt(t, 10, 64)
	if err != nil || v <= 0 || v > (1<<62)/mult {
		return 0, fmt.Errorf("not a size: %q", s)
	}
	return v * mult, nil
}

// FormatBytes is n in the largest whole binary unit, else MiB with one
// decimal.
func FormatBytes(n int64) string {
	for _, u := range []struct {
		s string
		b int64
	}{{"TiB", 1 << 40}, {"GiB", 1 << 30}, {"MiB", 1 << 20}, {"KiB", 1 << 10}} {
		if n >= u.b && n%u.b == 0 {
			return strconv.FormatInt(n/u.b, 10) + " " + u.s
		}
	}
	if n >= 1<<20 {
		return strconv.FormatFloat(float64(n)/(1<<20), 'f', 1, 64) + " MiB"
	}
	return strconv.FormatInt(n, 10) + " B"
}
