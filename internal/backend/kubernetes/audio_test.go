//go:build !nok8s

package kubernetes

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/BenjaminBenetti/fleet-man/internal/backend"
	"github.com/BenjaminBenetti/fleet-man/internal/backend/kubernetes/devcli"
	"github.com/BenjaminBenetti/fleet-man/internal/backend/kubernetes/podspec"
	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
)

// Before the build, a create on a cluster without root says in its log that
// a prebuilt image gets no audio layer, when audio is on; it is a note, not
// a warning: whether the image has the stack is known once the pod runs.
func TestNoteAudioOfAPrebuiltImage(t *testing.T) {
	origWanted := audioWanted
	t.Cleanup(func() { audioWanted = origWanted })
	for _, tc := range []struct {
		name    string
		config  string
		nonRoot bool
		wanted  bool
		note    bool
	}{
		{"prebuilt, no root, audio on", `{"image": "alpine:3"}`, true, true, true},
		{"prebuilt, no root, audio off", `{"image": "alpine:3"}`, true, false, false},
		{"prebuilt, root", `{"image": "alpine:3"}`, false, true, false},
		{"features get built", `{"image": "alpine:3", "features": {"ghcr.io/devcontainers/features/node:1": {}}}`, true, true, false},
		{"a Dockerfile", `{"build": {"dockerfile": "Dockerfile"}}`, true, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			audioWanted = func() bool { return tc.wanted }
			var log bytes.Buffer
			ctx := backend.WithJobLog(context.Background(), &log)
			c := &creation{rec: fleet.KubeCluster{ID: "lab-id", DisplayName: "lab"}, identity: podspec.Identity{NonRoot: tc.nonRoot}}
			c.noteAudio(ctx, &devcli.Config{Configuration: []byte(tc.config)})
			got := log.String()
			if want := "lab: no audio unless alpine:3 has PulseAudio and ALSA"; tc.note != strings.Contains(got, want) {
				t.Errorf("log %q, want the note: %v", got, tc.note)
			}
			if len(c.warnings) != 0 {
				t.Errorf("warnings %q: the note is not a warning", c.warnings)
			}
		})
	}
}
