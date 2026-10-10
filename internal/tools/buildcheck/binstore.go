package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"path/filepath"
	"strings"

	"github.com/BenjaminBenetti/fleet-man/internal/backend/kubernetes/binstore"
)

// runBinStore checks that a daemon build's BinStore serves the pod builds
// beside it: for each arch, the store of the daemon binary -daemon over the
// directory -dir finds fleet-pod-linux-<arch> as the pod build of exactly the
// daemon's version (a release tag, or a dev build's commit), and never the
// daemon's own executable, which it refuses even when pointed straight at it.
func runBinStore(args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("binstore", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	daemon := fs.String("daemon", "", "the full fleet build (fleet-linux-<arch>)")
	dir := fs.String("dir", "", "the directory holding fleet-pod-linux-<arch>")
	arches := fs.String("arch", "amd64,arm64", "pod arches, comma- or space-separated")
	if err := fs.Parse(args); err != nil || *daemon == "" || *dir == "" || fs.NArg() > 0 {
		return errors.New("usage: binstore -daemon FILE -dir DIR [-arch LIST]")
	}
	info, err := binstore.Inspect(*daemon)
	if err != nil {
		return err
	}
	exe, err := filepath.Abs(*daemon)
	if err != nil {
		return err
	}
	var failed []string
	for _, arch := range strings.FieldsFunc(*arches, func(r rune) bool { return r == ',' || r == ' ' }) {
		s := &binstore.Store{Dir: *dir, Self: info.Identity, SelfExe: exe, Getenv: func(string) string { return "" }}
		a, err := s.Binary(context.Background(), arch)
		if err != nil {
			failed = append(failed, err.Error())
			continue
		}
		if a.Path == exe {
			failed = append(failed, fmt.Sprintf("linux/%s: the store served the daemon's own executable", arch))
			continue
		}
		fmt.Fprintf(stdout, "binstore: linux/%s serves %s (%s) for %s\n", arch, a.Path, a.Info.Identity, *daemon)

		// Pointed at the daemon itself, the store refuses it.
		envName := binstore.EnvBinaryPrefix + strings.ToUpper(arch)
		s = &binstore.Store{Dir: *dir, Self: info.Identity, SelfExe: exe, Getenv: func(k string) string {
			if k == envName {
				return exe
			}
			return ""
		}}
		if _, err := s.Binary(context.Background(), arch); !errors.Is(err, binstore.ErrOwnExecutable) {
			failed = append(failed, fmt.Sprintf("linux/%s: pointed at the daemon's own executable the store said %v, want a refusal", arch, err))
		}
	}
	if len(failed) > 0 {
		return errors.New(strings.Join(failed, "\n"))
	}
	return nil
}
