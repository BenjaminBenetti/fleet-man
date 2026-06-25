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

func TestInstanceAutoTag_ClosedPurple(t *testing.T) {
	// A branch whose only PR is closed/merged keeps a persistent purple "PR" badge
	// (issue #203): open_count 0 but closed_count > 0. It shows just "PR" — no
	// review/checks — in the purple style, and stays distinct from "no PR ever".
	inst := &fleet.Instance{Name: "agent-1", Status: fleet.StatusRunning}
	ps := &fleetgrpc.PrStatus{
		ClosedCount: 1,
		PrSignal:    fleetgrpc.PrSignal_PR_SIGNAL_PURPLE,
		Prs:         []*fleetgrpc.PrRef{{Number: 7, Url: "https://example.test/pr/7", Title: "done"}},
	}
	m := autoTagModel(newFleetPage(), inst, true, ps)
	got := m.instanceAutoTag("alpha", "agent-1", false)
	if !strings.Contains(got, "PR") {
		t.Fatalf("closed PR tag %q missing PR badge", got)
	}
	for _, unwanted := range []string{"Accepted", "Rejected", "Pending", "Checks", "PRx"} {
		if strings.Contains(got, unwanted) {
			t.Errorf("closed PR tag should be a bare PR badge, got %q (has %q)", got, unwanted)
		}
	}
	// The badge renders in the purple style.
	if !strings.Contains(got, prPurpleStyle.Render("PR")) {
		t.Errorf("closed PR badge %q not rendered in the purple style", got)
	}
}

func TestInstanceAutoTag_MultipleClosedPurple(t *testing.T) {
	// Two closed/merged PRs (e.g. workspace repo + a subrepo) render "PRx2" in
	// purple, matching the open "PRxN" convention and the multi-PR chooser.
	inst := &fleet.Instance{Name: "agent-1", Status: fleet.StatusRunning}
	ps := &fleetgrpc.PrStatus{
		ClosedCount: 2,
		PrSignal:    fleetgrpc.PrSignal_PR_SIGNAL_PURPLE,
		Prs: []*fleetgrpc.PrRef{
			{Number: 7, Url: "https://example.test/pr/7"},
			{Number: 8, Url: "https://example.test/pr/8"},
		},
	}
	m := autoTagModel(newFleetPage(), inst, true, ps)
	got := m.instanceAutoTag("alpha", "agent-1", false)
	if !strings.Contains(got, "PRx2") {
		t.Errorf("two closed PRs should render PRx2, got %q", got)
	}
	if !strings.Contains(got, prPurpleStyle.Render("PRx2")) {
		t.Errorf("PRx2 badge %q not rendered in the purple style", got)
	}
}

func TestPrSignalStylePurple(t *testing.T) {
	if prSignalStyle(fleetgrpc.PrSignal_PR_SIGNAL_PURPLE).GetForeground() != prPurpleStyle.GetForeground() {
		t.Errorf("a closed/merged PR should render in purple")
	}
}

func TestPrChecksStyleGraysPending(t *testing.T) {
	// Pending checks are de-emphasised to grey; pass/fail keep green/red.
	if prChecksStyle(fleetgrpc.PrSignal_PR_SIGNAL_YELLOW).GetForeground() != prGrayStyle.GetForeground() {
		t.Errorf("pending (yellow) checks should render in grey")
	}
	if prChecksStyle(fleetgrpc.PrSignal_PR_SIGNAL_GREEN).GetForeground() != prGreenStyle.GetForeground() {
		t.Errorf("passing checks should stay green")
	}
	if prChecksStyle(fleetgrpc.PrSignal_PR_SIGNAL_RED).GetForeground() != prRedStyle.GetForeground() {
		t.Errorf("failing checks should stay red")
	}
	// The PR indicator itself keeps yellow for its pending state.
	if prSignalStyle(fleetgrpc.PrSignal_PR_SIGNAL_YELLOW).GetForeground() != prYellowStyle.GetForeground() {
		t.Errorf("the PR indicator should keep yellow, not grey")
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
	for _, want := range []string{"PR", "Accepted", "Checks 3/3"} {
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
	for _, want := range []string{"PRx3", "Rejected", "Checks 4/6"} {
		if !strings.Contains(got, want) {
			t.Errorf("auto tag %q missing %q", got, want)
		}
	}
	if strings.Contains(got, "Accepted") || strings.Contains(got, "Pending") {
		t.Errorf("rejected tag should not contain Accepted/Pending: %q", got)
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
	if strings.Contains(got, "Accepted") || strings.Contains(got, "Rejected") || strings.Contains(got, "Pending") {
		t.Errorf("review element should be hidden: %q", got)
	}
	if strings.Contains(got, "Checks") {
		t.Errorf("checks element should be hidden when total is 0: %q", got)
	}
}

func TestBuildRowsMarksFirstChildForInlineAutoTagWhenNoUserTag(t *testing.T) {
	inst := &fleet.Instance{Name: "agent-1", Status: fleet.StatusRunning} // no user tag
	ps := &fleetgrpc.PrStatus{OpenCount: 1, PrSignal: fleetgrpc.PrSignal_PR_SIGNAL_GREEN}
	fp := newFleetPage()
	m := autoTagModel(fp, inst, true, ps)

	fp.buildRows(m)

	// The PR-status auto tag rides the first child row's status column rather
	// than a dedicated row, so no rowInstanceTag is inserted and the first child
	// (here "+ new session", since there are no sessions) is marked inline.
	for _, r := range fp.rows {
		if r.kind == rowInstanceTag {
			t.Fatalf("auto tag should not get its own row when there's no user tag: %#v", fp.rows)
		}
	}
	marked := 0
	firstChildInline := false
	for _, r := range fp.rows {
		if r.prStatusInline {
			marked++
			if r.kind == rowNewSession {
				firstChildInline = true
			}
		}
	}
	if marked != 1 {
		t.Fatalf("want exactly one inline-PR-status row, got %d: %#v", marked, fp.rows)
	}
	if !firstChildInline {
		t.Fatalf("first child row (+ new session) should carry the inline PR status: %#v", fp.rows)
	}
}

func TestBuildRowsMarksFirstSessionRowInlineWithPRStatus(t *testing.T) {
	inst := &fleet.Instance{Name: "agent-1", Status: fleet.StatusRunning, ContainerID: "abc"}
	ps := &fleetgrpc.PrStatus{OpenCount: 1, PrSignal: fleetgrpc.PrSignal_PR_SIGNAL_GREEN}
	fp := newFleetPage()
	fp.savedGroups[computeGroupKey("agent-1", "abc123")] = savedGroup{
		GroupID: "g-a", InstanceName: "agent-1",
		Sessions: []string{"g-a", "g-a~ff00"}, PaneCount: 2,
	}
	store := NewSessionStore()
	ref := InstanceRef{Fleet: "alpha", Instance: "agent-1"}
	store.SetExpanded(ref, true)
	store.SetDiscovery(ref, nil)
	m := &model{
		st:           &state.State{Fleets: map[string]*fleet.Fleet{"alpha": {Name: "alpha", Instances: []*fleet.Instance{inst}}}},
		sessionStore: store,
		fleetPage:    fp,
		runtime:      map[string]*fleetgrpc.InstanceRuntime{rtKey("alpha", "agent-1"): {Fleet: "alpha", Instance: "agent-1", PrStatus: ps}},
	}

	fp.buildRows(m)

	// The first child row is the session/group row; it — and only it — carries
	// the inline PR status, not the "+ new session" row below it.
	var firstChild *row
	for i := range fp.rows {
		if fp.rows[i].kind == rowSession || fp.rows[i].kind == rowNewSession {
			firstChild = &fp.rows[i]
			break
		}
	}
	if firstChild == nil || firstChild.kind != rowSession {
		t.Fatalf("expected first child to be a session row: %#v", fp.rows)
	}
	if !firstChild.prStatusInline {
		t.Fatalf("first session row should carry the inline PR status: %#v", fp.rows)
	}
	for _, r := range fp.rows {
		if r.kind == rowNewSession && r.prStatusInline {
			t.Fatalf("+ new session should not carry the inline PR status when a session exists")
		}
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
	for _, want := range []string{"PRx2", "Accepted", "Checks 5/8"} {
		if !strings.Contains(view, want) {
			t.Fatalf("view missing %q:\n%s", want, view)
		}
	}
}

func TestViewFleetListRendersClosedPurpleTag(t *testing.T) {
	// End-to-end: a closed-only PrStatus still marks the first child row inline and
	// surfaces the purple PR badge in the rendered list (issue #203) — the indicator
	// no longer vanishes when the PR closes.
	inst := &fleet.Instance{Name: "agent-1", Status: fleet.StatusRunning}
	ps := &fleetgrpc.PrStatus{
		ClosedCount: 1,
		PrSignal:    fleetgrpc.PrSignal_PR_SIGNAL_PURPLE,
		Prs:         []*fleetgrpc.PrRef{{Number: 7, Url: "https://example.test/pr/7", Title: "done"}},
	}
	fp := newFleetPage()
	m := autoTagModel(fp, inst, true, ps)
	fp.buildRows(m)

	inlineMarked := false
	for _, r := range fp.rows {
		if r.prStatusInline {
			inlineMarked = true
		}
	}
	if !inlineMarked {
		t.Fatalf("closed PR should still mark an inline-PR-status row: %#v", fp.rows)
	}
	if view := fp.viewFleetList(m); !strings.Contains(view, "PR") {
		t.Fatalf("rendered list missing the persistent closed PR badge:\n%s", view)
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
