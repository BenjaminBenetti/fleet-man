package kubeexec

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// fleet's Kubernetes packages do no work at program start: every fleet
// command links them (Tab completion included), so clients, schemes and
// codecs are built on first use. Package-level variables may not be
// initialized from a k8s.io package, and there are no init functions.
func TestNoClusterWorkAtInit(t *testing.T) {
	for _, dir := range []string{".", "../k8s/names", "../k8s/owner", "../k8s/preflight", "../server", "../backend/kubernetes", "../backend/kubernetes/binstore"} {
		files, err := filepath.Glob(filepath.Join(dir, "*.go"))
		if err != nil {
			t.Fatal(err)
		}
		for _, path := range files {
			if strings.HasSuffix(path, "_test.go") {
				continue
			}
			checkNoInitWork(t, path)
		}
	}
}

func checkNoInitWork(t *testing.T, path string) {
	t.Helper()
	f, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.SkipObjectResolution)
	if err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	k8sImports := map[string]bool{}
	for _, imp := range f.Imports {
		p, _ := strconv.Unquote(imp.Path.Value)
		if !strings.HasPrefix(p, "k8s.io/") && !strings.HasPrefix(p, "sigs.k8s.io/") {
			continue
		}
		name := filepath.Base(p)
		if imp.Name != nil {
			name = imp.Name.Name
		}
		k8sImports[name] = true
	}
	for _, decl := range f.Decls {
		switch d := decl.(type) {
		case *ast.FuncDecl:
			if d.Recv == nil && d.Name.Name == "init" {
				t.Errorf("%s: func init", path)
			}
		case *ast.GenDecl:
			if d.Tok != token.VAR {
				continue
			}
			for _, spec := range d.Specs {
				for _, value := range spec.(*ast.ValueSpec).Values {
					ast.Inspect(value, func(n ast.Node) bool {
						if sel, ok := n.(*ast.SelectorExpr); ok {
							if x, ok := sel.X.(*ast.Ident); ok && k8sImports[x.Name] {
								t.Errorf("%s: a package-level variable is initialized from %s.%s", path, x.Name, sel.Sel.Name)
							}
						}
						return true
					})
				}
			}
		}
	}
}
