package devcli

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/BenjaminBenetti/fleet-man/internal/kubeshim"
)

func TestLayerDockerfile(t *testing.T) {
	base := kubeshim.ImageConfig{}
	base.Config.User = "vscode"
	got, err := LayerDockerfile(LayerRequest{From: "vsc-app-1234:latest", Base: base, Files: map[string]string{"b.sh": "", "a.sh": ""}, Run: "sh a.sh"})
	if err != nil {
		t.Fatal(err)
	}
	want := "FROM vsc-app-1234:latest\nUSER root\nCOPY a.sh " + LayerDir + "/a.sh\nCOPY b.sh " + LayerDir + "/b.sh\n" +
		"RUN cd " + LayerDir + " && sh a.sh && cd / && rm -rf " + LayerDir + "\nUSER vscode\n"
	if got != want {
		t.Fatalf("Dockerfile:\n%s\nwant:\n%s", got, want)
	}
	// An image whose user is root (unset) keeps it: no USER line after.
	if got, _ := LayerDockerfile(LayerRequest{From: "img", Run: "true"}); strings.Count(got, "USER") != 1 {
		t.Fatalf("a root image got its user changed:\n%s", got)
	}
	for name, req := range map[string]LayerRequest{
		"no base":         {Run: "true"},
		"spaced base":     {From: "a b", Run: "true"},
		"two lines":       {From: "img", Run: "true\nfalse"},
		"no command":      {From: "img"},
		"a path for name": {From: "img", Run: "true", Files: map[string]string{"../x": ""}},
	} {
		if _, err := LayerDockerfile(req); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

// The layer is built with the daemon host's docker on capture-up's pinned
// builder and platform, from the context holding its files, and the new
// image is read back by the id the build reported.
func TestAddLayer(t *testing.T) {
	f := newFakeTools(t)
	if err := os.WriteFile(filepath.Join(f.dir, "layered.json"), []byte(`[{"Id":"sha256:layered","Config":{"User":"vscode"}}]`), 0o644); err != nil {
		t.Fatal(err)
	}
	base := kubeshim.ImageConfig{ID: "sha256:built"}
	base.Config.User = "vscode"
	img, err := AddLayer(context.Background(), LayerRequest{
		Tools: f.Tools, From: "vsc-app-1234:latest", Base: base, Platform: "linux/arm64",
		Files: map[string]string{"fleet-audio.sh": "echo install"}, Run: "sh fleet-audio.sh",
	})
	if err != nil {
		t.Fatal(err)
	}
	if img.ID != "sha256:layered" {
		t.Fatalf("image %q, want the layered one", img.ID)
	}
	if df := f.read(t, "layer.Dockerfile"); !strings.HasPrefix(df, "FROM vsc-app-1234:latest\nUSER root\n") || !strings.HasSuffix(df, "USER vscode\n") {
		t.Fatalf("Dockerfile:\n%s", df)
	}
	if got := f.read(t, "layer.file.fleet-audio.sh"); got != "echo install" {
		t.Fatalf("the layer's file holds %q", got)
	}
	env := f.read(t, "layer.env")
	if !strings.Contains(env, "BUILDX_BUILDER=desktop-linux") || !strings.Contains(env, "DOCKER_DEFAULT_PLATFORM=linux/arm64") {
		t.Fatalf("the build was not pinned like capture-up's:\n%s", env)
	}

	t.Setenv("FAKE_BUILD_FAIL", "1")
	_, err = AddLayer(context.Background(), LayerRequest{Tools: f.Tools, From: "vsc-app-1234:latest", Run: "true"})
	if err == nil || !strings.Contains(err.Error(), "Unable to locate package pulseaudio") {
		t.Fatalf("a failed build: %v, want its last lines", err)
	}
}
