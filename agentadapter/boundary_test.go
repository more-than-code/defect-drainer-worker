package agentadapter_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strings"
	"testing"
)

// No adapter code reads the process environment: the caller builds the child
// env and hands it over (Task.Env). Tests may, so _test.go files are skipped.
func TestNoEnvironmentReads(t *testing.T) {
	banned := map[string]bool{
		"os.Environ": true, "os.Getenv": true, "os.LookupEnv": true, "os.ExpandEnv": true,
		"syscall.Environ": true, "syscall.Getenv": true,
	}
	fset := token.NewFileSet()
	err := filepath.WalkDir(".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() && d.Name() == "testdata" {
			return filepath.SkipDir
		}
		if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		f, perr := parser.ParseFile(fset, path, nil, 0)
		if perr != nil {
			return perr
		}
		ast.Inspect(f, func(n ast.Node) bool {
			sel, ok := n.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			if id, ok := sel.X.(*ast.Ident); ok && banned[id.Name+"."+sel.Sel.Name] {
				t.Errorf("%s: %s.%s reads the process environment", fset.Position(sel.Pos()), id.Name, sel.Sel.Name)
			}
			return true
		})
		ast.Inspect(f, func(n ast.Node) bool {
			lit, ok := n.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return true
			}
			// The hygiene filter names the prefix to drop it; nothing else
			// may spell a DD variable.
			if strings.Contains(lit.Value, "DEFECT_DRAINER_") && path != "env.go" {
				t.Errorf("%s: DD variable name %s in adapter code", fset.Position(lit.Pos()), lit.Value)
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
