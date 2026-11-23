//go:build !linux

package git

import "syscall"

// IntoCgroup is a no-op off Linux: there are no cgroups.
func IntoCgroup(*syscall.SysProcAttr, int) {}
