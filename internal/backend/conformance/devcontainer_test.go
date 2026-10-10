package conformance

import (
	"fmt"
	"os/exec"
	"strings"
	"testing"

	"github.com/BenjaminBenetti/fleet-man/internal/backend"
	"github.com/BenjaminBenetti/fleet-man/internal/backend/backendtest"
	"github.com/BenjaminBenetti/fleet-man/internal/backendutil"
	"github.com/BenjaminBenetti/fleet-man/internal/devmeta"
	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	"github.com/BenjaminBenetti/fleet-man/internal/flog"
)

func init() {
	targets[backendtest.BackendDevcontainer] = devcontainerTarget
}

// devcontainerTarget is the devcontainer backend as the daemon binds it, on
// this host's docker.
func devcontainerTarget(t *testing.T, _ backendtest.Selection) backendtest.Target {
	for _, tool := range []string{"docker", "devcontainer"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Fatalf("the devcontainer conformance run needs %s on PATH: %v", tool, err)
		}
	}
	builtBefore := fixtureImages(t)
	t.Cleanup(func() { removeNewFixtureImages(t, builtBefore) })
	return backendtest.Target{
		Name:         backendtest.BackendDevcontainer,
		Type:         fleet.BackendDevcontainer,
		Bind:         func(inst *fleet.Instance) backend.Backend { return backendutil.NewForInstance(inst, false) },
		Capabilities: backendutil.CapabilitiesFor,
		Leftovers:    devcontainerLeftovers,
		EventLog:     flog.Path(),
		Sizes:        backendtest.PlanSizes(),
	}
}

// devcontainerLeftovers lists what docker holds labelled with inst's workspace
// folder: the containers, running or not, the devcontainer CLI made for it,
// and the images committed from them. A clone's image is committed from its
// source and keeps the source's labels, so an image the clone's Down failed
// to remove shows up at the source's teardown.
func devcontainerLeftovers(inst *fleet.Instance) ([]string, error) {
	filter := "label=" + devmeta.LocalFolderLabel + "=" + inst.WorkspaceDir
	var held []string
	for _, list := range []struct {
		kind string
		args []string
	}{
		{"container", []string{"ps", "-a", "-q", "--no-trunc", "--filter", filter}},
		{"image", []string{"image", "ls", "-q", "--no-trunc", "--filter", filter}},
	} {
		out, err := exec.Command("docker", list.args...).Output()
		if err != nil {
			return nil, fmt.Errorf("list %ss: %w", list.kind, err)
		}
		for _, id := range strings.Fields(string(out)) {
			held = append(held, list.kind+" "+id)
		}
	}
	return held, nil
}

// fixtureImages lists the references (repository:tag) of the images the
// devcontainer CLI built for fixture workspaces (vsc-fixture-*). By reference,
// not ID: each workspace's build tags the same cached image again.
func fixtureImages(t *testing.T) map[string]bool {
	t.Helper()
	out, err := exec.Command("docker", "image", "ls", "--format", "{{.Repository}}:{{.Tag}}",
		"--filter", "reference=vsc-"+backendtest.FixtureRepo+"-*").Output()
	if err != nil {
		t.Logf("list fixture images: %v", err)
		return nil
	}
	refs := map[string]bool{}
	for _, ref := range strings.Fields(string(out)) {
		refs[ref] = true
	}
	return refs
}

// removeNewFixtureImages untags the fixture images this run built (Down leaves
// the images `devcontainer up` builds, as it does for every instance), best
// effort; the layers stay in docker's build cache for the next run.
func removeNewFixtureImages(t *testing.T, before map[string]bool) {
	if before == nil {
		return
	}
	for ref := range fixtureImages(t) {
		if before[ref] {
			continue
		}
		if out, err := exec.Command("docker", "image", "rm", ref).CombinedOutput(); err != nil {
			t.Logf("remove fixture image %s: %v %s", ref, err, out)
		}
	}
}
