//go:build !nok8s

package kubernetes

import (
	"context"
	"errors"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	kruntime "k8s.io/apimachinery/pkg/runtime"

	"github.com/BenjaminBenetti/fleet-man/internal/backend"
	"github.com/BenjaminBenetti/fleet-man/internal/backend/kubernetes/podspec"
	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	"github.com/BenjaminBenetti/fleet-man/internal/hostrelay"
	"github.com/BenjaminBenetti/fleet-man/internal/k8s/names"
	"github.com/BenjaminBenetti/fleet-man/internal/kubeerr"
	"github.com/BenjaminBenetti/fleet-man/internal/podshare"
)

// cloneSourceObjects are a stopped source instance as fleet made it: its
// StatefulSet (applied by fleet, rendered with spec hash hash), disk and
// Secrets, and fleet's record of it.
func cloneSourceObjects(t *testing.T, edit func(*appsv1.StatefulSet)) (*appsv1.StatefulSet, InstanceRecord) {
	t.Helper()
	zero := int32(0)
	sts := &appsv1.StatefulSet{ObjectMeta: handleMeta(handleSTS()), Spec: appsv1.StatefulSetSpec{
		Replicas: &zero,
		Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: DevContainer, Image: "img@sha256:aa"}}}},
	}}
	sts.UID = "u-src"
	sts.ManagedFields = []metav1.ManagedFieldsEntry{{Manager: DefaultFieldManager}}
	sts.Annotations = map[string]string{names.AnnotationRebuildGen: "2"}
	rec := InstanceRecord{
		Cluster: handleCluster, Namespace: handleNS, Key: handleKey, Fleet: "alpha", Instance: "web", StatefulSet: handleSTS(),
		StatefulSetUID: "u-src", Gen: 2, SpecHash: podspec.SpecHash(&sts.Spec.Template.Spec), Image: "img@sha256:aa", ImageDigests: []string{"sha256:aa"},
	}
	if edit != nil {
		edit(sts)
	}
	return sts, rec
}

// cloneRig is a clone of the rig's web instance in the making, its
// connection and identity filled in as connect does.
func cloneRig(t *testing.T, sts *appsv1.StatefulSet, rec InstanceRecord) *cloning {
	t.Helper()
	objs := []kruntime.Object{sts,
		&corev1.PersistentVolumeClaim{ObjectMeta: handleMeta(handleSTS())},
		&corev1.Secret{ObjectMeta: handleMeta(names.SpecSecret(handleSTS(), 2))},
		&corev1.Secret{ObjectMeta: handleMeta(names.EnvSecret(handleSTS(), 2))},
	}
	r := newHandleRig(t, objs...)
	if err := SaveInstanceRecord(rec); err != nil {
		t.Fatal(err)
	}
	dest := bound{cluster: handleCluster, key: "dest2345", fleet: "alpha", name: "copy", sts: names.Instance("alpha", "copy", "dest2345")}
	c := &cloning{creation: &creation{p: r.pool, id: dest, cs: r.cs, ns: handleNS, owner: handleOwner, verb: "clone", start: time.Now()},
		srcID: ContainerID(handleCluster, handleNS, handleSTS(), 2)}
	return c
}

func TestCloneLoadsAVerifiedSource(t *testing.T) {
	sts, rec := cloneSourceObjects(t, nil)
	c := cloneRig(t, sts, rec)
	if err := c.loadSource(context.Background()); err != nil {
		t.Fatal(err)
	}
	if c.running || c.srcRec.Gen != 2 || c.srcDisk.Name != handleSTS() || c.srcSpec.Name != names.SpecSecret(handleSTS(), 2) {
		t.Fatalf("source %+v running %v", c.srcRec, c.running)
	}
}

// The clone's record is the source's under the clone's name: every field
// is filled (a field the record gains must be carried or set here, or a
// clone loses it, as it once lost the published ports), the lists are the
// clone's own.
func TestCloneRecordCarriesTheSources(t *testing.T) {
	sts, rec := cloneSourceObjects(t, nil)
	rec.Strategy, rec.RemoteUser, rec.WorkspaceFolder, rec.NonRoot = "push", "vscode", "/workspaces/web", true
	rec.AudioMissing = true
	rec.Created = time.Unix(1700000000, 0)
	rec.Ports = []backend.PublishedPort{{HostIP: "127.0.0.1", HostPort: 3000, ContainerPort: 3000}}
	rec.HostSockets = []hostrelay.Socket{{Target: "/run/gpg.sock", Source: "/home/u/.gnupg/S.gpg-agent.extra", Mode: 0o700}}
	rec.HostSocketDirs = []hostrelay.SocketDir{{Target: "/tmp/.X11-unix", Source: "/tmp/.X11-unix", Shadowed: []string{"X1"}}}
	rec.HostGateway, rec.GatewaySkip = true, []uint16{3000}
	rec.Share = &ShareRecord{Mode: "single-node", Claim: "fs-alpha-abcdefgh-share", Roots: []podshare.SpokeRoot{{Name: ".mnt", Reserved: []string{"data"}}}, UID: 1000, GID: 1000}
	c := cloneRig(t, sts, rec)
	c.srcRec, c.gen = rec, rec.Gen
	got := c.cloneRecord(&podspec.Objects{StatefulSet: sts})
	if got.Share == rec.Share || !reflect.DeepEqual(got.Share, rec.Share) || &got.Share.Roots[0].Reserved[0] == &rec.Share.Roots[0].Reserved[0] {
		t.Errorf("the clone's share record is %+v, want a copy of the source's", got.Share)
	}
	v := reflect.ValueOf(got)
	for i := 0; i < v.NumField(); i++ {
		switch name := v.Type().Field(i).Name; name {
		case "V", "StatefulSetUID": // SaveInstanceRecord's, and the applied StatefulSet's
		default:
			if v.Field(i).IsZero() {
				t.Errorf("the clone's record has no %s", name)
			}
		}
	}
	if got.Key != "dest2345" || got.StatefulSet != c.id.sts || got.Instance != "copy" || !slices.Equal(got.Ports, rec.Ports) {
		t.Fatalf("clone record %+v", got)
	}
	if !reflect.DeepEqual(got.HostSocketDirs, rec.HostSocketDirs) || &got.HostSocketDirs[0].Shadowed[0] == &rec.HostSocketDirs[0].Shadowed[0] {
		t.Errorf("the clone's socket directories %+v, want a copy of the source's", got.HostSocketDirs)
	}
	if !got.NonRoot || !got.AudioMissing {
		t.Fatalf("clone of an audio-less pod without root: nonRoot %v audioMissing %v", got.NonRoot, got.AudioMissing)
	}
	if !slices.Equal(got.HostSockets, rec.HostSockets) || !got.HostGateway || !slices.Equal(got.GatewaySkip, rec.GatewaySkip) {
		t.Fatalf("clone host relays %+v %v %v", got.HostSockets, got.HostGateway, got.GatewaySkip)
	}
	got.Ports[0].HostPort, got.ImageDigests[0], got.HostSockets[0].Target, got.GatewaySkip[0] = 1, "x", "/x", 1
	if rec.Ports[0].HostPort != 3000 || rec.ImageDigests[0] != "sha256:aa" || rec.HostSockets[0].Target != "/run/gpg.sock" || rec.GatewaySkip[0] != 3000 {
		t.Fatal("the clone's record shares the source's lists")
	}
}

func TestCloneRefusesASourceItCannotTrust(t *testing.T) {
	for _, tc := range []struct {
		name  string
		edit  func(*appsv1.StatefulSet, *InstanceRecord)
		srcID string
		want  string
	}{
		{"its template changed", func(s *appsv1.StatefulSet, _ *InstanceRecord) {
			s.Spec.Template.Spec.Volumes = append(s.Spec.Template.Spec.Volumes, corev1.Volume{Name: "host", VolumeSource: corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{Path: "/"}}})
		}, "", "pod template differs"},
		{"made again", func(s *appsv1.StatefulSet, _ *InstanceRecord) { s.UID = "u-other" }, "", "fleet created"},
		{"not applied by fleet", func(s *appsv1.StatefulSet, _ *InstanceRecord) { s.ManagedFields = nil }, "", "not applied by fleet"},
		{"another daemon's", func(s *appsv1.StatefulSet, _ *InstanceRecord) { s.Labels[names.LabelOwner] = "someoneelse1" }, "", "not one this daemon made"},
		{"a job in progress", func(s *appsv1.StatefulSet, _ *InstanceRecord) { s.Annotations[names.AnnotationOp] = "rebuild@seed" }, "", "middle of a job"},
		{"another generation", func(_ *appsv1.StatefulSet, r *InstanceRecord) { r.Gen = 3 }, "", "root generation"},
		{"another connection", nil, ContainerID("elsewhere", handleNS, handleSTS(), 2), "another connection"},
		{"a docker container", nil, "0123456789ab", "not an instance on a cluster"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sts, rec := cloneSourceObjects(t, nil)
			if tc.edit != nil {
				tc.edit(sts, &rec)
			}
			c := cloneRig(t, sts, rec)
			if tc.srcID != "" {
				c.srcID = tc.srcID
			}
			err := c.loadSource(context.Background())
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestCloneNoticesADiskTheDriverRefuses(t *testing.T) {
	sts, rec := cloneSourceObjects(t, nil)
	c := cloneRig(t, sts, rec)
	c.plan, c.driver = ClonePlan{Tier: TierCSIClone}, "hostpath.csi.k8s.io"
	pvc := c.id.sts
	event := func(name, msg string, count int32, at time.Time) {
		t.Helper()
		ev := &corev1.Event{
			ObjectMeta:     metav1.ObjectMeta{Name: name, Namespace: handleNS},
			InvolvedObject: corev1.ObjectReference{Kind: "PersistentVolumeClaim", Name: pvc},
			Reason:         "ProvisioningFailed", Message: msg, Count: count, LastTimestamp: metav1.NewTime(at),
		}
		if _, err := c.cs.CoreV1().Events(handleNS).Create(context.Background(), ev, metav1.CreateOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	check := c.diskRefused(pvc)
	ctx := context.Background()
	if err := check(ctx); err != nil {
		t.Fatalf("no events: %v", err)
	}
	event("old", "rpc error: code = DeadlineExceeded", 9, c.start.Add(-time.Hour))
	event("once", "rpc error: code = DeadlineExceeded desc = context deadline exceeded", 1, time.Now())
	if err := check(ctx); err != nil {
		t.Fatalf("an old event, or one failure the provisioner retries: %v", err)
	}
	event("again", "rpc error: code = AlreadyExists desc = existing volume source volume id not matching", 3, time.Now())
	var refused *diskRefusedError
	if err := check(ctx); !errors.As(err, &refused) || refused.driver != "hostpath.csi.k8s.io" {
		t.Fatalf("a refusal the provisioner keeps making: %v", err)
	}
	if code, _ := kubeerr.Of(refused); code != kubeerr.Misconfigured {
		t.Fatalf("code %q", code)
	}
	c.plan.Tier = TierCopy
	if c.diskRefused(pvc) != nil {
		t.Fatal("a copier's empty disk has nothing to refuse")
	}
}

func TestCloneHoldsAStoppedSourceForNothing(t *testing.T) {
	sts, rec := cloneSourceObjects(t, nil)
	c := cloneRig(t, sts, rec)
	c.plan = ClonePlan{Tier: TierCopy, Hold: HoldFreeze}
	if err := c.hold(context.Background(), fleet.StepSeed); err != nil || c.isHeld() {
		t.Fatalf("hold of a stopped source: %v, held %v", err, c.isHeld())
	}
	c.letGo()
	if err := c.waitRestart(); err != nil {
		t.Fatal(err)
	}
}

func TestCloneFactsReadTheSourceDisksStorage(t *testing.T) {
	sts, rec := cloneSourceObjects(t, nil)
	c := cloneRig(t, sts, rec)
	if err := c.loadSource(context.Background()); err != nil {
		t.Fatal(err)
	}
	c.rec = fleet.KubeCluster{CloneConsistency: fleet.KubeCloneStopSource}
	f, err := c.cloneFacts(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	// A fake cluster: no StorageClass, no CSI driver, no snapshot API.
	if f.Driver != "" || f.SnapshotClass != "" || f.CSIClone || f.SourceRunning || f.Consistency != fleet.KubeCloneStopSource {
		t.Fatalf("facts %+v", f)
	}
	plan, err := selectCloneTier(f)
	if err != nil || plan.Tier != TierCopy || plan.Hold != HoldNone {
		t.Fatalf("plan %+v, %v", plan, err)
	}
}

// A running source that cannot be frozen makes a copier's clone
// best-effort (its final delta runs live), but not a snapshot's: a snapshot
// of a live disk is of one point in time, as a crash leaves it.
func TestAnUnfreezableSourceIsBestEffortOnlyForACopy(t *testing.T) {
	for _, tc := range []struct {
		tier CloneTier
		want bool
	}{{TierCopy, true}, {TierSnapshot, false}} {
		t.Run(string(tc.tier), func(t *testing.T) {
			sts, rec := cloneSourceObjects(t, nil)
			c := cloneRig(t, sts, rec)
			if err := c.loadSource(context.Background()); err != nil {
				t.Fatal(err)
			}
			c.jobCtx = context.Background()
			c.running, c.plan = true, ClonePlan{Tier: tc.tier, Hold: HoldFreeze}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			// The rig's source has no pod: its agent cannot freeze it.
			if err := c.hold(ctx, fleet.StepSeed); err != nil || c.isHeld() {
				t.Fatalf("hold: %v, held %v", err, c.isHeld())
			}
			if got := c.bestEffortWhy() != ""; got != tc.want {
				t.Fatalf("best-effort %v (%q), want %v", got, c.bestEffortWhy(), tc.want)
			}
			// Nothing held: the source's slot is free for another clone.
			slot, err := c.takeSlot(context.Background(), fleet.StepSeed)
			if err != nil {
				t.Fatal(err)
			}
			<-slot
		})
	}
}
