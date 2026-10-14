package kubeimage

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestParseChallenge(t *testing.T) {
	tests := []struct {
		in     string
		scheme string
		params map[string]string
	}{
		{`Bearer realm="https://auth.docker.io/token",service="registry.docker.io",scope="repository:library/ubuntu:pull"`, "bearer",
			map[string]string{"realm": "https://auth.docker.io/token", "service": "registry.docker.io", "scope": "repository:library/ubuntu:pull"}},
		{`Bearer realm="https://ghcr.io/token",scope="repository:a/b:pull,push"`, "bearer",
			map[string]string{"realm": "https://ghcr.io/token", "scope": "repository:a/b:pull,push"}},
		{`Basic realm="Registry Realm"`, "basic", map[string]string{"realm": "Registry Realm"}},
		{`basic realm=fleet, charset="UTF-8"`, "basic", map[string]string{"realm": "fleet", "charset": "UTF-8"}},
		{`Bearer realm="a\"b"`, "bearer", map[string]string{"realm": `a"b`}},
		{``, "", map[string]string{}},
	}
	for _, tt := range tests {
		scheme, params := parseChallenge(tt.in)
		if scheme != tt.scheme || len(params) != len(tt.params) {
			t.Errorf("parseChallenge(%q) = %q %v", tt.in, scheme, params)
			continue
		}
		for k, v := range tt.params {
			if params[k] != v {
				t.Errorf("parseChallenge(%q)[%s] = %q, want %q", tt.in, k, params[k], v)
			}
		}
	}
}

func TestRegistryClientAuth(t *testing.T) {
	ctx := context.Background()
	for _, mode := range []authMode{authNone, authBasic, authBearer} {
		reg := newFakeRegistry(t)
		reg.auth = mode
		d, _ := reg.addImage("team/app", "v1", "amd64", map[string]any{"User": "dev"})

		c := newRegistryClient("registry.test", false, Credential{Username: "u", Password: "p"}, reg.transport())
		got, err := c.headManifest(ctx, "team/app", "v1")
		if err != nil || got != d {
			t.Fatalf("mode %d: headManifest = %q, %v", mode, got, err)
		}
		// The token or Basic login is kept: the next request needs no new
		// challenge.
		before := len(reg.seen())
		if _, err := c.headManifest(ctx, "team/app", d); err != nil {
			t.Fatal(err)
		}
		if n := len(reg.seen()) - before; n != 1 {
			t.Errorf("mode %d: the second request took %d requests: %v", mode, n, reg.seen()[before:])
		}

		if mode == authNone {
			continue
		}
		bad := newRegistryClient("registry.test", false, Credential{Username: "u", Password: "wrong"}, reg.transport())
		if _, err := bad.headManifest(ctx, "team/app", "v1"); err == nil {
			t.Errorf("mode %d: a wrong password pulled", mode)
		}
		anon := newRegistryClient("registry.test", false, Credential{}, reg.transport())
		if _, err := anon.headManifest(ctx, "team/app", "v1"); !isDenied(err) {
			t.Errorf("mode %d: anonymous = %v, want denied", mode, err)
		}
	}
}

func TestRegistryClientBearerScope(t *testing.T) {
	reg := newFakeRegistry(t)
	reg.auth = authBearer
	c := newRegistryClient("registry.test", false, Credential{Username: "u", Password: "p"}, reg.transport())
	err := c.deleteManifest(context.Background(), "fleet/3fa2c91e/devcontainer", zeroDigest)
	if !isNotFound(err) {
		t.Fatalf("delete of an unknown manifest: %v", err)
	}
	if len(reg.scopes) == 0 || reg.scopes[len(reg.scopes)-1] != "repository:fleet/3fa2c91e/devcontainer:delete" {
		t.Errorf("token scopes %v", reg.scopes)
	}
}

func TestRegistryClientIdentityToken(t *testing.T) {
	var form string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/token":
			_ = r.ParseForm()
			form = r.Method + " " + r.PostForm.Encode()
			_, _ = w.Write([]byte(`{"access_token":"tok"}`))
		default:
			if r.Header.Get("Authorization") != "Bearer tok" {
				w.Header().Set("WWW-Authenticate", `Bearer realm="http://`+r.Host+`/token",service="s"`)
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			w.Header().Set("Docker-Content-Digest", zeroDigest)
		}
	}))
	defer srv.Close()
	host := strings.TrimPrefix(srv.URL, "http://")
	c := newRegistryClient(host, false, Credential{IdentityToken: "refresh"}, nil)
	if _, err := c.headManifest(context.Background(), "a/b", "v1"); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(form, "POST ") || !strings.Contains(form, "grant_type=refresh_token") || !strings.Contains(form, "refresh_token=refresh") || !strings.Contains(form, "scope=repository%3Aa%2Fb%3Apull") {
		t.Errorf("token request %q", form)
	}
}

// TestRegistryClientPlainHTTP: a loopback registry on plain HTTP is reached
// after the HTTPS attempt fails, as docker reaches it.
func TestRegistryClientPlainHTTP(t *testing.T) {
	reg := newFakeRegistry(t)
	d, _ := reg.addImage("a", "v1", "amd64", nil)
	c := newRegistryClient(reg.host(), false, Credential{}, nil)
	got, err := c.headManifest(context.Background(), "a", "v1")
	if err != nil || got != d {
		t.Fatalf("headManifest = %q, %v", got, err)
	}
	if c.scheme != "http" {
		t.Errorf("scheme %q", c.scheme)
	}
	if err := c.ping(context.Background()); err != nil {
		t.Errorf("ping: %v", err)
	}

	// A registry that is neither loopback nor set insecure never gets plain
	// HTTP.
	strict := newRegistryClient("registry.invalid", false, Credential{}, nil)
	if strict.insecure {
		t.Error("a remote registry is insecure by default")
	}
}

func TestRegistryClientManifests(t *testing.T) {
	ctx := context.Background()
	reg := newFakeRegistry(t)
	reg.noHeadDigest = true
	d, cfg := reg.addImage("a/b", "v1", "amd64", map[string]any{"User": "dev"})
	c := newRegistryClient("registry.test", false, Credential{}, reg.transport())

	// HEAD without Docker-Content-Digest falls back to a GET.
	if got, err := c.headManifest(ctx, "a/b", "v1"); err != nil || got != d {
		t.Errorf("headManifest = %q, %v", got, err)
	}
	if reg.count("GET /v2/a/b/manifests/v1") != 1 {
		t.Errorf("requests %v", reg.seen())
	}
	// A manifest that is not what its digest says is refused.
	other := sha256Digest([]byte("other"))
	reg.mu.Lock()
	reg.manifests["a/b"][other] = reg.manifests["a/b"]["v1"]
	reg.mu.Unlock()
	if _, _, _, err := c.getManifest(ctx, "a/b", other); err == nil {
		t.Error("a manifest with another digest was accepted")
	}
	// Blobs are checked against their digest and the size limit.
	if b, err := c.getBlob(ctx, "a/b", cfg, maxConfigSize); err != nil || len(b) == 0 {
		t.Errorf("getBlob: %v", err)
	}
	if _, err := c.getBlob(ctx, "a/b", cfg, 3); err == nil {
		t.Error("a blob over the limit was read")
	}
	if _, err := c.headManifest(ctx, "a/b", "nope"); !isNotFound(err) {
		t.Errorf("an unknown tag: %v", err)
	}
	var re *RegistryError
	_, _, _, err := c.getManifest(ctx, "a/b", "nope")
	if !errors.As(err, &re) || !re.has("MANIFEST_UNKNOWN") || !strings.Contains(re.Error(), "HTTP 404") {
		t.Errorf("error %v", err)
	}
}

// TestRegistryClientKeepsTheLoginOffPlainHTTP: a login goes to a plain-HTTP
// token realm only when the registry itself is reached insecurely.
func TestRegistryClientKeepsTheLoginOffPlainHTTP(t *testing.T) {
	var tokenAsked bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/token" {
			tokenAsked = true
			_, _ = w.Write([]byte(`{"token":"t"}`))
			return
		}
		w.Header().Set("WWW-Authenticate", `Bearer realm="http://`+r.Host+`/token"`)
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()
	host := strings.TrimPrefix(srv.URL, "http://")
	rewrite := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		out := r.Clone(r.Context())
		out.URL.Scheme, out.URL.Host = "http", host
		return http.DefaultTransport.RoundTrip(out)
	})
	c := newRegistryClient("registry.test", false, Credential{Username: "u", Password: "p"}, rewrite)
	if _, err := c.headManifest(context.Background(), "a", "v1"); err == nil || !strings.Contains(err.Error(), "plain-HTTP token realm") || tokenAsked {
		t.Errorf("headManifest = %v (token asked: %v)", err, tokenAsked)
	}
	// Anonymous requests and insecure registries may use it.
	anon := newRegistryClient("registry.test", false, Credential{}, rewrite)
	_, _ = anon.headManifest(context.Background(), "a", "v1")
	if !tokenAsked {
		t.Error("an anonymous token was not asked for")
	}
}
