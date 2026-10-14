package backend

import (
	"io/fs"
	"testing"
)

// capsOnly answers the pure capability questions and nothing more: what
// backendutil.CapabilitiesFor hands a caller that only asks.
type capsOnly struct{}

func (capsOnly) SupportsSharedMounts() bool { return true }
func (capsOnly) SupportsClone() bool        { return true }
func (capsOnly) SupportsRebuild() bool      { return true }
func (capsOnly) SupportsMicSink() bool      { return true }

// channelCaps also carries the control channel.
type channelCaps struct{ capsOnly }

func (channelCaps) WriteControlFile(string, []byte, fs.FileMode) error { return nil }
func (channelCaps) RemoveControlFile(string) error                     { return nil }

// ownerCaps keeps the workspace when owns is set.
type ownerCaps struct {
	capsOnly
	owns bool
}

func (o ownerCaps) OwnsWorkspace() bool { return o.owns }

func TestHasControlChannel(t *testing.T) {
	if HasControlChannel(capsOnly{}) {
		t.Error("a backend without ControlChannel reported one")
	}
	if !HasControlChannel(channelCaps{}) {
		t.Error("a ControlChannel backend reported none")
	}
}

func TestOwnsWorkspace(t *testing.T) {
	for _, tc := range []struct {
		name string
		caps Capabilities
		want bool
	}{
		{"no WorkspaceOwner", capsOnly{}, false},
		{"WorkspaceOwner that owns it", ownerCaps{owns: true}, true},
		{"WorkspaceOwner that does not", ownerCaps{owns: false}, false},
	} {
		if got := OwnsWorkspace(tc.caps); got != tc.want {
			t.Errorf("%s: OwnsWorkspace = %v, want %v", tc.name, got, tc.want)
		}
	}
}
