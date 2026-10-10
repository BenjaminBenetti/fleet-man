package kubeimage

import (
	"fmt"
	"regexp"
	"strings"
)

// Reference is a parsed image reference, normalized the way docker and the
// kubelet read it: a reference with no registry is on Docker Hub
// ("docker.io"), and a one-part Docker Hub path is under "library/".
type Reference struct {
	// Domain is the registry ("docker.io", "ghcr.io", "localhost:5000").
	Domain string
	// Path is the repository path within the registry ("library/ubuntu").
	Path string
	// Tag is "" when the reference has none.
	Tag string
	// Digest is "" when the reference has none ("sha256:<hex>").
	Digest string
}

// dockerHub is Docker Hub's reference domain; its API lives elsewhere
// (dockerHubAPI) and its credential under dockerHubAuthKey.
const (
	dockerHub        = "docker.io"
	dockerHubAPI     = "registry-1.docker.io"
	dockerHubAuthKey = "https://index.docker.io/v1/"
)

var (
	// pathComponent is one repository path component (distribution's
	// reference grammar).
	pathComponent = regexp.MustCompile(`^[a-z0-9]+(?:(?:[._]|__|-+)[a-z0-9]+)*$`)
	// domainPattern is a registry host with an optional port.
	domainPattern = regexp.MustCompile(`^(?:[a-zA-Z0-9](?:[a-zA-Z0-9-]*[a-zA-Z0-9])?(?:\.[a-zA-Z0-9](?:[a-zA-Z0-9-]*[a-zA-Z0-9])?)*|\[[0-9a-fA-F:]+\])(?::[0-9]+)?$`)
	tagPattern    = regexp.MustCompile(`^[\w][\w.-]{0,127}$`)
	digestPattern = regexp.MustCompile(`^sha256:[a-f0-9]{64}$`)
)

// ParseReference parses an image reference ("ubuntu:24.04",
// "ghcr.io/org/img@sha256:...") and normalizes its domain.
func ParseReference(s string) (Reference, error) {
	var r Reference
	name := s
	if i := strings.IndexByte(name, '@'); i >= 0 {
		r.Digest = name[i+1:]
		name = name[:i]
		if !digestPattern.MatchString(r.Digest) {
			return Reference{}, fmt.Errorf("image reference %q: invalid digest %q", s, r.Digest)
		}
	}
	// A tag follows the last ':' that comes after the last '/' (a ':'
	// before it is the registry's port).
	if i := strings.LastIndexByte(name, ':'); i > strings.LastIndexByte(name, '/') {
		r.Tag = name[i+1:]
		name = name[:i]
		if !tagPattern.MatchString(r.Tag) {
			return Reference{}, fmt.Errorf("image reference %q: invalid tag %q", s, r.Tag)
		}
	}
	r.Domain, r.Path = splitDomain(name)
	if err := validatePath(r.Path); err != nil {
		return Reference{}, fmt.Errorf("image reference %q: %w", s, err)
	}
	if !domainPattern.MatchString(r.Domain) {
		return Reference{}, fmt.Errorf("image reference %q: invalid registry %q", s, r.Domain)
	}
	return r, nil
}

// splitDomain splits a reference name into its registry and repository
// path as docker does: the first component is a registry only when it holds
// a '.' or ':', is "localhost", or has an uppercase letter.
func splitDomain(name string) (domain, path string) {
	i := strings.IndexByte(name, '/')
	if i < 0 || !looksLikeDomain(name[:i]) {
		domain, path = dockerHub, name
	} else {
		domain, path = name[:i], name[i+1:]
	}
	if domain == "index.docker.io" {
		domain = dockerHub
	}
	if domain == dockerHub && !strings.Contains(path, "/") {
		path = "library/" + path
	}
	return domain, path
}

func looksLikeDomain(s string) bool {
	return strings.ContainsAny(s, ".:") || s == "localhost" || strings.ToLower(s) != s
}

func validatePath(p string) error {
	if p == "" {
		return fmt.Errorf("empty repository path")
	}
	if len(p) > 255 {
		return fmt.Errorf("repository path longer than 255 characters")
	}
	for _, c := range strings.Split(p, "/") {
		if !pathComponent.MatchString(c) {
			return fmt.Errorf("invalid repository path component %q", c)
		}
	}
	return nil
}

// Repository is the reference without tag or digest ("docker.io/library/ubuntu").
func (r Reference) Repository() string { return r.Domain + "/" + r.Path }

// String is the full normalized reference.
func (r Reference) String() string {
	s := r.Repository()
	if r.Tag != "" {
		s += ":" + r.Tag
	}
	if r.Digest != "" {
		s += "@" + r.Digest
	}
	return s
}

// apiHost is the host[:port] the registry's /v2/ API is served on.
func apiHost(domain string) string {
	if domain == dockerHub {
		return dockerHubAPI
	}
	return domain
}

// authKey is the key a registry's credential has in docker's config.json and
// in a kubelet pull Secret.
func authKey(domain string) string {
	if domain == dockerHub {
		return dockerHubAuthKey
	}
	return domain
}

// isLoopback reports whether a registry domain is on this host, which docker
// (and containerd) reach over plain HTTP without being told to.
func isLoopback(domain string) bool {
	host := domain
	if strings.HasPrefix(host, "[") {
		if i := strings.IndexByte(host, ']'); i > 0 {
			host = host[1:i]
		}
	} else if i := strings.LastIndexByte(host, ':'); i >= 0 {
		host = host[:i]
	}
	return host == "localhost" || host == "::1" || strings.HasPrefix(host, "127.")
}

// Registry is an image.registry setting: a registry and an optional path
// prefix ("registry.example.com/team").
type Registry struct {
	Domain string
	// Prefix is the repository path under which fleet's repositories go;
	// "" for the registry's root.
	Prefix string
}

// ParseRegistry parses an image.registry setting. It must start with a
// registry host (a '.' or ':' in it, or "localhost"), so it is never read as a
// Docker Hub repository, and may go on with a repository path prefix.
func ParseRegistry(s string) (Registry, error) {
	s = strings.TrimSuffix(strings.TrimSpace(s), "/")
	if s == "" {
		return Registry{}, fmt.Errorf("no registry set")
	}
	if strings.Contains(s, "://") {
		return Registry{}, fmt.Errorf("registry %q: give the host and path without a scheme (registry.example.com/team)", s)
	}
	domain, prefix, _ := strings.Cut(s, "/")
	if !looksLikeDomain(domain) || !domainPattern.MatchString(domain) {
		return Registry{}, fmt.Errorf("registry %q must start with a registry host (registry.example.com[:port][/path])", s)
	}
	if domain == "index.docker.io" {
		domain = dockerHub
	}
	if prefix != "" {
		if err := validatePath(prefix); err != nil {
			return Registry{}, fmt.Errorf("registry %q: %w", s, err)
		}
	}
	return Registry{Domain: domain, Prefix: prefix}, nil
}

// String is the setting's normalized form.
func (r Registry) String() string {
	if r.Prefix == "" {
		return r.Domain
	}
	return r.Domain + "/" + r.Prefix
}
