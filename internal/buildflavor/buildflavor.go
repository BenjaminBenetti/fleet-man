// Package buildflavor says which build of fleet a binary is.
//
// ./cmd/fleet ships in two builds. The full build is the one users install and
// the one that runs fleetd. The pod build (`go build -tags nok8s`, `make
// fleet-pod`) is the binary fleetd stages into Kubernetes pods. It leaves out
// client-go and every package that links it, which keeps that init cost off
// hot in-pod commands.
//
// Code that links client-go lives in files tagged `//go:build !nok8s`, each
// with a `//go:build nok8s` twin that stubs it out. `make check-nok8s` fails
// when a k8s.io module reaches the pod build's import graph.
package buildflavor

import (
	"debug/buildinfo"
	"fmt"
	"runtime/debug"
	"strings"
)

// Flavor names a build of fleet.
type Flavor string

const (
	// Full is the default build: CLI, TUI and fleetd with every backend.
	Full Flavor = "full"
	// Pod is the client-go-free build staged into Kubernetes pods.
	Pod Flavor = "pod"
)

// PodTag is the build tag that selects the pod build.
const PodTag = "nok8s"

// Of reports the flavor that build info describes. Info without a -tags
// setting (a binary built without tags) is the full build.
func Of(info *debug.BuildInfo) Flavor {
	if info == nil {
		return Full
	}
	for _, s := range info.Settings {
		if s.Key != "-tags" {
			continue
		}
		tags := strings.FieldsFunc(s.Value, func(r rune) bool { return r == ',' || r == ' ' })
		for _, tag := range tags {
			if tag == PodTag {
				return Pod
			}
		}
	}
	return Full
}

// OfFile reads the build info embedded in the Go binary at path, which may be
// built for any OS or arch, and reports its flavor.
func OfFile(path string) (Flavor, error) {
	info, err := buildinfo.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read build info of %s: %w", path, err)
	}
	return Of(info), nil
}
