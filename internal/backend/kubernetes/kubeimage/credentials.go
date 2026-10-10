package kubeimage

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// Credential is the daemon host's docker login for one registry.
type Credential struct {
	// ServerAddress is the key the credential was found under.
	ServerAddress string
	Username      string
	Password      string
	// IdentityToken is an OAuth2 refresh token, which some registries hand
	// out at `docker login` instead of keeping the password.
	IdentityToken string
}

// Empty reports whether there is no credential (an anonymous registry, or no
// login on the daemon host).
func (c Credential) Empty() bool {
	return c.Username == "" && c.Password == "" && c.IdentityToken == ""
}

// Credentials looks up the daemon host's credential for a registry domain.
type Credentials interface {
	Lookup(ctx context.Context, domain string) (Credential, error)
}

// DockerCredentials reads credentials the way the docker CLI does: from
// $DOCKER_CONFIG/config.json (default ~/.docker/config.json), through the
// registry's credHelpers entry, else the credsStore, else the file's own
// auths. Helpers are docker-credential-<name> programs on PATH.
type DockerCredentials struct {
	// ConfigDir overrides the docker config directory.
	ConfigDir string
}

// helperTimeout bounds one credential helper run (a cloud helper may mint a
// token over the network).
const helperTimeout = 30 * time.Second

type dockerConfigFile struct {
	Auths       map[string]dockerAuthEntry `json:"auths"`
	CredsStore  string                     `json:"credsStore"`
	CredHelpers map[string]string          `json:"credHelpers"`
}

type dockerAuthEntry struct {
	Auth          string `json:"auth"`
	Username      string `json:"username"`
	Password      string `json:"password"`
	IdentityToken string `json:"identitytoken"`
}

func (d DockerCredentials) configPath() (string, error) {
	dir := d.ConfigDir
	if dir == "" {
		dir = os.Getenv("DOCKER_CONFIG")
	}
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		dir = filepath.Join(home, ".docker")
	}
	return filepath.Join(dir, "config.json"), nil
}

// Lookup returns the credential for domain, or an empty one when the daemon
// host has none.
func (d DockerCredentials) Lookup(ctx context.Context, domain string) (Credential, error) {
	path, err := d.configPath()
	if err != nil {
		return Credential{}, err
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return Credential{}, nil
	}
	if err != nil {
		return Credential{}, fmt.Errorf("reading docker config: %w", err)
	}
	var cfg dockerConfigFile
	if err := json.Unmarshal(data, &cfg); err != nil {
		return Credential{}, fmt.Errorf("parsing %s: %w", path, err)
	}
	key := authKey(domain)
	helper := cfg.CredHelpers[key]
	if helper == "" {
		helper = cfg.CredHelpers[domain]
	}
	if helper == "" {
		helper = cfg.CredsStore
	}
	if helper != "" {
		return helperGet(ctx, helper, key)
	}
	entry, found := cfg.Auths[key]
	if !found {
		for k, e := range cfg.Auths {
			if hostnameOf(k) == hostnameOf(key) {
				entry, found = e, true
				break
			}
		}
	}
	if !found {
		return Credential{}, nil
	}
	cred := Credential{ServerAddress: key, Username: entry.Username, Password: entry.Password, IdentityToken: entry.IdentityToken}
	if entry.Auth != "" {
		raw, err := base64.StdEncoding.DecodeString(entry.Auth)
		if err != nil {
			return Credential{}, fmt.Errorf("docker config: the auth of %s is not base64", key)
		}
		user, pass, ok := strings.Cut(string(raw), ":")
		if !ok {
			return Credential{}, fmt.Errorf("docker config: the auth of %s is not user:password", key)
		}
		cred.Username, cred.Password = user, pass
	}
	return cred, nil
}

// hostnameOf reduces a config.json auths key ("https://ghcr.io/v1/",
// "ghcr.io") to its host, as the docker CLI matches them.
func hostnameOf(key string) string {
	s := key
	if strings.Contains(s, "://") {
		if u, err := url.Parse(s); err == nil && u.Host != "" {
			s = u.Host
		}
	}
	host, _, _ := strings.Cut(s, "/")
	if host == "index.docker.io" || host == "registry-1.docker.io" {
		return dockerHub
	}
	return host
}

// helperNotFound is what credential helpers print for a server they hold
// nothing for.
const helperNotFound = "credentials not found"

// helperGet runs `docker-credential-<helper> get` for server.
func helperGet(ctx context.Context, helper, server string) (Credential, error) {
	ctx, cancel := context.WithTimeout(ctx, helperTimeout)
	defer cancel()
	prog := "docker-credential-" + helper
	path, err := exec.LookPath(prog)
	if err != nil {
		return Credential{}, fmt.Errorf("docker config names credential helper %q, but %s is not on the daemon host's PATH", helper, prog)
	}
	cmd := exec.CommandContext(ctx, path, "get")
	cmd.Stdin = strings.NewReader(server)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		out := strings.TrimSpace(stdout.String() + " " + stderr.String())
		if strings.Contains(strings.ToLower(out), helperNotFound) {
			return Credential{}, nil
		}
		return Credential{}, fmt.Errorf("%s get %s: %v: %s", prog, server, err, truncate(out, 300))
	}
	var resp struct {
		ServerURL string
		Username  string
		Secret    string
	}
	if err := json.Unmarshal(stdout.Bytes(), &resp); err != nil {
		return Credential{}, fmt.Errorf("%s get %s: unreadable reply: %w", prog, server, err)
	}
	cred := Credential{ServerAddress: server, Username: resp.Username, Password: resp.Secret}
	if resp.Username == "<token>" {
		cred = Credential{ServerAddress: server, IdentityToken: resp.Secret}
	}
	return cred, nil
}

// dockerConfigJSON is the .dockerconfigjson of a pull Secret holding one
// registry's credential and nothing else.
func dockerConfigJSON(domain string, c Credential) ([]byte, error) {
	if c.IdentityToken != "" || c.Username == "" {
		return nil, fmt.Errorf("the daemon host's login to %s is a token the kubelet cannot use; log in with a username and password (or an access token as the password), or list an existing pull Secret in the cluster's image settings", domain)
	}
	type entry struct {
		Username string `json:"username"`
		Password string `json:"password"`
		Auth     string `json:"auth"`
	}
	cfg := struct {
		Auths map[string]entry `json:"auths"`
	}{Auths: map[string]entry{authKey(domain): {
		Username: c.Username,
		Password: c.Password,
		Auth:     base64.StdEncoding.EncodeToString([]byte(c.Username + ":" + c.Password)),
	}}}
	return json.Marshal(cfg)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
