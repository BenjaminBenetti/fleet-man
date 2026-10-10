package kubeimage

import (
	"context"
	"runtime"
	"slices"
	"strings"
	"testing"
)

func TestRemoteImageConfigPicksThePlatform(t *testing.T) {
	reg := newFakeRegistry(t)
	amd, amdCfg := reg.addImage("devcontainers/base", "", "amd64", map[string]any{
		"User": "vscode", "Env": []string{"PATH=/usr/bin", "LANG=C.UTF-8"}, "Cmd": []string{"bash"},
		"Labels": map[string]string{"devcontainer.metadata": `[{"remoteUser":"vscode"}]`}, "WorkingDir": "/w",
	})
	arm, armCfg := reg.addImage("devcontainers/base", "", "arm64", map[string]any{"User": "vscode-arm"})
	idx := reg.addIndex("devcontainers/base", "debian-12", map[string]string{"linux/amd64": amd, "linux/arm64/v8": arm})
	opts := RemoteOptions{Credentials: staticCredentials{}, Transport: reg.transport()}
	ctx := context.Background()

	img, err := RemoteImageConfig(ctx, "mcr.microsoft.com/devcontainers/base:debian-12", "linux/amd64", opts)
	if err != nil {
		t.Fatal(err)
	}
	if img.ID != amdCfg || img.Architecture != "amd64" || img.Os != "linux" || img.Config.User != "vscode" ||
		!slices.Equal(img.Config.Cmd, []string{"bash"}) || img.Config.WorkingDir != "/w" || img.Config.Labels["devcontainer.metadata"] == "" ||
		!slices.Contains(img.Config.Env, "LANG=C.UTF-8") {
		t.Errorf("amd64 image %+v", img)
	}
	// The digest a pod pulls is the index's, good on any of its platforms.
	if !slices.Equal(img.RepoDigests, []string{"mcr.microsoft.com/devcontainers/base@" + idx}) ||
		!slices.Equal(img.RepoTags, []string{"mcr.microsoft.com/devcontainers/base:debian-12"}) {
		t.Errorf("repo digests %v tags %v", img.RepoDigests, img.RepoTags)
	}

	for _, p := range []string{"linux/arm64", "linux/arm64/v8"} {
		img, err = RemoteImageConfig(ctx, "mcr.microsoft.com/devcontainers/base:debian-12", p, opts)
		if err != nil || img.ID != armCfg || img.Variant != "v8" || img.Config.User != "vscode-arm" {
			t.Errorf("%s: %+v, %v", p, img, err)
		}
	}
	if _, err := RemoteImageConfig(ctx, "mcr.microsoft.com/devcontainers/base:debian-12", "linux/s390x", opts); err == nil ||
		!strings.Contains(err.Error(), "no image for linux/s390x") || !strings.Contains(err.Error(), "linux/arm64/v8") {
		t.Errorf("a platform the index lacks: %v", err)
	}
	// "" is the daemon host's own platform.
	if runtime.GOARCH == "amd64" || runtime.GOARCH == "arm64" {
		img, err := RemoteImageConfig(ctx, "mcr.microsoft.com/devcontainers/base:debian-12", "", opts)
		if err != nil || img.Architecture != runtime.GOARCH {
			t.Errorf("host platform: %+v, %v", img, err)
		}
	}
}

func TestRemoteImageConfigSingleManifestAndDigestRefs(t *testing.T) {
	reg := newFakeRegistry(t)
	d, cfg := reg.addImage("library/alpine", "3.24", "amd64", map[string]any{"Cmd": []string{"/bin/sh"}})
	opts := RemoteOptions{Credentials: staticCredentials{}, Transport: reg.transport()}
	for _, ref := range []string{"alpine:3.24", "alpine@" + d} {
		img, err := RemoteImageConfig(context.Background(), ref, "linux/amd64", opts)
		if err != nil {
			t.Fatalf("%s: %v", ref, err)
		}
		if img.ID != cfg || !slices.Equal(img.RepoDigests, []string{"docker.io/library/alpine@" + d}) {
			t.Errorf("%s: %+v", ref, img)
		}
	}
	// A single-platform image is no image for another platform's nodes.
	if _, err := RemoteImageConfig(context.Background(), "alpine:3.24", "linux/arm64", opts); err == nil || !strings.Contains(err.Error(), "for linux/amd64 only, not linux/arm64") {
		t.Errorf("an amd64-only image for arm64 nodes: %v", err)
	}
	if _, err := RemoteImageConfig(context.Background(), "alpine:nope", "", opts); err == nil || !isNotFound(err) {
		t.Errorf("an unknown tag: %v", err)
	}
	if _, err := RemoteImageConfig(context.Background(), "Not A Ref", "", opts); err == nil {
		t.Error("a bad reference")
	}
}

func TestRemoteImageConfigUsesTheHostLogin(t *testing.T) {
	reg := newFakeRegistry(t)
	reg.auth = authBearer
	reg.addImage("org/private", "v1", "amd64", nil)
	_, err := RemoteImageConfig(context.Background(), "ghcr.io/org/private:v1", "linux/amd64", RemoteOptions{Credentials: staticCredentials{}, Transport: reg.transport()})
	if !isDenied(err) {
		t.Errorf("anonymous: %v", err)
	}
	creds := staticCredentials{"ghcr.io": {Username: "u", Password: "p"}}
	if _, err := RemoteImageConfig(context.Background(), "ghcr.io/org/private:v1", "linux/amd64", RemoteOptions{Credentials: creds, Transport: reg.transport()}); err != nil {
		t.Errorf("with the login: %v", err)
	}
}
