package inference

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"

	wire "github.com/openabstractions/abstraction-inference/go/abstraction/inference/api"
)

// TestEveryCredentialApplyConsumerIsDeclared checks INF-A2 step 8: every
// consumer string a non-test file passes to cfg.Apply is in the IDL's
// credential_consumers constant.
func TestEveryCredentialApplyConsumerIsDeclared(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	var parsed []*ast.File
	consts := map[string]string{}
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		src, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		f, err := parser.ParseFile(fset, name, src, 0)
		if err != nil {
			t.Fatal(err)
		}
		parsed = append(parsed, f)
		for _, decl := range f.Decls {
			gen, ok := decl.(*ast.GenDecl)
			if !ok || gen.Tok != token.CONST {
				continue
			}
			for _, spec := range gen.Specs {
				vs := spec.(*ast.ValueSpec)
				for i, id := range vs.Names {
					if i >= len(vs.Values) {
						continue
					}
					if lit, ok := vs.Values[i].(*ast.BasicLit); ok && lit.Kind == token.STRING {
						if v, err := strconv.Unquote(lit.Value); err == nil {
							consts[id.Name] = v
						}
					}
				}
			}
		}
	}

	used := map[string]bool{}
	for _, f := range parsed {
		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || sel.Sel.Name != "Apply" || len(call.Args) < 3 {
				return true
			}
			recv, ok := sel.X.(*ast.SelectorExpr)
			if !ok || recv.Sel.Name != "cfg" {
				return true
			}
			switch arg := call.Args[2].(type) {
			case *ast.Ident:
				v, ok := consts[arg.Name]
				if !ok {
					t.Errorf("%s: cfg.Apply consumer %s is not a string constant of this package", fset.Position(call.Pos()), arg.Name)
					return true
				}
				used[v] = true
			case *ast.BasicLit:
				v, _ := strconv.Unquote(arg.Value)
				used[v] = true
			default:
				t.Errorf("%s: cfg.Apply consumer is not a constant", fset.Position(call.Pos()))
			}
			return true
		})
	}
	if len(used) == 0 {
		t.Fatal("found no cfg.Apply call; the scan is broken")
	}

	declared := map[string]bool{}
	for _, c := range wire.CredentialConsumers {
		declared[c] = true
	}
	var names []string
	for c := range used {
		names = append(names, c)
	}
	sort.Strings(names)
	for _, c := range names {
		if declared[c] {
			continue
		}
		t.Errorf("cfg.Apply consumer %q is not in credential_consumers %v", c, wire.CredentialConsumers)
	}
}
