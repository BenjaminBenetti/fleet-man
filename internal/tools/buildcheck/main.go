// Command buildcheck holds the release build checks the Makefile and CI run:
//
//	buildcheck static FILE...                 linux binaries (and bin/ in .tar.gz bundles) are static
//	buildcheck flavor -want full|pod FILE...  binaries are the build their names say
//	buildcheck nok8s [-platforms LIST] PKG    no k8s.io module in PKG's pod-build import graph
//	buildcheck startup [flags]                cold start and RSS budgets against a base build
//
// Run it with `go run ./internal/tools/buildcheck`; see the check-* and
// startup-budget targets in the Makefile.
package main

import (
	"fmt"
	"io"
	"os"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		usage(stderr)
		return 2
	}
	var err error
	switch args[0] {
	case "static":
		err = runStatic(args[1:], stdout)
	case "flavor":
		err = runFlavor(args[1:], stdout)
	case "nok8s":
		err = runNoK8s(args[1:], stdout)
	case "startup":
		err = runStartup(args[1:], stdout)
	case "-h", "-help", "--help", "help":
		usage(stdout)
		return 0
	default:
		fmt.Fprintf(stderr, "buildcheck: unknown command %q\n", args[0])
		usage(stderr)
		return 2
	}
	if err != nil {
		fmt.Fprintf(stderr, "buildcheck %s: %v\n", args[0], err)
		return 1
	}
	return 0
}

func usage(w io.Writer) {
	fmt.Fprint(w, `usage: buildcheck <command> [args]

  static FILE...                  linux binaries (and bin/ in .tar.gz bundles) are static
  flavor -want full|pod FILE...   binaries are the build their names say
  nok8s [-platforms LIST] PKG     no k8s.io module in PKG's -tags nok8s import graph
  startup [flags]                 cold start and RSS budgets against a base build
`)
}
