package kubernetes

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/BenjaminBenetti/fleet-man/internal/inspector"
)

func TestParse(t *testing.T) {
	for _, tc := range []struct {
		name string
		json string
		want Config
	}{
		{"plain image is prebuilt", `{"image": "mcr.microsoft.com/devcontainers/base:ubuntu"}`,
			Config{Prebuilt: "mcr.microsoft.com/devcontainers/base:ubuntu"}},
		{"jsonc", "{\n  // a comment\n  \"image\": \"alpine:3\", /* block */\n  \"runArgs\": [\"--privileged\",],\n}",
			Config{Privileged: "--privileged", Prebuilt: "alpine:3"}},
		{"features get built", `{"image": "debian", "features": {"ghcr.io/devcontainers/features/node:1": {}}}`,
			Config{}},
		{"a Dockerfile is built", `{"build": {"dockerfile": "Dockerfile"}, "image": "ignored"}`, Config{}},
		{"the legacy dockerFile is built", `{"dockerFile": "Dockerfile", "image": "x"}`, Config{}},
		{"compose", `{"dockerComposeFile": "docker-compose.yml", "service": "app"}`, Config{Compose: true}},
		{"compose list", `{"dockerComposeFile": ["a.yml", "b.yml"], "service": "app", "image": "x"}`, Config{Compose: true}},
		{"empty compose list", `{"dockerComposeFile": [], "image": "x"}`, Config{Prebuilt: "x"}},
		{"dind feature", `{"image": "x", "features": {"ghcr.io/devcontainers/features/docker-in-docker:2": {"moby": false}}}`,
			Config{Privileged: "Docker-in-Docker"}},
		{"dind feature by digest and short id", `{"image": "x", "features": {"docker-in-docker@sha256:abc": "latest"}}`,
			Config{Privileged: "Docker-in-Docker"}},
		{"privileged property", `{"image": "x", "privileged": true}`, Config{Privileged: "--privileged", Prebuilt: "x"}},
		{"privileged=true run argument", `{"image": "x", "runArgs": ["--privileged=true"]}`, Config{Privileged: "--privileged", Prebuilt: "x"}},
		{"dood feature", `{"image": "x", "features": {"ghcr.io/devcontainers/features/docker-outside-of-docker:1": {}}}`,
			Config{DooD: "Docker outside of Docker"}},
		{"docker-from-docker feature", `{"image": "x", "features": {"ghcr.io/devcontainers/features/docker-from-docker:1": {}}}`,
			Config{DooD: "Docker outside of Docker"}},
		{"docker.sock mount string", `{"image": "x", "mounts": ["source=/var/run/docker.sock,target=/var/run/docker.sock,type=bind"]}`,
			Config{DooD: "a docker.sock bind", Prebuilt: "x"}},
		{"docker.sock mount object", `{"image": "x", "mounts": [{"source": "/run/user/1000/docker.sock", "target": "/var/run/docker.sock", "type": "bind"}]}`,
			Config{DooD: "a docker.sock bind", Prebuilt: "x"}},
		{"other mounts", `{"image": "x", "mounts": ["source=cache,target=/cache,type=volume", {"source": "/srv", "target": "/srv"}]}`,
			Config{Prebuilt: "x"}},
		{"docker.sock -v", `{"image": "x", "runArgs": ["-v", "/var/run/docker.sock:/var/run/docker.sock"]}`,
			Config{DooD: "a docker.sock bind", Prebuilt: "x"}},
		{"docker.sock --volume=", `{"image": "x", "runArgs": ["--volume=/var/run/docker.sock:/var/run/docker-host.sock:ro"]}`,
			Config{DooD: "a docker.sock bind", Prebuilt: "x"}},
		{"docker.sock --mount", `{"image": "x", "runArgs": ["--mount", "type=bind,src=/var/run/docker.sock,dst=/var/run/docker.sock"]}`,
			Config{DooD: "a docker.sock bind", Prebuilt: "x"}},
		{"another -v", `{"image": "x", "runArgs": ["-v", "/data:/data", "--volumes-from", "docker.sock"]}`, Config{Prebuilt: "x"}},
		{"use-api-socket", `{"image": "x", "runArgs": ["--use-api-socket"]}`, Config{DooD: "--use-api-socket", Prebuilt: "x"}},
		{"nothing to say", `{}`, Config{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Parse([]byte(tc.json))
			if err != nil {
				t.Fatal(err)
			}
			if got != tc.want {
				t.Errorf("Parse = %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestParseRefusesWhatDoesNotRead(t *testing.T) {
	if _, err := Parse([]byte(`{"image": `)); err == nil {
		t.Error("Parse of a truncated file succeeded")
	}
}

func TestRead(t *testing.T) {
	repo := &inspector.Repo{Root: t.TempDir()}
	if _, err := Read(repo); !errors.Is(err, inspector.ErrNoDevcontainerConfig) {
		t.Fatalf("Read without a config = %v, want ErrNoDevcontainerConfig", err)
	}
	dir := filepath.Join(repo.Root, ".devcontainer")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "devcontainer.json"), []byte(`{"dockerComposeFile": "c.yml"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := Read(repo)
	if err != nil || !got.Compose {
		t.Errorf("Read = %+v, %v; want a compose config", got, err)
	}
}

func TestCheck(t *testing.T) {
	open := Cluster{Name: "mk", Known: true, PrivilegedAllowed: true, Version: "v1.33.1"}
	closed := Cluster{Name: "prod", Known: true, Version: "v1.33.1"}
	sysbox := Cluster{Name: "sb", Known: true, Sysbox: true, Version: "v1.33.1"}
	noRoot := Cluster{Name: "tight", Known: true, NoRoot: true, Version: "v1.33.1"}
	old := Cluster{Name: "old", Known: true, PrivilegedAllowed: true, Version: "v1.28.3-gke.1"}
	unknown := Cluster{Name: "new"}
	dind := Config{Privileged: "Docker-in-Docker"}
	dood := Config{DooD: "Docker outside of Docker"}
	prebuilt := Config{Prebuilt: "alpine:3"}
	for _, tc := range []struct {
		name    string
		cfg     Config
		cluster Cluster
		audio   bool
		want    Findings
	}{
		{"compose is refused everywhere", Config{Compose: true, Privileged: "--privileged"}, open, true, Findings{Refusal: ComposeRefusal}},
		{"compose on a cluster not checked yet", Config{Compose: true}, unknown, false, Findings{Refusal: ComposeRefusal}},
		{"dind where privileged pods are allowed", dind, open, false, Findings{}},
		{"dind where they are not", dind, closed, false, Findings{Warnings: []string{
			"prod: Docker-in-Docker needs privileged pods, which are not allowed there (Allow privileged pods, Settings → Kubernetes)"}}},
		{"dind under sysbox", dind, sysbox, false, Findings{}},
		{"dind without root", dind, noRoot, false, Findings{Warnings: []string{
			"tight: Docker-in-Docker needs root, and instances there run without it"}}},
		{"dind on a cluster not checked yet", dind, unknown, false, Findings{}},
		{"dood where privileged pods are allowed", dood, open, false, Findings{}},
		{"dood where they are not", dood, closed, false, Findings{Warnings: []string{
			"prod: Docker outside of Docker gets a docker engine sidecar there, which needs privileged pods (Allow privileged pods, Settings → Kubernetes)"}}},
		{"dood under sysbox", dood, sysbox, false, Findings{}},
		{"dood without root", dood, noRoot, false, Findings{Warnings: []string{
			"tight: Docker outside of Docker gets a docker engine sidecar there, which needs root; instances there run without it"}}},
		{"dood before native sidecars", dood, old, false, Findings{Warnings: []string{
			"old: Docker outside of Docker gets a docker engine sidecar there, which needs Kubernetes 1.29 or later; it runs v1.28.3-gke.1"}}},
		{"dind before native sidecars is fine", dind, old, false, Findings{}},
		{"a prebuilt image without root", prebuilt, noRoot, true, Findings{Warnings: []string{
			"tight: no audio unless alpine:3 has PulseAudio and ALSA: instances there run without root, and fleet adds the audio stack only to images it builds"}}},
		{"a prebuilt image without root, audio off", prebuilt, noRoot, false, Findings{}},
		{"a prebuilt image with root", prebuilt, open, true, Findings{}},
		{"a built image without root", Config{}, noRoot, true, Findings{}},
		{"both at once", Config{Privileged: "--privileged", DooD: "a docker.sock bind", Prebuilt: "x"}, noRoot, true, Findings{Warnings: []string{
			"tight: --privileged needs root, and instances there run without it",
			"tight: a docker.sock bind gets a docker engine sidecar there, which needs root; instances there run without it",
			"tight: no audio unless x has PulseAudio and ALSA: instances there run without root, and fleet adds the audio stack only to images it builds",
		}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := Check(tc.cfg, tc.cluster, tc.audio); !reflect.DeepEqual(got, tc.want) {
				t.Errorf("Check = %#v\nwant    %#v", got, tc.want)
			}
		})
	}
}

func TestAudioWarningNeedsAKnownClusterWithoutRoot(t *testing.T) {
	cfg := Config{Prebuilt: "img"}
	if w := AudioWarning(cfg, Cluster{Name: "c", NoRoot: true}); w != "" {
		t.Errorf("a cluster not checked yet: %q", w)
	}
	if w := AudioWarning(cfg, Cluster{Name: "c", Known: true, NoRoot: true}); !strings.HasPrefix(w, "c: no audio unless img has") {
		t.Errorf("a cluster without root: %q", w)
	}
}

func TestMinorBelow(t *testing.T) {
	for v, want := range map[string]bool{
		"v1.28.3": true, "1.9": true, "v1.29.0": false, "v1.30.1-eks-1": false, "v2.0": false, "": false, "garbage": false,
	} {
		if got := minorBelow(v, 29); got != want {
			t.Errorf("minorBelow(%q, 29) = %v, want %v", v, got, want)
		}
	}
}
