//go:build !linux || !(amd64 || arm64)

package sandbox

import (
	"fmt"
	"runtime"
)

// seccompFilter has no table for this platform: the sandbox is unavailable.
func seccompFilter() ([]byte, error) {
	return nil, fmt.Errorf("seccomp: no filter for %s/%s", runtime.GOOS, runtime.GOARCH)
}
