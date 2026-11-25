//go:build !linux

package cgroup

import (
	"fmt"
	"runtime"
)

// Manager is unused off Linux.
type Manager struct{}

// Job is unused off Linux.
type Job struct{ Limits Limits }

// Open fails: there are no cgroups off Linux.
func Open() (*Manager, error) {
	return nil, fmt.Errorf("%w: not supported on %s", ErrUnavailable, runtime.GOOS)
}

func (m *Manager) Dir() string           { return "" }
func (m *Manager) Controllers() []string { return nil }
func (m *Manager) Job(string, Limits) (*Job, error) {
	return nil, fmt.Errorf("%w: not supported on %s", ErrUnavailable, runtime.GOOS)
}
func (j *Job) FD() int            { return 0 }
func (j *Job) Dir() string        { return "" }
func (j *Job) OOMKills() int      { return 0 }
func (j *Job) Empty() bool        { return true }
func (j *Job) Close() error       { return nil }
func (j *Job) Release()           {}
func (m *Manager) PidsMax() int64 { return 0 }

// OpenDir fails off Linux.
func OpenDir(string) (*Manager, error) { return Open() }
