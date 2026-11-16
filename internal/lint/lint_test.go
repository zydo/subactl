// Package lint holds repository-wide checks that run as tests.
package lint

import (
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

const (
	module    = "github.com/zydo/subactl"
	miniAgent = module + "/examples/mini-agent"
)

// TestImports enforces the dependency rules of the design:
//   - only the standard library, golang.org/x/sys and golang.org/x/term;
//   - mini-agent sees subactl only through its public packages.
func TestImports(t *testing.T) {
	root := filepath.Join("..", "..")
	fset := token.NewFileSet()
	files := 0
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() && strings.HasPrefix(d.Name(), ".") && path != root {
			return filepath.SkipDir
		}
		if d.IsDir() || !strings.HasSuffix(path, ".go") {
			return nil
		}
		files++
		rel, _ := filepath.Rel(root, path)
		inMini := strings.HasPrefix(filepath.ToSlash(rel), "examples/mini-agent/")
		f, err := parser.ParseFile(fset, path, nil, parser.ImportsOnly)
		if err != nil {
			return err
		}
		for _, imp := range f.Imports {
			p, _ := strconv.Unquote(imp.Path.Value)
			first, _, _ := strings.Cut(p, "/")
			switch {
			case !strings.Contains(first, "."): // standard library
			case p == "golang.org/x/sys" || strings.HasPrefix(p, "golang.org/x/sys/"):
			case p == "golang.org/x/term":
			case p == miniAgent || strings.HasPrefix(p, miniAgent+"/"):
				if !inMini {
					t.Errorf("%s imports %s: subactl must not depend on mini-agent", rel, p)
				}
			case p == module || strings.HasPrefix(p, module+"/"):
				if inMini && strings.Contains(p+"/", "/internal/") {
					t.Errorf("%s imports %s: mini-agent may only use subactl's public packages", rel, p)
				}
			default:
				t.Errorf("%s imports %s: only the standard library, golang.org/x/sys and golang.org/x/term are allowed", rel, p)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if files < 10 {
		t.Fatalf("walked only %d Go files; is the repository root wrong?", files)
	}
}
