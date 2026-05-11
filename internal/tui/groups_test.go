package tui

import "testing"

func TestGroupRestoreTokens(t *testing.T) {
	fp := newFleetPage()

	first := fp.beginGroupRestore("abc123")
	if first == 0 {
		t.Fatal("beginGroupRestore returned zero token")
	}
	if !fp.restoreInProgress() {
		t.Fatal("restore should be marked in progress")
	}

	second := fp.beginGroupRestore("def456")
	if second == first {
		t.Fatal("restore token did not advance")
	}
	if fp.finishGroupRestore(first) {
		t.Fatal("stale restore token should be rejected")
	}
	if !fp.restoreInProgress() {
		t.Fatal("stale restore token cleared active restore")
	}
	if !fp.finishGroupRestore(second) {
		t.Fatal("current restore token should be accepted")
	}
	if fp.restoreInProgress() {
		t.Fatal("current restore token did not clear active restore")
	}
}

// TestSameSavedGroupEqual confirms byte-identical savedGroups compare equal.
// The diff gate in saveCurrentGroupLayout depends on this being true so
// that idle discovery ticks don't rewrite state.json.
func TestSameSavedGroupEqual(t *testing.T) {
	a := savedGroup{
		GroupID:      "abc123",
		InstanceName: "alpha",
		Sessions:     []string{"alpha~abc123", "alpha~abc123~ff00"},
		Layout:       "4fe2,140x40,0,0{...}",
		PaneCount:    2,
	}
	b := savedGroup{
		GroupID:      "abc123",
		InstanceName: "alpha",
		Sessions:     []string{"alpha~abc123", "alpha~abc123~ff00"},
		Layout:       "4fe2,140x40,0,0{...}",
		PaneCount:    2,
	}
	if !sameSavedGroup(a, b) {
		t.Fatalf("identical savedGroups reported unequal")
	}
}

// TestSameSavedGroupFieldDifferences verifies every scalar field
// participates in the comparison. If any field is dropped from
// sameSavedGroup, the corresponding subtest fails and the poll loop
// silently stops persisting that kind of change.
func TestSameSavedGroupFieldDifferences(t *testing.T) {
	base := savedGroup{
		GroupID:      "abc123",
		InstanceName: "alpha",
		Sessions:     []string{"alpha~abc123"},
		Layout:       "L",
		PaneCount:    1,
	}
	cases := []struct {
		name string
		mut  func(*savedGroup)
	}{
		{"GroupID", func(g *savedGroup) { g.GroupID = "other" }},
		{"InstanceName", func(g *savedGroup) { g.InstanceName = "beta" }},
		{"Layout", func(g *savedGroup) { g.Layout = "L2" }},
		{"PaneCount", func(g *savedGroup) { g.PaneCount = 2 }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b := base
			tc.mut(&b)
			if sameSavedGroup(base, b) {
				t.Fatalf("sameSavedGroup returned true when %s differed", tc.name)
			}
		})
	}
}

// TestSameSavedGroupSessionsDiffer covers slice-content and slice-length
// differences, which the scalar loop above can't express without a
// helper.
func TestSameSavedGroupSessionsDiffer(t *testing.T) {
	base := savedGroup{
		GroupID:      "abc123",
		InstanceName: "alpha",
		Sessions:     []string{"alpha~abc123", "alpha~abc123~ff00"},
		Layout:       "L",
		PaneCount:    2,
	}

	t.Run("different element", func(t *testing.T) {
		b := base
		b.Sessions = []string{"alpha~abc123", "alpha~abc123~aaaa"}
		if sameSavedGroup(base, b) {
			t.Fatal("sameSavedGroup ignored a session content change")
		}
	})

	t.Run("shorter slice", func(t *testing.T) {
		b := base
		b.Sessions = []string{"alpha~abc123"}
		b.PaneCount = 1 // keep scalars aligned — otherwise the scalar check short-circuits
		if sameSavedGroup(base, b) {
			t.Fatal("sameSavedGroup ignored a session length change")
		}
	})

	t.Run("longer slice", func(t *testing.T) {
		b := base
		b.Sessions = []string{"alpha~abc123", "alpha~abc123~ff00", "alpha~abc123~bbbb"}
		b.PaneCount = 3
		if sameSavedGroup(base, b) {
			t.Fatal("sameSavedGroup ignored a session length change")
		}
	})
}

// TestSameSavedGroupNilVsEmpty documents that nil and empty Sessions
// slices compare equal — both have length zero and the content loop
// never runs. saveCurrentGroupLayout only ever assigns non-nil slices,
// so the distinction doesn't matter in practice.
func TestSameSavedGroupNilVsEmpty(t *testing.T) {
	a := savedGroup{GroupID: "g", InstanceName: "i", Sessions: nil, Layout: "L"}
	b := savedGroup{GroupID: "g", InstanceName: "i", Sessions: []string{}, Layout: "L"}
	if !sameSavedGroup(a, b) {
		t.Fatal("nil and empty Sessions slices should compare equal")
	}
}

func TestSavedGroupSessionNamesUsesSavedOrder(t *testing.T) {
	sg := savedGroup{
		GroupID:      "abc123",
		InstanceName: "alpha",
		Sessions:     []string{"alpha~abc123", "alpha~abc123~ff00"},
		PaneCount:    2,
	}

	got := savedGroupSessionNames(sg, "alpha")
	want := []string{"alpha~abc123", "alpha~abc123~ff00"}
	if len(got) != len(want) {
		t.Fatalf("len = %d, want %d: %#v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("session[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

// TestSavedGroupSessionNamesFallsBackToRootWhenSessionsEmpty documents
// the only legitimate synthesis path: legacy state that recorded a
// PaneCount with no Sessions array. Returns a single root session — the
// callers don't see an empty slice but also don't get padded ghost
// names like the old implementation produced.
func TestSavedGroupSessionNamesFallsBackToRootWhenSessionsEmpty(t *testing.T) {
	sg := savedGroup{
		GroupID:      "abc123",
		InstanceName: "alpha",
		PaneCount:    2,
	}

	got := savedGroupSessionNames(sg, "alpha")
	want := []string{"alpha~abc123"}
	if len(got) != len(want) {
		t.Fatalf("len = %d, want %d: %#v", len(got), len(want), got)
	}
	if got[0] != want[0] {
		t.Fatalf("session[0] = %q, want %q", got[0], want[0])
	}
}

// TestSavedGroupSessionNamesDoesNotPadFromPaneCount is the regression
// test for the PR #42 ghost-pane bug. PaneCount > len(Sessions) must NOT
// trigger fabrication of `~restored##` names — those get persisted on
// the next save and later restore as blank tmux sessions.
func TestSavedGroupSessionNamesDoesNotPadFromPaneCount(t *testing.T) {
	sg := savedGroup{
		GroupID:      "abc123",
		InstanceName: "alpha",
		Sessions:     []string{"alpha~abc123"},
		PaneCount:    3,
	}

	got := savedGroupSessionNames(sg, "alpha")
	want := []string{"alpha~abc123"}
	if len(got) != len(want) {
		t.Fatalf("len = %d, want %d (pad must not happen): %#v", len(got), len(want), got)
	}
	if got[0] != want[0] {
		t.Fatalf("session[0] = %q, want %q", got[0], want[0])
	}
}

func TestDerivePersistableSnapshotHappyPath(t *testing.T) {
	active := ActiveGroup{
		Ref:     InstanceRef{Fleet: "repo", Instance: "alpha"},
		GroupID: "abc123",
	}
	panes := []paneByPosition{
		{title: "alpha~abc123"},
		{title: "alpha~abc123~ff00"},
	}
	sg, ok := derivePersistableSnapshot(active, panes, "layout-string")
	if !ok {
		t.Fatal("expected ok=true for well-formed pane titles")
	}
	if sg.PaneCount != 2 {
		t.Fatalf("PaneCount = %d, want 2", sg.PaneCount)
	}
	if sg.Layout != "layout-string" {
		t.Fatalf("Layout = %q, want layout-string", sg.Layout)
	}
	want := []string{"alpha~abc123", "alpha~abc123~ff00"}
	if len(sg.Sessions) != len(want) {
		t.Fatalf("Sessions len = %d, want %d", len(sg.Sessions), len(want))
	}
	for i := range want {
		if sg.Sessions[i] != want[i] {
			t.Fatalf("Sessions[%d] = %q, want %q", i, sg.Sessions[i], want[i])
		}
	}
}

// TestDerivePersistableSnapshotBailsOnEmptyTitle is the core regression
// guard: when a pane hasn't been tagged by `fleet shell` yet, its title
// is empty (or the host hostname, see ParsesUnknownTitleAsHost). We must
// bail rather than fabricate a placeholder, because fabricated names get
// persisted and later restored as ghost panes.
func TestDerivePersistableSnapshotBailsOnEmptyTitle(t *testing.T) {
	active := ActiveGroup{
		Ref:     InstanceRef{Fleet: "repo", Instance: "alpha"},
		GroupID: "abc123",
	}
	panes := []paneByPosition{
		{title: "alpha~abc123"},
		{title: ""},
	}
	if _, ok := derivePersistableSnapshot(active, panes, "L"); ok {
		t.Fatal("expected ok=false when a pane has no title")
	}
}

func TestDerivePersistableSnapshotBailsOnUnparseableTitle(t *testing.T) {
	active := ActiveGroup{
		Ref:     InstanceRef{Fleet: "repo", Instance: "alpha"},
		GroupID: "abc123",
	}
	panes := []paneByPosition{
		{title: "alpha~abc123"},
		{title: "runner-host"}, // pre-tag default, must NOT be coerced
	}
	if _, ok := derivePersistableSnapshot(active, panes, "L"); ok {
		t.Fatal("expected ok=false when a pane title doesn't parse as a group session")
	}
}

func TestDerivePersistableSnapshotBailsOnForeignGroup(t *testing.T) {
	active := ActiveGroup{
		Ref:     InstanceRef{Fleet: "repo", Instance: "alpha"},
		GroupID: "abc123",
	}
	panes := []paneByPosition{
		{title: "alpha~abc123"},
		{title: "alpha~def456~ff00"}, // belongs to a different group
	}
	if _, ok := derivePersistableSnapshot(active, panes, "L"); ok {
		t.Fatal("expected ok=false when a pane belongs to a different group")
	}
}

func TestDerivePersistableSnapshotBailsOnDuplicateTitle(t *testing.T) {
	active := ActiveGroup{
		Ref:     InstanceRef{Fleet: "repo", Instance: "alpha"},
		GroupID: "abc123",
	}
	panes := []paneByPosition{
		{title: "alpha~abc123"},
		{title: "alpha~abc123"},
	}
	if _, ok := derivePersistableSnapshot(active, panes, "L"); ok {
		t.Fatal("expected ok=false when two panes report the same title")
	}
}

func TestDerivePersistableSnapshotBailsWithoutActiveGroup(t *testing.T) {
	panes := []paneByPosition{{title: "alpha~abc123"}}
	if _, ok := derivePersistableSnapshot(ActiveGroup{}, panes, "L"); ok {
		t.Fatal("expected ok=false when activeGroup is empty")
	}
}

func TestDerivePersistableSnapshotBailsWithNoPanes(t *testing.T) {
	active := ActiveGroup{
		Ref:     InstanceRef{Fleet: "repo", Instance: "alpha"},
		GroupID: "abc123",
	}
	if _, ok := derivePersistableSnapshot(active, nil, "L"); ok {
		t.Fatal("expected ok=false when no panes exist")
	}
}

func TestRestoreSessionNamesTopsUpIncompleteLiveDiscovery(t *testing.T) {
	sg := savedGroup{
		GroupID:      "abc123",
		InstanceName: "alpha",
		Sessions:     []string{"alpha~abc123", "alpha~abc123~ff00"},
		PaneCount:    2,
	}

	got := restoreSessionNames(
		"alpha~abc123\n",
		"alpha~abc123",
		sg.Sessions,
		&sg,
		"alpha",
	)
	want := []string{"alpha~abc123", "alpha~abc123~ff00"}
	if len(got) != len(want) {
		t.Fatalf("len = %d, want %d: %#v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("session[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestRestoreSessionNamesUsesSavedLayoutOverLiveDiscovery(t *testing.T) {
	sg := savedGroup{
		GroupID:      "abc123",
		InstanceName: "alpha",
		Sessions:     []string{"alpha~abc123", "alpha~abc123~ff00"},
		PaneCount:    2,
	}

	got := restoreSessionNames(
		"alpha~abc123\nalpha~abc123~stale\n",
		"alpha~abc123",
		nil,
		&sg,
		"alpha",
	)
	want := []string{"alpha~abc123", "alpha~abc123~ff00"}
	if len(got) != len(want) {
		t.Fatalf("len = %d, want %d: %#v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("session[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}
