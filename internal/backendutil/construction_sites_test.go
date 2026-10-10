package backendutil

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

const modulePath = "github.com/BenjaminBenetti/fleet-man"

// concreteConstructorAllowed lists the module-relative files outside the
// backend tree that may call a concrete backend package's constructor
// directly. Everything else constructs through NewForInstance (acting on an
// instance) or CapabilitiesFor (asking about one); the by-type constructor is
// unexported, so the compiler already refuses construction by type alone.
var concreteConstructorAllowed = map[string]string{
	"internal/backendutil/backendutil.go": "the one constructor",
	// Create-time builders: a create passes options (template, preset,
	// parameters, repo, branch, machine type) only a create needs.
	"internal/create/build_coder_backend.go":      "coder create options",
	"internal/create/build_codespaces_backend.go": "codespaces create options",
}

// TestBackendsAreConstructedOnlyThroughNewForInstance is the construction-site
// guard. Every site that acts on an instance must build its backend through
// NewForInstance, so a backend whose handle binds more of the instance than its
// type (for Kubernetes: its cluster, namespace and object key) gets that
// binding everywhere: create, the clone destination, rebuild, start/stop, down,
// exec and the pollers. It scans every non-test Go file in the module for a
// direct call of a concrete backend package's New or ForInstance outside the
// backend tree and the allowlist above.
func TestBackendsAreConstructedOnlyThroughNewForInstance(t *testing.T) {
	root := moduleRoot(t)
	fset := token.NewFileSet()
	var offenders []string
	scanned := 0
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			// Skip hidden dirs (.git, .claude worktrees) and vendored trees.
			if path != root && (strings.HasPrefix(d.Name(), ".") || d.Name() == "vendor" || d.Name() == "node_modules") {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		rel := filepath.ToSlash(strings.TrimPrefix(path, root+string(filepath.Separator)))
		if strings.HasPrefix(rel, "internal/backend/") {
			return nil // backend packages construct their own types freely
		}
		scanned++
		if _, ok := concreteConstructorAllowed[rel]; ok {
			return nil
		}
		offenders = append(offenders, concreteConstructorCalls(t, fset, path, rel)...)
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}
	if scanned < 100 {
		t.Fatalf("scanned only %d files under %s; the module walk is broken", scanned, root)
	}
	if len(offenders) > 0 {
		t.Fatalf("backend constructed outside backendutil.NewForInstance:\n  %s\nconstruct through backendutil.NewForInstance (or CapabilitiesFor for a pure capability question)", strings.Join(offenders, "\n  "))
	}
}

// concreteConstructorCalls returns "file:line pkg.Func" for each call in the
// file of New or ForInstance on an imported concrete backend package
// (internal/backend/<name>).
func concreteConstructorCalls(t *testing.T, fset *token.FileSet, path, rel string) []string {
	t.Helper()
	header, err := parser.ParseFile(fset, path, nil, parser.ImportsOnly)
	if err != nil {
		t.Fatalf("parse %s: %v", rel, err)
	}
	// Local name -> import path, for the concrete backend packages imported.
	backends := map[string]string{}
	for _, imp := range header.Imports {
		p, _ := strconv.Unquote(imp.Path.Value)
		name, ok := strings.CutPrefix(p, modulePath+"/internal/backend/")
		if !ok || strings.Contains(name, "/") {
			continue
		}
		local := name
		if imp.Name != nil {
			local = imp.Name.Name
		}
		backends[local] = p
	}
	if len(backends) == 0 {
		return nil
	}
	file, err := parser.ParseFile(fset, path, nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", rel, err)
	}
	var calls []string
	ast.Inspect(file, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		pkg, ok := sel.X.(*ast.Ident)
		if !ok {
			return true
		}
		if _, isBackend := backends[pkg.Name]; isBackend && (sel.Sel.Name == "New" || sel.Sel.Name == "ForInstance") {
			calls = append(calls, rel+":"+strconv.Itoa(fset.Position(call.Pos()).Line)+" "+pkg.Name+"."+sel.Sel.Name)
		}
		return true
	})
	return calls
}

// moduleRoot walks up from the test's working directory to the go.mod.
func moduleRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("go.mod not found above the test directory")
		}
		dir = parent
	}
}

// TestConcreteConstructorCallsFindsDirectConstruction proves the guard's
// detector sees aliased and unaliased imports, so a passing guard means
// something.
func TestConcreteConstructorCallsFindsDirectConstruction(t *testing.T) {
	src := `package x

import (
	dc "github.com/BenjaminBenetti/fleet-man/internal/backend/devcontainer"
	"github.com/BenjaminBenetti/fleet-man/internal/backend/coder"
	"github.com/BenjaminBenetti/fleet-man/internal/backend"
)

var _ backend.Backend = dc.New()

func f() { _ = coder.New(); _ = coder.WithVerbose(true) }
`
	path := filepath.Join(t.TempDir(), "x.go")
	if err := os.WriteFile(path, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	got := concreteConstructorCalls(t, token.NewFileSet(), path, "x.go")
	want := []string{"x.go:9 dc.New", "x.go:11 coder.New"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("detected %v, want %v", got, want)
	}
}
