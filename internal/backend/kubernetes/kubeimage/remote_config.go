package kubeimage

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"runtime"
	"strings"

	"github.com/BenjaminBenetti/fleet-man/internal/kubeshim"
)

// RemoteOptions say how to reach a registry from the daemon host.
type RemoteOptions struct {
	// Credentials finds the daemon host's login; nil reads docker's config.
	Credentials Credentials
	// Insecure allows plain HTTP and an untrusted certificate (a loopback
	// registry always gets plain HTTP).
	Insecure bool
	// Transport overrides the HTTP transport (tests).
	Transport http.RoundTripper
}

func (o RemoteOptions) credentials() Credentials {
	if o.Credentials == nil {
		return DockerCredentials{}
	}
	return o.Credentials
}

// client builds a registry client for domain with the daemon host's
// credential for it.
func (o RemoteOptions) client(ctx context.Context, domain string) (*registryClient, error) {
	cred, err := o.credentials().Lookup(ctx, domain)
	if err != nil {
		return nil, err
	}
	return newRegistryClient(domain, o.Insecure, cred, o.Transport), nil
}

// RemoteImageConfig reads an image's config from its registry, for the
// platform the cluster's nodes run ("linux/arm64"; "" takes the daemon host's
// own): what
// `docker image inspect` would say after a pull, without pulling. The
// result's ID is the platform image's config digest and its one RepoDigest is
// <repository>@<digest the reference resolves to> (an index's digest when the
// image is multi-platform), which a pod on any of the index's platforms
// pulls.
//
// Capture-up uses it for an image-only devcontainer whose image the daemon
// host has only for its own architecture: a `docker pull --platform` there
// would move the host's tag to the other architecture, under every
// devcontainer-backend instance that uses it.
func RemoteImageConfig(ctx context.Context, ref, platform string, opts RemoteOptions) (kubeshim.ImageConfig, error) {
	pm, err := readPlatformManifest(ctx, ref, platform, opts)
	if err != nil {
		return kubeshim.ImageConfig{}, err
	}
	c, r, manifest, picked, digest := pm.c, pm.ref, pm.body, pm.picked, pm.digest
	var m struct {
		Config struct {
			Digest string `json:"digest"`
		} `json:"config"`
	}
	if err := json.Unmarshal(manifest, &m); err != nil || !digestPattern.MatchString(m.Config.Digest) {
		return kubeshim.ImageConfig{}, fmt.Errorf("%s: the registry's manifest names no image config", ref)
	}
	blob, err := c.getBlob(ctx, r.Path, m.Config.Digest, maxConfigSize)
	if err != nil {
		return kubeshim.ImageConfig{}, fmt.Errorf("reading the config of %s: %w", ref, err)
	}
	// The OCI image config spells its fields as docker's inspect does.
	var cfg struct {
		Architecture string          `json:"architecture"`
		Os           string          `json:"os"`
		Variant      string          `json:"variant"`
		Config       json.RawMessage `json:"config"`
	}
	if err := json.Unmarshal(blob, &cfg); err != nil {
		return kubeshim.ImageConfig{}, fmt.Errorf("parsing the config of %s: %w", ref, err)
	}
	// A single-platform image is for whatever platform its config says;
	// pods on other nodes could not run it (a pull for the platform fails
	// the same way).
	if platform != "" && !picked && !samePlatform(platform, cfg.Os, cfg.Architecture) {
		return kubeshim.ImageConfig{}, fmt.Errorf("%s is an image for %s/%s only, not %s", ref, cfg.Os, cfg.Architecture, platform)
	}
	img := kubeshim.ImageConfig{ID: m.Config.Digest, Architecture: cfg.Architecture, Os: cfg.Os, Variant: cfg.Variant}
	if len(cfg.Config) > 0 && string(cfg.Config) != "null" {
		if err := json.Unmarshal(cfg.Config, &img.Config); err != nil {
			return kubeshim.ImageConfig{}, fmt.Errorf("parsing the config of %s: %w", ref, err)
		}
	}
	img.RepoDigests = []string{r.Repository() + "@" + digest}
	if r.Tag != "" {
		img.RepoTags = []string{r.Repository() + ":" + r.Tag}
	}
	return img, nil
}

// platformManifest is an image's manifest for a platform, as its registry
// has it.
type platformManifest struct {
	// c is the client that read it, ref the image's reference.
	c   *registryClient
	ref Reference
	// body is the platform's manifest; picked when an index named it.
	body   []byte
	picked bool
	// digest is what ref resolves to (the index's when picked).
	digest string
}

// readPlatformManifest reads ref's manifest from its registry, the one for
// platform (pickPlatform) when ref is an index.
func readPlatformManifest(ctx context.Context, ref, platform string, opts RemoteOptions) (*platformManifest, error) {
	r, err := ParseReference(ref)
	if err != nil {
		return nil, err
	}
	c, err := opts.client(ctx, r.Domain)
	if err != nil {
		return nil, err
	}
	tagOrDigest := r.Digest
	if tagOrDigest == "" {
		tagOrDigest = r.Tag
	}
	if tagOrDigest == "" {
		tagOrDigest = "latest"
	}
	body, mediaType, digest, err := c.getManifest(ctx, r.Path, tagOrDigest)
	if err != nil {
		return nil, fmt.Errorf("reading %s from its registry: %w", ref, err)
	}
	pm := &platformManifest{c: c, ref: r, body: body, digest: digest}
	if isIndex(mediaType) {
		pm.picked = true
		child, err := pickPlatform(body, platform)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", ref, err)
		}
		if pm.body, _, _, err = c.getManifest(ctx, r.Path, child); err != nil {
			return nil, fmt.Errorf("reading %s for %s from its registry: %w", ref, platformOr(platform), err)
		}
	}
	return pm, nil
}

// samePlatform reports whether an image config's os and architecture are
// those of platform ("os/arch[/variant]"); a config that leaves one out
// matches.
func samePlatform(platform, os, arch string) bool {
	want := strings.Split(platform, "/")
	if len(want) < 2 {
		return false
	}
	return (os == "" || os == want[0]) && (arch == "" || arch == want[1])
}

func isIndex(mediaType string) bool {
	return mediaType == mediaOCIIndex || mediaType == mediaDockerList
}

// platformOr is p, or the daemon host's own platform.
func platformOr(p string) string {
	if p == "" {
		return "linux/" + runtime.GOARCH
	}
	return p
}

// pickPlatform returns the digest of an index's manifest for platform
// ("os/arch[/variant]"). arm64 without a variant matches v8 and none.
func pickPlatform(index []byte, platform string) (string, error) {
	want := strings.Split(platformOr(platform), "/")
	if len(want) < 2 {
		return "", fmt.Errorf("invalid platform %q", platform)
	}
	var idx struct {
		Manifests []struct {
			Digest   string `json:"digest"`
			Platform *struct {
				OS           string `json:"os"`
				Architecture string `json:"architecture"`
				Variant      string `json:"variant"`
			} `json:"platform"`
		} `json:"manifests"`
	}
	if err := json.Unmarshal(index, &idx); err != nil {
		return "", fmt.Errorf("unreadable image index: %w", err)
	}
	wantVariant := ""
	if len(want) > 2 {
		wantVariant = want[2]
	}
	var offered []string
	for _, m := range idx.Manifests {
		p := m.Platform
		if p == nil || !digestPattern.MatchString(m.Digest) {
			continue
		}
		offered = append(offered, strings.TrimSuffix(p.OS+"/"+p.Architecture+"/"+p.Variant, "/"))
		if p.OS != want[0] || p.Architecture != want[1] {
			continue
		}
		if wantVariant == "" || p.Variant == wantVariant || (p.Architecture == "arm64" && wantVariant == "v8" && p.Variant == "") {
			return m.Digest, nil
		}
	}
	return "", fmt.Errorf("no image for %s (the registry offers %s)", platformOr(platform), strings.Join(offered, ", "))
}
