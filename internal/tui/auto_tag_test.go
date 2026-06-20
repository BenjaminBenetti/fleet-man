package tui

import (
	"strings"
	"testing"

	"github.com/BenjaminBenetti/fleet-man/fleetgrpc"
	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	"github.com/BenjaminBenetti/fleet-man/internal/state"
)

// autoTagModel builds a model with one instance and an optional PrStatus on the
// runtime sidecar, mirroring tagTestModel but for the auto-tag path.
func autoTagModel(fp *fleetPage, inst *fleet.Instance, expanded bool, ps *fleetgrpc.PrStatus) *model {
	store := NewSessionStore()
	if expanded {
		store.SetExpanded(InstanceRef{Fleet: "alpha", Instance: inst.Name}, true)
	}
	runtime := map[string]*fleetgrpc.InstanceRuntime{}
	if ps != nil {
		runtime[rtKey("alpha", inst.Name)] = &fleetgrpc.InstanceRuntime{
			Fleet: "alpha", Instance: inst.Name, PrStatus: ps,
		}
	}
	return &model{
		st: &state.State{
			Fleets: map[string]*fleet.Fleet{
				"alpha": {Name: "alpha", Instances: []*fleet.Instance{inst}},
			},
		},
		sessionStore: store,
		fleetPage:    fp,
		runtime:      runtime,
	}
}

func TestInstanceAutoTag_NoStatus(t *testing.T) {
	inst := &fleet.Instance{Name: "agent-1", Status: fleet.StatusRunning}
	m := autoTagModel(newFleetPage(), inst, true, nil)
	if got := m.instanceAutoTag("alpha", "agent-1", false); got != "" {
		t.Fatalf("auto tag with no runtime = %q, want empty", got)
	}
	// Probed, but no open PRs.
	m = autoTagModel(newFleetPage(), inst, true, &fleetgrpc.PrStatus{OpenCount: 0})
	if got := m.instanceAutoTag("alpha", "agent-1", false); got != "" {
		t.Fatalf("auto tag with zero open PRs = %q, want empty", got)
	}
}

func TestInstanceAutoTag_Format(t *testing.T) {
	inst := &fleet.Instance{Name: "agent-1", Status: fleet.StatusRunning}
	ps := &fleetgrpc.PrStatus{
		OpenCount:    1,
		PrSignal:     fleetgrpc.PrSignal_PR_SIGNAL_GREEN,
		Review:       fleetgrpc.PrReviewState_PR_REVIEW_STATE_APPROVED,
		ChecksPassed: 3,
		ChecksTotal:  3,
		ChecksSignal: fleetgrpc.PrSignal_PR_SIGNAL_GREEN,
	}
	m := autoTagModel(newFleetPage(), inst, true, ps)
	got := m.instanceAutoTag("alpha", "agent-1", false)
	for _, want := range []string{"PR", "Approved", "Checks 3/3"} {
		if !strings.Contains(got, want) {
			t.Errorf("auto tag %q missing %q", got, want)
		}
	}
	if strings.Contains(got, "PRx") {
		t.Errorf("single PR should render \"PR\", not %q", got)
	}
}

func TestInstanceAutoTag_MultiplePRsAndChangesRequested(t *testing.T) {
	inst := &fleet.Instance{Name: "agent-1", Status: fleet.StatusRunning}
	ps := &fleetgrpc.PrStatus{
		OpenCount:    3,
		PrSignal:     fleetgrpc.PrSignal_PR_SIGNAL_RED,
		Review:       fleetgrpc.PrReviewState_PR_REVIEW_STATE_CHANGES_REQUESTED,
		ChecksPassed: 4,
		ChecksTotal:  6,
		ChecksSignal: fleetgrpc.PrSignal_PR_SIGNAL_RED,
	}
	m := autoTagModel(newFleetPage(), inst, true, ps)
	got := m.instanceAutoTag("alpha", "agent-1", false)
	for _, want := range []string{"PRx3", "Changes Requested", "Checks 4/6"} {
		if !strings.Contains(got, want) {
			t.Errorf("auto tag %q missing %q", got, want)
		}
	}
	if strings.Contains(got, "Approved") {
		t.Errorf("changes-requested tag should not contain Approved: %q", got)
	}
}

func TestInstanceAutoTag_HidesReviewAndChecksWhenAbsent(t *testing.T) {
	inst := &fleet.Instance{Name: "agent-1", Status: fleet.StatusRunning}
	ps := &fleetgrpc.PrStatus{
		OpenCount: 1,
		PrSignal:  fleetgrpc.PrSignal_PR_SIGNAL_YELLOW,
		Review:    fleetgrpc.PrReviewState_PR_REVIEW_STATE_UNSPECIFIED,
		// No checks at all.
		ChecksTotal: 0,
	}
	m := autoTagModel(newFleetPage(), inst, true, ps)
	got := m.instanceAutoTag("alpha", "agent-1", false)
	if !strings.Contains(got, "PR") {
		t.Errorf("auto tag %q missing PR indicator", got)
	}
	if strings.Contains(got, "Approved") || strings.Contains(got, "Changes Requested") {
		t.Errorf("review element should be hidden: %q", got)
	}
	if strings.Contains(got, "Checks") {
		t.Errorf("checks element should be hidden when total is 0: %q", got)
	}
}

func TestBuildRowsInsertsAutoTagRowWhenNoUserTag(t *testing.T) {
	inst := &fleet.Instance{Name: "agent-1", Status: fleet.StatusRunning} // no user tag
	ps := &fleetgrpc.PrStatus{OpenCount: 1, PrSignal: fleetgrpc.PrSignal_PR_SIGNAL_GREEN}
	fp := newFleetPage()
	m := autoTagModel(fp, inst, true, ps)

	fp.buildRows(m)

	found := false
	for _, r := range fp.rows {
		if r.kind == rowInstanceTag {
			found = true
		}
	}
	if !found {
		t.Fatalf("auto-tag row not inserted: %#v", fp.rows)
	}
}

func TestBuildRowsNoAutoTagRowWhenNoPRs(t *testing.T) {
	inst := &fleet.Instance{Name: "agent-1", Status: fleet.StatusRunning}
	fp := newFleetPage()
	m := autoTagModel(fp, inst, true, nil) // no PrStatus at all

	fp.buildRows(m)

	for _, r := range fp.rows {
		if r.kind == rowInstanceTag {
			t.Fatalf("tag row present with no user tag and no PR status: %#v", fp.rows)
		}
	}
}

func TestViewFleetListRendersAutoTag(t *testing.T) {
	inst := &fleet.Instance{Name: "agent-1", Status: fleet.StatusRunning}
	ps := &fleetgrpc.PrStatus{
		OpenCount:    2,
		PrSignal:     fleetgrpc.PrSignal_PR_SIGNAL_YELLOW,
		Review:       fleetgrpc.PrReviewState_PR_REVIEW_STATE_APPROVED,
		ChecksPassed: 5,
		ChecksTotal:  8,
		ChecksSignal: fleetgrpc.PrSignal_PR_SIGNAL_YELLOW,
	}
	fp := newFleetPage()
	m := autoTagModel(fp, inst, true, ps)
	fp.buildRows(m)

	view := fp.viewFleetList(m)
	for _, want := range []string{"PRx2", "Approved", "Checks 5/8"} {
		if !strings.Contains(view, want) {
			t.Fatalf("view missing %q:\n%s", want, view)
		}
	}
}

func TestUserTagTakesPrecedenceOverAutoTag(t *testing.T) {
	inst := &fleet.Instance{Name: "agent-1", Status: fleet.StatusRunning, Tag: "refactor auth"}
	ps := &fleetgrpc.PrStatus{
		OpenCount:   1,
		PrSignal:    fleetgrpc.PrSignal_PR_SIGNAL_GREEN,
		ChecksTotal: 3, ChecksPassed: 3, ChecksSignal: fleetgrpc.PrSignal_PR_SIGNAL_GREEN,
	}
	fp := newFleetPage()
	m := autoTagModel(fp, inst, true, ps)
	fp.buildRows(m)

	view := fp.viewFleetList(m)
	if !strings.Contains(view, "# refactor auth") {
		t.Fatalf("user tag not rendered:\n%s", view)
	}
	if strings.Contains(view, "Checks 3/3") {
		t.Fatalf("auto tag rendered despite a user-set tag:\n%s", view)
	}
}
