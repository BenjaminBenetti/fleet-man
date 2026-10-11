package kubernetes

import (
	"strings"
	"testing"

	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	"github.com/BenjaminBenetti/fleet-man/internal/kubeerr"
)

func TestSelectCloneTier(t *testing.T) {
	const csi = "hostpath.csi.k8s.io"
	for _, tc := range []struct {
		name    string
		f       CloneFacts
		tier    CloneTier
		hold    CloneHold
		snap    string
		refused string
	}{
		{"running, snapshots", CloneFacts{SourceRunning: true, Driver: csi, SnapshotClass: "snap", CSIClone: true}, TierSnapshot, HoldFreeze, "snap", ""},
		{"stopped, CSI clone", CloneFacts{Driver: csi, SnapshotClass: "snap", CSIClone: true}, TierCSIClone, HoldNone, "", ""},
		{"stopped, snapshots only", CloneFacts{Driver: csi, SnapshotClass: "snap"}, TierSnapshot, HoldNone, "snap", ""},
		{"running, CSI clone only", CloneFacts{SourceRunning: true, Driver: csi, CSIClone: true}, TierCopy, HoldFreeze, "", ""},
		{"running, nothing", CloneFacts{SourceRunning: true, Driver: "ebs.csi.aws.com"}, TierCopy, HoldFreeze, "", ""},
		{"not CSI", CloneFacts{SourceRunning: true, SnapshotClass: "snap", CSIClone: true}, TierCopy, HoldFreeze, "", ""},
		{"stopped, not CSI", CloneFacts{}, TierCopy, HoldNone, "", ""},
		{"stop-source copy", CloneFacts{SourceRunning: true, Consistency: fleet.KubeCloneStopSource}, TierCopy, HoldStopSource, "", ""},
		{"stop-source snapshot", CloneFacts{SourceRunning: true, Consistency: fleet.KubeCloneStopSource, Driver: csi, SnapshotClass: "snap"}, TierSnapshot, HoldStopSource, "snap", ""},
		{"copy asked", CloneFacts{Method: fleet.KubeCloneCopy, SourceRunning: true, Driver: csi, SnapshotClass: "snap"}, TierCopy, HoldFreeze, "", ""},
		{"snapshot asked", CloneFacts{Method: fleet.KubeCloneSnapshot, Driver: csi, SnapshotClass: "snap"}, TierSnapshot, HoldNone, "snap", ""},
		{"snapshot asked, none", CloneFacts{Method: fleet.KubeCloneSnapshot, Driver: csi}, "", "", "", "no VolumeSnapshotClass serves"},
		{"snapshot asked, class missing", CloneFacts{Method: fleet.KubeCloneSnapshot, Driver: csi, SnapshotClassSet: "gold"}, "", "", "", `snapshot class "gold"`},
		{"snapshot asked, not CSI", CloneFacts{Method: fleet.KubeCloneSnapshot}, "", "", "", "not a CSI volume"},
		{"csi-clone asked, stopped", CloneFacts{Method: fleet.KubeCloneCSIClone, Driver: csi}, TierCSIClone, HoldNone, "", ""},
		{"csi-clone asked, running", CloneFacts{Method: fleet.KubeCloneCSIClone, Driver: csi, SourceRunning: true}, "", "", "", "stop the instance first"},
		{"csi-clone asked, stop-source", CloneFacts{Method: fleet.KubeCloneCSIClone, Driver: csi, SourceRunning: true, Consistency: fleet.KubeCloneStopSource}, TierCSIClone, HoldStopSource, "", ""},
		{"csi-clone asked, not CSI", CloneFacts{Method: fleet.KubeCloneCSIClone}, "", "", "", "not a CSI volume"},
		{"auto spelled out", CloneFacts{Method: fleet.KubeCloneAuto, Consistency: fleet.KubeCloneConsistencyAuto, SourceRunning: true}, TierCopy, HoldFreeze, "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, err := selectCloneTier(tc.f)
			if tc.refused != "" {
				if err == nil || !strings.Contains(err.Error(), tc.refused) {
					t.Fatalf("plan %+v, err %v; want a refusal with %q", p, err, tc.refused)
				}
				if code, ok := kubeerr.Of(err); !ok || code != kubeerr.Misconfigured {
					t.Fatalf("refusal code %v", code)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if p.Tier != tc.tier || p.Hold != tc.hold || p.SnapshotClass != tc.snap || p.Why == "" {
				t.Fatalf("plan %+v, want tier %s hold %s snapshot class %q", p, tc.tier, tc.hold, tc.snap)
			}
		})
	}
}
