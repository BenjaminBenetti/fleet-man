package tui

import (
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/BenjaminBenetti/fleet-man/fleetgrpc"
)

// storageStatus is a connection of three nodes in single-node mode: no
// class passed the multi-node test, a fleet-size cap, Filestore not tested
// yet (it asks first), an SMB class whose outcome is known.
func storageStatus() *fleetgrpc.KubeClusterStatus {
	st := kubeSt("gke-1", "prod-eu", fleetgrpc.KubeClusterState_KUBE_CLUSTER_STATE_LIMITED)
	st.Message = "each fleet on one node"
	st.Warnings = []string{"Each fleet's instances run together on one node (cap 6)"}
	st.Capabilities = &fleetgrpc.KubeCapabilities{
		ShareMode: "single-node", SharedStorageClass: "standard-rwo", InstanceStorageClass: "standard-rwo",
		SingleNodeFleetCap: 6, SingleNodeFleetCapReason: "node memory; attach limit 127",
		StorageClasses: []*fleetgrpc.KubeStorageClass{
			{Name: "standard-rwo", Provisioner: "pd.csi.storage.gke.io", IsDefault: true, Block: true, BindingMode: "WaitForFirstConsumer", AllowExpansion: true,
				MultiNodeTest: &fleetgrpc.KubeCheck{Result: fleetgrpc.KubeCheckResult_KUBE_CHECK_RESULT_SKIPPED, Detail: "not tested: its provisioner makes block disks only"}},
			{Name: "filestore", Provisioner: "filestore.csi.storage.gke.io", LargeMinimum: "1Ti",
				ShareHint:     "Filestore: the cluster's Filestore CSI driver must be enabled, and each share is at least 1Ti (so is the test's volume)",
				MultiNodeTest: &fleetgrpc.KubeCheck{Result: fleetgrpc.KubeCheckResult_KUBE_CHECK_RESULT_SKIPPED, Detail: "not tested: each of its volumes is at least 1Ti, so Auto asks first"}},
			{Name: "azurefile", Provisioner: "file.csi.azure.com", ShareHint: "SMB Azure Files keeps no owners, modes or symlinks, so it fails the multi-node test",
				DiskHint:      "a network file share for instance disks: it may not keep owners, and small-file IO is slow",
				MultiNodeTest: &fleetgrpc.KubeCheck{Result: fleetgrpc.KubeCheckResult_KUBE_CHECK_RESULT_FAIL, Detail: "file owners do not behave across the pods"}},
		},
	}
	st.Checks = []*fleetgrpc.KubeCheck{
		{Name: "root_storage", Result: fleetgrpc.KubeCheckResult_KUBE_CHECK_RESULT_PASS, Detail: "auto → standard-rwo (RWO, WFFC, expandable, network)"},
		{Name: "shared_storage", Result: fleetgrpc.KubeCheckResult_KUBE_CHECK_RESULT_WARN, Detail: "no class passed the multi-node test · failed: azurefile",
			Fix: "enable the Filestore CSI driver (RWX; each share is at least 1Ti)"},
		{Name: "attach_limits", Result: fleetgrpc.KubeCheckResult_KUBE_CHECK_RESULT_PASS, Detail: "127 volumes per node (pd.csi.storage.gke.io)\nfleet-size cap (single-node mode): 6 instances per fleet (node memory; attach limit 127)"},
		{Name: "share_pods", Result: fleetgrpc.KubeCheckResult_KUBE_CHECK_RESULT_PASS, Detail: "two pods on one node mount one volume"},
	}
	st.ShareMigrations = []*fleetgrpc.KubeShareMigration{{Fleet: "beta", FromClass: "standard-rwo", ToClass: "filestore", State: "waiting", Detail: "waits for 1 running instance on the cluster to stop"}}
	return st
}

// The Storage section: both pickers say what Auto resolved to, the share
// mode with the one-node warning and the fleet-size cap, the hint for no
// class passing, the attach limits, each class's multi-node result (a large
// one waiting for consent, an SMB one failed), the fleets' shares and the
// moves under way, and [ test storage ].
func TestEditorStorageSection(t *testing.T) {
	st := storageStatus()
	m, sp := kubeTestModel(t, st)
	e := openEditor(m, sp, "gke-1")
	e.update(m, sp, kubeShareUsageMsg{editor: e, shares: []*fleetgrpc.KubeShareUsage{
		{Cluster: "gke-1", Fleet: "alpha", UsedBytes: 1 << 30, CapacityBytes: 10 << 30, Measured: true, Expandable: true},
		{Cluster: "gke-1", Fleet: "beta", UsedBytes: 19 << 29, CapacityBytes: 10 << 30, Measured: true, Warning: "95% full; its StorageClass cannot grow it"},
	}})
	m.height = 200
	out := flatText(e.view(m, sp))
	for _, want := range []string{
		"Shared storage: [ auto → standard-rwo ] ▲ single-node mode",
		"no class passed the multi-node test · failed: azurefile",
		"hint: enable the Filestore CSI driver",
		"⚠ Each fleet's instances run together on one node (cap 6)",
		"cap: 6 instances per fleet (node memory; attach limit 127)",
		"Instance disks: [ auto → standard-rwo ]",
		"✓ auto → standard-rwo (RWO, WFFC, expandable, network)",
		"[ test storage ]",
		"attach limits 127 volumes per node (pd.csi.storage.gke.io) fleet-size cap (single-node mode): 6 instances per fleet",
		"share pods two pods on one node mount one volume",
		"class filestore",
		"not tested: each volume is at least 1Ti ([ test storage ] asks first)",
		"class azurefile",
		"✕ multi-node: file owners do not behave across the pods",
		"share of alpha: 1.0 GiB of 10.0 GiB (10%) · grows when 90% full",
		"share of beta: 9.5 GiB of 10.0 GiB (95%) ⚠ 95% full; its StorageClass cannot grow it",
		"beta's share moves standard-rwo → filestore waiting: waits for 1 running instance on the cluster to stop",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("the Storage section lacks %q:\n%s", want, out)
		}
	}
	// Shared storage comes first, the instance disks after.
	if i, j := strings.Index(out, "Shared storage:"), strings.Index(out, "Instance disks:"); i < 0 || j < i {
		t.Errorf("Shared storage should come before Instance disks:\n%s", out)
	}

	// The expected outcome shows for the class the pickers stand for.
	st.Cluster.SharedStorageClass, st.Cluster.StorageClass = "azurefile", "azurefile"
	out = flatText(e.view(m, sp))
	for _, want := range []string{"expected: SMB Azure Files keeps no owners", "⚠ a network file share for instance disks"} {
		if !strings.Contains(out, want) {
			t.Errorf("lacks %q:\n%s", want, out)
		}
	}
	for _, want := range []string{"Shared storage: [ azurefile ]", "Instance disks: [ azurefile ]"} {
		if !strings.Contains(out, want) {
			t.Errorf("a set class shows Auto's pick, not %q:\n%s", want, out)
		}
	}
}

// Picking a class whose volumes are large as the shared class asks first:
// no save until y; n keeps the setting.
func TestEditorLargeSharedClassAsksFirst(t *testing.T) {
	st := storageStatus()
	m, sp := kubeTestModel(t, st)
	calls := stubKubeUpdate(t, savedStatus(st))
	e := openEditor(m, sp, "gke-1")
	editorRowOf(t, e, "shared_storage_class")
	// auto → standard-rwo → filestore
	pumpKube(t, m, press(m, sp, "right"))
	pumpKube(t, m, press(m, sp, "right"))
	if len(*calls) != 1 || (*calls)[0].cluster.GetSharedStorageClass() != "standard-rwo" {
		t.Fatalf("saves %v", *calls)
	}
	st.Cluster.SharedStorageClass = "standard-rwo"
	pumpKube(t, m, press(m, sp, "right"))
	if len(*calls) != 1 || e.consent == nil {
		t.Fatalf("picking filestore saved without asking: %v", *calls)
	}
	if out := flatText(e.view(m, sp)); !strings.Contains(out, "filestore: each volume is at least 1Ti, and the multi-node test makes one") || !strings.Contains(out, "[y] save and test it") {
		t.Fatalf("no question:\n%s", out)
	}
	pumpKube(t, m, press(m, sp, "n"))
	if len(*calls) != 1 || e.consent != nil {
		t.Fatalf("n: saves %v consent %v", *calls, e.consent)
	}
	pumpKube(t, m, press(m, sp, "right"))
	pumpKube(t, m, press(m, sp, "y"))
	if len(*calls) != 2 || (*calls)[1].cluster.GetSharedStorageClass() != "filestore" || !slices.Equal((*calls)[1].paths, []string{"shared_storage_class"}) {
		t.Fatalf("y: saves %v", *calls)
	}
}

// [ test storage ] runs the multi-node test again; in Auto, with a class
// whose volumes are large not tested yet, it asks: y tests it too, n the
// others only.
func TestEditorTestStorage(t *testing.T) {
	st := storageStatus()
	m, sp := kubeTestModel(t, st)
	reqs := stubKubeTest(t, func(*fleetgrpc.TestKubeClusterRequest) []*fleetgrpc.TestKubeClusterEvent { return testEvents(st) })
	e := openEditor(m, sp, "gke-1")
	for i, row := range e.rows() {
		if row.kind == kubeEditorRowAction && row.action == kubeEditorActionTestStorage {
			e.row = i
		}
	}
	pumpKube(t, m, press(m, sp, "enter"))
	if len(*reqs) != 0 || e.consent == nil {
		t.Fatalf("tested without asking: %v", *reqs)
	}
	pumpKube(t, m, press(m, sp, "y"))
	if len(*reqs) != 1 || !(*reqs)[0].GetTestStorage() || !slices.Equal((*reqs)[0].GetStorageClasses(), []string{"filestore"}) || (*reqs)[0].GetId() != "gke-1" {
		t.Fatalf("y: %v", *reqs)
	}
	pumpKube(t, m, press(m, sp, "enter"))
	pumpKube(t, m, press(m, sp, "n"))
	if len(*reqs) != 2 || !(*reqs)[1].GetTestStorage() || len((*reqs)[1].GetStorageClasses()) != 0 {
		t.Fatalf("n: %v", *reqs)
	}
	// Tested already (or a class set): no question.
	m.kubeStatus("gke-1").Capabilities.StorageClasses[1].MultiNodeTest = &fleetgrpc.KubeCheck{Result: fleetgrpc.KubeCheckResult_KUBE_CHECK_RESULT_PASS}
	pumpKube(t, m, press(m, sp, "enter"))
	if len(*reqs) != 3 || e.consent != nil {
		t.Fatalf("a tested class asked again: %v", *reqs)
	}
}

// Without the right to list StorageClasses, the editor says to type one,
// and a typed name saves.
func TestEditorStorageClassesUnreadable(t *testing.T) {
	st := kubeSt("eks-1", "staging", fleetgrpc.KubeClusterState_KUBE_CLUSTER_STATE_READY)
	st.Capabilities = &fleetgrpc.KubeCapabilities{StorageClassesUnreadable: true, ShareMode: "single-node"}
	m, sp := kubeTestModel(t, st)
	calls := stubKubeUpdate(t, savedStatus(st))
	e := openEditor(m, sp, "eks-1")
	if out := flatText(e.view(m, sp)); !strings.Contains(out, "StorageClasses are not readable here type a class name on a picker") {
		t.Fatalf("no typed-name note:\n%s", out)
	}
	editorRowOf(t, e, "shared_storage_class")
	pumpKube(t, m, press(m, sp, "enter"))
	for _, r := range "efs-static" {
		pumpKube(t, m, press(m, sp, string(r)))
	}
	pumpKube(t, m, press(m, sp, "enter"))
	if len(*calls) != 1 || (*calls)[0].cluster.GetSharedStorageClass() != "efs-static" {
		t.Fatalf("saves %v", *calls)
	}
}

// The shares' usage comes from GetShareSync, this cluster's only; a
// failure is shown, not fatal.
func TestEditorLoadsTheSharesUsage(t *testing.T) {
	orig := getShareSyncRemote
	t.Cleanup(func() { getShareSyncRemote = orig })
	getShareSyncRemote = func() (*fleetgrpc.GetShareSyncReply, error) {
		return &fleetgrpc.GetShareSyncReply{Fleets: []*fleetgrpc.FleetShareSync{{Fleet: "alpha", Shares: []*fleetgrpc.KubeShareUsage{
			{Cluster: "gke-1", Fleet: "alpha", CapacityBytes: 10 << 30}, {Cluster: "other", Fleet: "alpha", CapacityBytes: 5 << 30},
		}}}}, nil
	}
	st := storageStatus()
	m, sp := kubeTestModel(t, st)
	e := openEditor(m, sp, "gke-1")
	msg := e.loadUsage()().(kubeShareUsageMsg)
	if len(msg.shares) != 1 || msg.shares[0].GetCluster() != "gke-1" {
		t.Fatalf("shares %v", msg.shares)
	}
	e.update(m, sp, msg)
	m.height = 200
	if out := flatText(e.view(m, sp)); !strings.Contains(out, "share of alpha: 10.0 GiB (not measured: no pod of the fleet runs)") {
		t.Fatalf("usage:\n%s", out)
	}
	e.update(m, sp, kubeShareUsageMsg{editor: e, err: errors.New("unavailable")})
	if out := flatText(e.view(m, sp)); !strings.Contains(out, "shares: unavailable") {
		t.Fatalf("usage error:\n%s", out)
	}
}

// Every class line gives its multi-node result.
func TestKubeMultiNodeBadge(t *testing.T) {
	for _, tc := range []struct {
		sc   *fleetgrpc.KubeStorageClass
		want string
	}{
		{&fleetgrpc.KubeStorageClass{MultiNodeTest: &fleetgrpc.KubeCheck{Result: fleetgrpc.KubeCheckResult_KUBE_CHECK_RESULT_PASS}}, "✓ multi-node"},
		{&fleetgrpc.KubeStorageClass{MultiNodeTest: &fleetgrpc.KubeCheck{Result: fleetgrpc.KubeCheckResult_KUBE_CHECK_RESULT_WARN, Detail: "the test could not tell"}}, "▲ multi-node: the test could not tell"},
		{&fleetgrpc.KubeStorageClass{MultiNodeTest: &fleetgrpc.KubeCheck{Result: fleetgrpc.KubeCheckResult_KUBE_CHECK_RESULT_SKIPPED, Detail: "not tested: its volumes are on one node's disk"}}, "– its volumes are on one node's disk"},
		{&fleetgrpc.KubeStorageClass{ShareHint: "CephFS: typically passes the multi-node test"}, "expected: CephFS: typically passes"},
		{&fleetgrpc.KubeStorageClass{}, ""},
	} {
		if got := flatText(kubeMultiNodeBadge(tc.sc)); !strings.Contains(got, tc.want) || (tc.want == "") != (got == "") {
			t.Errorf("badge %q, want %q", got, tc.want)
		}
	}
}
