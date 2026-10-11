package kubeimage

import (
	"strings"
	"testing"
)

func TestParseReference(t *testing.T) {
	d := "sha256:" + strings.Repeat("ab", 32)
	tests := []struct {
		in                        string
		domain, path, tag, digest string
		repo                      string
	}{
		{"ubuntu", "docker.io", "library/ubuntu", "", "", "docker.io/library/ubuntu"},
		{"ubuntu:24.04", "docker.io", "library/ubuntu", "24.04", "", "docker.io/library/ubuntu"},
		{"user/app:1", "docker.io", "user/app", "1", "", "docker.io/user/app"},
		{"docker.io/ubuntu", "docker.io", "library/ubuntu", "", "", "docker.io/library/ubuntu"},
		{"index.docker.io/library/ubuntu", "docker.io", "library/ubuntu", "", "", "docker.io/library/ubuntu"},
		{"mcr.microsoft.com/devcontainers/base:debian-12", "mcr.microsoft.com", "devcontainers/base", "debian-12", "", "mcr.microsoft.com/devcontainers/base"},
		{"localhost:5000/fleet/x/devcontainer:abc", "localhost:5000", "fleet/x/devcontainer", "abc", "", "localhost:5000/fleet/x/devcontainer"},
		{"localhost/app", "localhost", "app", "", "", "localhost/app"},
		{"ghcr.io/org/img@" + d, "ghcr.io", "org/img", "", d, "ghcr.io/org/img"},
		{"ghcr.io/org/img:v1@" + d, "ghcr.io", "org/img", "v1", d, "ghcr.io/org/img"},
		{"10.0.0.1:5000/a/b", "10.0.0.1:5000", "a/b", "", "", "10.0.0.1:5000/a/b"},
		{"[::1]:5000/a", "[::1]:5000", "a", "", "", "[::1]:5000/a"},
		{"vsc-app-1234-features", "docker.io", "library/vsc-app-1234-features", "", "", "docker.io/library/vsc-app-1234-features"},
		{"Registry/app", "Registry", "app", "", "", "Registry/app"},
	}
	for _, tt := range tests {
		r, err := ParseReference(tt.in)
		if err != nil {
			t.Errorf("ParseReference(%q): %v", tt.in, err)
			continue
		}
		if r.Domain != tt.domain || r.Path != tt.path || r.Tag != tt.tag || r.Digest != tt.digest || r.Repository() != tt.repo {
			t.Errorf("ParseReference(%q) = %+v (repo %s)", tt.in, r, r.Repository())
		}
	}
	for _, bad := range []string{"", "UPPER", "a//b", "a/b:", "a@sha256:short", "a:-x", "ghcr.io/", "ex ample/a", "a/B"} {
		if r, err := ParseReference(bad); err == nil {
			t.Errorf("ParseReference(%q) = %+v, want an error", bad, r)
		}
	}
}

func TestReferenceString(t *testing.T) {
	r, _ := ParseReference("ubuntu:24.04")
	if s := r.String(); s != "docker.io/library/ubuntu:24.04" {
		t.Errorf("String = %q", s)
	}
	r.Digest = zeroDigest
	r.Tag = ""
	if s := r.String(); s != "docker.io/library/ubuntu@"+zeroDigest {
		t.Errorf("String = %q", s)
	}
}

func TestRegistryHostsAndKeys(t *testing.T) {
	if h := apiHost("docker.io"); h != "registry-1.docker.io" {
		t.Errorf("apiHost(docker.io) = %q", h)
	}
	if h := apiHost("ghcr.io"); h != "ghcr.io" {
		t.Errorf("apiHost(ghcr.io) = %q", h)
	}
	if k := authKey("docker.io"); k != "https://index.docker.io/v1/" {
		t.Errorf("authKey(docker.io) = %q", k)
	}
	if k := authKey("localhost:5000"); k != "localhost:5000" {
		t.Errorf("authKey = %q", k)
	}
	for d, want := range map[string]bool{"localhost": true, "localhost:5000": true, "127.0.0.1:5000": true, "[::1]:5000": true, "ghcr.io": false, "10.0.0.1:5000": false, "localhost.example.com": false} {
		if got := isLoopback(d); got != want {
			t.Errorf("isLoopback(%q) = %v", d, got)
		}
	}
}

func TestParseRegistry(t *testing.T) {
	tests := []struct {
		in, domain, prefix, str string
	}{
		{"registry.example.com", "registry.example.com", "", "registry.example.com"},
		{"registry.example.com/team/", "registry.example.com", "team", "registry.example.com/team"},
		{" localhost:5000 ", "localhost:5000", "", "localhost:5000"},
		{"index.docker.io/me", "docker.io", "me", "docker.io/me"},
		{"123.dkr.ecr.us-east-1.amazonaws.com/a/b", "123.dkr.ecr.us-east-1.amazonaws.com", "a/b", "123.dkr.ecr.us-east-1.amazonaws.com/a/b"},
	}
	for _, tt := range tests {
		r, err := ParseRegistry(tt.in)
		if err != nil {
			t.Errorf("ParseRegistry(%q): %v", tt.in, err)
			continue
		}
		if r.Domain != tt.domain || r.Prefix != tt.prefix || r.String() != tt.str {
			t.Errorf("ParseRegistry(%q) = %+v", tt.in, r)
		}
	}
	for _, bad := range []string{"", "myuser", "myuser/repo", "https://registry.example.com", "registry.example.com/Team", "reg istry.com"} {
		if r, err := ParseRegistry(bad); err == nil {
			t.Errorf("ParseRegistry(%q) = %+v, want an error", bad, r)
		}
	}
}

func TestNames(t *testing.T) {
	reg := Registry{Domain: "registry.example.com", Prefix: "team"}
	id := "sha256:38410210f893325c69dca67faadfa2cf603b2a454b8e0c6dfa2b7f0ae9b7ced7"
	if got := RegistryRepository(reg, testDaemonID); got != "registry.example.com/team/fleet/3fa2c91e/devcontainer" {
		t.Errorf("RegistryRepository = %q", got)
	}
	if got := RegistryTag(Registry{Domain: "localhost:5000"}, testDaemonID, id); got != "localhost:5000/fleet/3fa2c91e/devcontainer:38410210f893" {
		t.Errorf("RegistryTag = %q", got)
	}
	if got := NodeLoadTag(strings.ToUpper(testDaemonID), id); got != "fleet.invalid/3fa2c91e/devcontainer:38410210f893" {
		t.Errorf("NodeLoadTag = %q", got)
	}
	if got := NodeLoadRepository(testDaemonID); got != "fleet.invalid/3fa2c91e/devcontainer" {
		t.Errorf("NodeLoadRepository = %q", got)
	}
	for _, ref := range []string{RegistryTag(reg, testDaemonID, id), NodeLoadTag(testDaemonID, id)} {
		if _, err := ParseReference(ref); err != nil {
			t.Errorf("%s does not parse: %v", ref, err)
		}
	}
	if got := PullSecretName(testDaemonID, "dev-abc123"); got != "fleet-pull-3fa2c91e0b77-dev-abc123" {
		t.Errorf("PullSecretName = %q", got)
	}
	// The longest cluster id (32) still fits a DNS subdomain label.
	if n := len(PullSecretName(testDaemonID, strings.Repeat("a", 32))); n > 63 {
		t.Errorf("PullSecretName is %d characters", n)
	}
}

func TestIsOwnRepository(t *testing.T) {
	tests := map[string]bool{
		"registry.example.com/fleet/3fa2c91e/devcontainer":      true,
		"registry.example.com/team/fleet/3fa2c91e/devcontainer": true,
		"localhost:5000/a/b/fleet/3fa2c91e/devcontainer":        true,
		"registry.example.com/fleet/deadbeef/devcontainer":      false, // another daemon
		"registry.example.com/fleet/3fa2c91e/other":             false,
		"registry.example.com/xfleet/3fa2c91e/devcontainer":     false,
		"registry.example.com/fleet/3fa2c91e/devcontainer/sub":  false,
		"fleet/3fa2c91e/devcontainer":                           false, // no registry: the first part is the registry
		"registry.example.com/team/app":                         false,
	}
	for repo, want := range tests {
		if got := isOwnRepository(repo, testDaemonID); got != want {
			t.Errorf("isOwnRepository(%q) = %v, want %v", repo, got, want)
		}
	}
	if isOwnRepository("r.io/fleet//devcontainer", "") {
		t.Error("an empty daemon id owns a repository")
	}
}
