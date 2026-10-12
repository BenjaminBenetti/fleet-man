// Package serviceimages names the images of the servers fleet runs on a
// cluster for a fleet (its deb and image caches, its shared Buildkit), each
// pinned by digest, and the image a connection runs for one: its
// service_images override, else the default. The servers' runtime
// (svcruntime/kubert) and the preflight's Buildkit canary both take them from
// here, so admission judges the image the server runs. A leaf: the preflight
// cannot import the runtime.
package serviceimages

import (
	"strings"

	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
)

// The servers' default images, pinned by digest (the connection's
// service_images overrides them; Harden refuses an override that is not
// pinned).
const (
	DebCache         = "docker.io/sameersbn/apt-cacher-ng:latest@sha256:82f55f9c8f627cee8ef5f710c1745388d79d3a3f2f3150353e6500021bec11b4"
	ImageCache       = "docker.io/library/registry:2.8.3@sha256:a3d8aaa63ed8681a604f1dea0aa03f100d5895b6a58ace528858a7b332415373"
	BuildkitRootless = "docker.io/moby/buildkit:v0.25.1-rootless@sha256:0c0ce4929976ace509f7a53878303504b2de8857829aea4932af9f7325a30f51"
	Buildkit         = "docker.io/moby/buildkit:v0.25.1@sha256:79cc6476ab1a3371c9afd8b44e7c55610057c43e18d9b39b68e2b0c2475cc1b6"
)

// For is svc's image on rec: the connection's service_images, else the
// pinned default. Buildkit runs rootless unless rootful. "" for a service
// fleet runs no server of.
func For(rec *fleet.KubeCluster, svc fleet.SharedService, rootful bool) string {
	img := rec.ServiceImages
	pick := func(override, def string) string {
		if v := strings.TrimSpace(override); v != "" {
			return v
		}
		return def
	}
	switch svc {
	case fleet.ServiceDebCache:
		return pick(img.DebCache, DebCache)
	case fleet.ServiceImageCache:
		return pick(img.ImageCache, ImageCache)
	case fleet.ServiceBuildkit:
		if rootful {
			return pick(img.Buildkit, Buildkit)
		}
		return pick(img.BuildkitRootless, BuildkitRootless)
	}
	return ""
}
