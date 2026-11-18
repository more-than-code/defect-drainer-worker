package boundary_test

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

const module = "github.com/more-than-code/defect-drainer-worker"

// allowed lists the only modules besides this one and the standard library
// that the worker may depend on, in the build or in tests.
var allowed = []string{
	"github.com/coder/websocket",
	"golang.org/x/sys",
}

// TestDependencyBoundary fails when any package of the module, tests
// included, depends on a module outside the allowlist. It checks each target
// OS the worker ships for, since files like exec/birthtime_linux.go are built
// only there.
func TestDependencyBoundary(t *testing.T) {
	for _, goos := range []string{"linux", "darwin"} {
		cmd := exec.Command("go", "list", "-deps", "-test", "-f", "{{.Standard}} {{.ImportPath}}", "./...")
		cmd.Env = append(os.Environ(), "GOOS="+goos)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("GOOS=%s go list -deps -test: %v\n%s", goos, err, out)
		}
		for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
			std, pkg, _ := strings.Cut(line, " ")
			// Test variants print as "pkg [pkg.test]"; the path is the first field.
			pkg, _, _ = strings.Cut(pkg, " ")
			if std == "true" || allowedPkg(pkg) {
				continue
			}
			t.Errorf("GOOS=%s: %s is outside the allowlist (this module, the standard library, %s)", goos, pkg, strings.Join(allowed, ", "))
		}
	}
}

func allowedPkg(pkg string) bool {
	if !strings.Contains(strings.SplitN(pkg, "/", 2)[0], ".") {
		return true // the standard library and its vendored packages
	}
	// Test binaries and external test packages carry a .test or _test suffix.
	pkg = strings.TrimSuffix(strings.TrimSuffix(pkg, ".test"), "_test")
	for _, m := range append([]string{module}, allowed...) {
		if pkg == m || strings.HasPrefix(pkg, m+"/") {
			return true
		}
	}
	return false
}
