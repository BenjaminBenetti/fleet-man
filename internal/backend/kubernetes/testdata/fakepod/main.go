// Command fakepod is the pod binary of the kubernetes package's tests: the
// in-pod commands fleetd runs (pod-agent serve, shepherd, self-version,
// install-bundle; exec-as), as the real fleet does them, but with the pod's
// directories taken from FAKEPOD_ROOT, so a test runs a local process tree
// as its "pod". The tests build it with -tags nok8s and a stamped version,
// as `make fleet-pod` builds the real one.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/BenjaminBenetti/fleet-man/internal/execas"
	"github.com/BenjaminBenetti/fleet-man/internal/podagent"
	"github.com/BenjaminBenetti/fleet-man/internal/podwire"
)

func main() {
	root := os.Getenv("FAKEPOD_ROOT")
	args := os.Args[1:]
	if len(args) == 1 && args[0] == "exec-as" {
		err := execas.RunExecAs(context.Background(), os.Stdin, execas.ExecAsOptions{SpecPath: filepath.Join(root, "exec.json")})
		fmt.Fprintf(os.Stderr, "fakepod exec-as: %v\n", err)
		os.Exit(execas.ExitCode(err))
	}
	if len(args) < 2 || args[0] != "pod-agent" {
		fmt.Fprintf(os.Stderr, "fakepod: unknown command %q\n", args)
		os.Exit(2)
	}
	fs := flag.NewFlagSet(args[1], flag.ExitOnError)
	switch args[1] {
	case "self-version":
		_ = json.NewEncoder(os.Stdout).Encode(podagent.SelfVersion())
	case "serve":
		proto := fs.Int("proto", 0, "")
		role := fs.String("role", string(podwire.RoleMain), "")
		_ = fs.Parse(args[2:])
		opts := podagent.DefaultOptions()
		opts.Role, opts.DaemonProto = podwire.Role(*role), *proto
		opts.SpecPath = filepath.Join(root, "exec.json")
		opts.CacheDir = ""
		opts.ControlDir = filepath.Join(root, "c")
		opts.RunDir = filepath.Join(root, "r")
		opts.InitOOMScore = ""
		conn, err := podagent.TakeStdio()
		if err == nil {
			err = podagent.Serve(context.Background(), conn, opts)
		}
		if err != nil {
			fmt.Fprintf(os.Stderr, "fakepod serve: %v\n", err)
			os.Exit(1)
		}
	case "shepherd":
		if err := podagent.RunShepherd(); err != nil {
			os.Exit(1)
		}
	case "install-bundle":
		var o podagent.BundleOptions
		fs.StringVar(&o.Dir, "dir", "", "")
		fs.StringVar(&o.Name, "name", "", "")
		fs.StringVar(&o.SHA256, "sha256", "", "")
		check := fs.Bool("check", false, "")
		_ = fs.Parse(args[2:])
		if *check {
			if !podagent.BundleInstalled(o) {
				os.Exit(3)
			}
			return
		}
		if err := podagent.InstallBundle(os.Stdin, o); err != nil {
			fmt.Fprintf(os.Stderr, "fakepod install-bundle: %v\n", err)
			os.Exit(1)
		}
	default:
		fmt.Fprintf(os.Stderr, "fakepod: unknown command %q\n", args)
		os.Exit(2)
	}
}
