package main

import (
	"bufio"
	"bytes"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"

	"github.com/BenjaminBenetti/fleet-man/internal/buildflavor"
)

// k8sModules are the module path prefixes that must never reach the pod
// build: client-go and the rest of the Kubernetes API machinery.
var k8sModules = []string{"k8s.io/", "sigs.k8s.io/"}

// runNoK8s fails when a Kubernetes module is in the -tags nok8s import graph
// of the package, for each platform the pod build ships on.
func runNoK8s(args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("nok8s", flag.ContinueOnError)
	platforms := fs.String("platforms", "linux/amd64,linux/arm64", "comma-separated GOOS/GOARCH list")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return errors.New("usage: nok8s [-platforms LIST] PKG")
	}
	return checkNoK8s("", fs.Arg(0), strings.Split(*platforms, ","), stdout)
}

// checkNoK8s runs the check from dir ("" for the working directory).
func checkNoK8s(dir, pkg string, platforms []string, stdout io.Writer) error {
	for _, platform := range platforms {
		goos, goarch, ok := strings.Cut(strings.TrimSpace(platform), "/")
		if !ok {
			return fmt.Errorf("bad platform %q, want GOOS/GOARCH", platform)
		}
		out, err := goListDeps(dir, pkg, goos, goarch)
		if err != nil {
			return err
		}
		g, err := parseImportGraph(out)
		if err != nil {
			return err
		}
		if bad := g.firstMatch(k8sModules); bad != "" {
			return fmt.Errorf("%s reaches the %s build (%s/%s):\n  %s\nguard the importing file with //go:build !%s (see internal/buildflavor)",
				bad, buildflavor.Pod, goos, goarch, strings.Join(g.chain(bad), "\n  -> "), buildflavor.PodTag)
		}
		fmt.Fprintf(stdout, "nok8s: %s %s/%s: %d packages, no k8s.io module\n", pkg, goos, goarch, len(g.order))
	}
	return nil
}

// goListDeps lists pkg's -tags nok8s dependency graph, one package per line:
// its import path, then the paths it imports.
func goListDeps(dir, pkg, goos, goarch string) ([]byte, error) {
	cmd := exec.Command("go", "list", "-deps", "-tags", buildflavor.PodTag,
		"-f", "{{.ImportPath}}{{range .Imports}} {{.}}{{end}}", pkg)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GOOS="+goos, "GOARCH="+goarch, "CGO_ENABLED=0")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("go list %s (%s/%s): %w\n%s", pkg, goos, goarch, err, strings.TrimSpace(stderr.String()))
	}
	return out, nil
}

// importGraph is `go list -deps` output: every package in dependency order,
// so the package that was listed comes last.
type importGraph struct {
	order   []string
	imports map[string][]string
}

func parseImportGraph(out []byte) (importGraph, error) {
	g := importGraph{imports: map[string][]string{}}
	sc := bufio.NewScanner(bytes.NewReader(out))
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) == 0 {
			continue
		}
		g.order = append(g.order, fields[0])
		g.imports[fields[0]] = fields[1:]
	}
	if err := sc.Err(); err != nil {
		return g, err
	}
	if len(g.order) == 0 {
		return g, errors.New("go list printed no packages")
	}
	return g, nil
}

func (g importGraph) root() string { return g.order[len(g.order)-1] }

// firstMatch returns the first package, in dependency order, whose import path
// starts with one of the prefixes.
func (g importGraph) firstMatch(prefixes []string) string {
	for _, pkg := range g.order {
		for _, prefix := range prefixes {
			if strings.HasPrefix(pkg, prefix) {
				return pkg
			}
		}
	}
	return ""
}

// chain is a shortest import path from the root to target, root first.
func (g importGraph) chain(target string) []string {
	root := g.root()
	prev := map[string]string{root: ""}
	queue := []string{root}
	for len(queue) > 0 {
		pkg := queue[0]
		queue = queue[1:]
		if pkg == target {
			var path []string
			for p := target; p != ""; p = prev[p] {
				path = append([]string{p}, path...)
			}
			return path
		}
		for _, imp := range g.imports[pkg] {
			if _, seen := prev[imp]; !seen {
				prev[imp] = pkg
				queue = append(queue, imp)
			}
		}
	}
	return []string{root, "...", target}
}
