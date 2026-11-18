//go:build !darwin && !linux

package exec

import (
	"os"
	"syscall"
	"time"
)

// FileBirthTime is unavailable on this GOOS. Callers fail open (Nlink/Dev
// checks still apply). Do not invent a portable no-op that looks like a
// successful birthtime read.
func FileBirthTime(*os.File, *syscall.Stat_t) (time.Time, bool) {
	return time.Time{}, false
}
