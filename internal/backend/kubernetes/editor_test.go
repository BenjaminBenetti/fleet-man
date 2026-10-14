//go:build !nok8s

package kubernetes

import (
	"context"
	"encoding/json"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/BenjaminBenetti/fleet-man/internal/backend/kubernetes/podspec"
	"github.com/BenjaminBenetti/fleet-man/internal/devmeta"
	"github.com/BenjaminBenetti/fleet-man/internal/k8s/names"
	"github.com/BenjaminBenetti/fleet-man/internal/kubeshim"
)

// A cluster instance opens over Remote-SSH: its URI names its alias on this
// daemon and its workspace folder, and the editor customizations come from
// the container the create captured.
func TestEditorURIAndInfo(t *testing.T) {
	label := `[{"customizations":{"vscode":{"extensions":["a.b"]}}}]`
	inspect, err := json.Marshal(kubeshim.Container{Labels: map[string]string{devmeta.MetadataLabel: label}})
	if err != nil {
		t.Fatal(err)
	}
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: names.SpecSecret(handleSTS(), 2), Namespace: handleNS},
		Data:       map[string][]byte{podspec.InspectFile: inspect},
	}
	r := newHandleRig(t, secret)
	b := newHandle(r.inst(), r.pool)
	if _, ok := b.EditorURI("", ""); ok {
		t.Fatal("an editor URI before fleetd has a record of the pod")
	}
	if err := SaveInstanceRecord(InstanceRecord{Cluster: handleCluster, Namespace: handleNS, Key: handleKey, Fleet: "alpha", Instance: "web",
		StatefulSet: handleSTS(), StatefulSetUID: "u", Gen: 2, SpecHash: "sha256:h", WorkspaceFolder: "/workspaces/alpha"}); err != nil {
		t.Fatal(err)
	}
	uri, ok := b.EditorURI("", "")
	if want := "vscode-remote://ssh-remote+web.alpha.fleet/workspaces/alpha"; !ok || uri != want {
		t.Fatalf("EditorURI = %q, %v; want %q", uri, ok, want)
	}
	info, err := b.EditorInfo(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	if info.WorkspaceFolder != "/workspaces/alpha" || info.Metadata != label {
		t.Fatalf("EditorInfo = %+v", info)
	}
}
