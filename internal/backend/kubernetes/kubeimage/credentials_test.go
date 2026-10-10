package kubeimage

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeDockerConfig writes a docker config.json into a new directory.
func writeDockerConfig(t *testing.T, cfg string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

// installHelper puts a docker-credential-<name> script first on PATH: it
// journals the server it was asked for and answers from a case table.
func installHelper(t *testing.T, name, body string) string {
	t.Helper()
	dir := t.TempDir()
	script := "#!/bin/sh\nread server\necho \"$1 $server\" >> \"$(dirname \"$0\")/asked\"\n" + body
	if err := os.WriteFile(filepath.Join(dir, "docker-credential-"+name), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return dir
}

func TestDockerCredentialsFromAuths(t *testing.T) {
	dir := writeDockerConfig(t, `{"auths":{
		"https://index.docker.io/v1/":{"auth":"`+basicAuth("hubuser", "hubpass")+`"},
		"https://ghcr.io/v1/":{"auth":"`+basicAuth("gh", "ghp_x:y")+`"},
		"localhost:5000":{"username":"lu","password":"lp"},
		"tok.example.com":{"identitytoken":"refresh-me"}
	}}`)
	c := DockerCredentials{ConfigDir: dir}
	ctx := context.Background()
	tests := []struct {
		domain   string
		user     string
		pass     string
		identity string
	}{
		{"docker.io", "hubuser", "hubpass", ""},
		{"ghcr.io", "gh", "ghp_x:y", ""}, // matched by host, a ':' in the password kept
		{"localhost:5000", "lu", "lp", ""},
		{"tok.example.com", "", "", "refresh-me"},
		{"quay.io", "", "", ""},
	}
	for _, tt := range tests {
		got, err := c.Lookup(ctx, tt.domain)
		if err != nil {
			t.Fatalf("Lookup(%s): %v", tt.domain, err)
		}
		if got.Username != tt.user || got.Password != tt.pass || got.IdentityToken != tt.identity {
			t.Errorf("Lookup(%s) = %+v", tt.domain, got)
		}
	}
	if got, _ := c.Lookup(ctx, "quay.io"); !got.Empty() {
		t.Errorf("no login reads as %+v", got)
	}
}

func TestDockerCredentialsMissingOrBroken(t *testing.T) {
	c := DockerCredentials{ConfigDir: t.TempDir()}
	if got, err := c.Lookup(context.Background(), "ghcr.io"); err != nil || !got.Empty() {
		t.Errorf("no config.json: %+v, %v", got, err)
	}
	t.Setenv("DOCKER_CONFIG", writeDockerConfig(t, `{"auths":{"ghcr.io":{"username":"envuser","password":"x"}}}`))
	if got, err := (DockerCredentials{}).Lookup(context.Background(), "ghcr.io"); err != nil || got.Username != "envuser" {
		t.Errorf("DOCKER_CONFIG is not read: %+v, %v", got, err)
	}
	for _, cfg := range []string{`{`, `{"auths":{"ghcr.io":{"auth":"!!"}}}`, `{"auths":{"ghcr.io":{"auth":"` + basicAuth("nocolon", "") + `"}}}`} {
		dir := writeDockerConfig(t, strings.Replace(cfg, basicAuth("nocolon", ""), "bm9jb2xvbg==", 1))
		if _, err := (DockerCredentials{ConfigDir: dir}).Lookup(context.Background(), "ghcr.io"); err == nil {
			t.Errorf("config %s: no error", cfg)
		}
	}
}

func TestDockerCredentialsHelpers(t *testing.T) {
	helperDir := installHelper(t, "fake", `case "$server" in
	gcr.io) echo '{"ServerURL":"gcr.io","Username":"oauth2accesstoken","Secret":"ya29.token"}' ;;
	https://index.docker.io/v1/) echo '{"ServerURL":"https://index.docker.io/v1/","Username":"<token>","Secret":"hub-identity"}' ;;
	broken.io) echo 'boom' >&2; exit 2 ;;
	*) echo 'credentials not found in native keychain'; exit 1 ;;
	esac`)
	ctx := context.Background()

	// credHelpers names the helper for one registry; others use the file.
	dir := writeDockerConfig(t, `{"credHelpers":{"gcr.io":"fake","broken.io":"fake"},"auths":{"ghcr.io":{"auth":"`+basicAuth("gh", "pw")+`"}}}`)
	c := DockerCredentials{ConfigDir: dir}
	got, err := c.Lookup(ctx, "gcr.io")
	if err != nil || got.Username != "oauth2accesstoken" || got.Password != "ya29.token" {
		t.Fatalf("credHelpers: %+v, %v", got, err)
	}
	if got, _ := c.Lookup(ctx, "ghcr.io"); got.Username != "gh" {
		t.Errorf("auths beside credHelpers: %+v", got)
	}
	if _, err := c.Lookup(ctx, "broken.io"); err == nil || !strings.Contains(err.Error(), "boom") {
		t.Errorf("a failing helper: %v", err)
	}

	// credsStore serves everything; "not found" is no login, and an
	// identity token comes back as one.
	dir = writeDockerConfig(t, `{"credsStore":"fake","auths":{"quay.io":{}}}`)
	c = DockerCredentials{ConfigDir: dir}
	if got, err := c.Lookup(ctx, "quay.io"); err != nil || !got.Empty() {
		t.Errorf("not found in the store: %+v, %v", got, err)
	}
	got, err = c.Lookup(ctx, "docker.io")
	if err != nil || got.IdentityToken != "hub-identity" || got.Username != "" {
		t.Errorf("identity token: %+v, %v", got, err)
	}
	asked, _ := os.ReadFile(filepath.Join(helperDir, "asked"))
	if !strings.Contains(string(asked), "get https://index.docker.io/v1/") || !strings.Contains(string(asked), "get gcr.io") {
		t.Errorf("the helper was asked:\n%s", asked)
	}

	// A helper not on PATH says so.
	dir = writeDockerConfig(t, `{"credsStore":"nosuchhelper"}`)
	if _, err := (DockerCredentials{ConfigDir: dir}).Lookup(ctx, "ghcr.io"); err == nil || !strings.Contains(err.Error(), "docker-credential-nosuchhelper") {
		t.Errorf("a missing helper: %v", err)
	}
}

func TestDockerConfigJSONHoldsOneRegistry(t *testing.T) {
	data, err := dockerConfigJSON("ghcr.io", Credential{Username: "gh", Password: "pw"})
	if err != nil {
		t.Fatal(err)
	}
	var cfg struct {
		Auths map[string]struct{ Username, Password, Auth string } `json:"auths"`
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		t.Fatal(err)
	}
	if len(cfg.Auths) != 1 {
		t.Fatalf("auths %v", cfg.Auths)
	}
	e := cfg.Auths["ghcr.io"]
	if e.Username != "gh" || e.Password != "pw" || e.Auth != basicAuth("gh", "pw") {
		t.Errorf("entry %+v", e)
	}
	data, _ = dockerConfigJSON("docker.io", Credential{Username: "u", Password: "p"})
	if !strings.Contains(string(data), `"https://index.docker.io/v1/"`) {
		t.Errorf("Docker Hub key: %s", data)
	}
	if _, err := dockerConfigJSON("ghcr.io", Credential{IdentityToken: "x"}); err == nil {
		t.Error("an identity token made a pull secret")
	}
}
