package exec_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

const module = "github.com/more-than-code/defect-drainer-worker"

// The executor reads no DD settings: caps and switches arrive in the
// JobSpec. It may read the host's own environment (HOME, PATH, the test
// account's variable), as a worker does. internal/git still reads three grok
// knobs itself; that is a W2 item, not this package's code.
func TestNoDDSettingsRead(t *testing.T) {
	fset := token.NewFileSet()
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range files {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		src, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		f, err := parser.ParseFile(fset, path, src, parser.ImportsOnly)
		if err != nil {
			t.Fatal(err)
		}
		for _, imp := range f.Imports {
			p, _ := strconv.Unquote(imp.Path.Value)
			if p == module+"/internal/env" {
				t.Errorf("%s imports internal/env", path)
			}
		}
		full, err := parser.ParseFile(fset, path, src, 0)
		if err != nil {
			t.Fatal(err)
		}
		ast.Inspect(full, func(n ast.Node) bool {
			lit, ok := n.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return true
			}
			if strings.Contains(lit.Value, "DEFECT_DRAINER_") || strings.Contains(lit.Value, "DEFECT_CHANNEL_") {
				t.Errorf("%s: DD variable name %s in executor code", fset.Position(lit.Pos()), lit.Value)
			}
			return true
		})
	}
}
