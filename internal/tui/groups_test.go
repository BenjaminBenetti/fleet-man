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

func TestRestoreSessionNamesRecreatesAllWhenGroupFullyDead(t *testing.T) {
	// Fleet/instance restart: no live session remains for the group, so
	// the snapshot is the only record of the panes — recreate all of
	// them (new-session -A) in saved order.
	sg := savedGroup{
		GroupID:      "abc123",
		InstanceName: "alpha",
		Sessions:     []string{"alpha~abc123", "alpha~abc123~ff00"},
		PaneCount:    2,
	}

	got := restoreSessionNames(
		"",
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

func TestRestoreSessionNamesDropsDeadSessionsWhenGroupAlive(t *testing.T) {
	// Issue #158: the group still has a live session, so a snapshot
	// entry missing from the live set was deliberately killed (a restart
	// would have killed the survivor too). It must NOT be recreated —
	// new-session -A would resurrect it as a ghost pane that the layout
	// tick then persists as real state.
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
	want := []string{"alpha~abc123"}
	if len(got) != len(want) {
		t.Fatalf("len = %d, want %d: %#v", len(got), len(want), got)
	}
	if got[0] != want[0] {
		t.Fatalf("session[0] = %q, want %q", got[0], want[0])
	}
}

func TestRestoreSessionNamesMergesSnapshotWithLiveGroup(t *testing.T) {
	// Issue #158: with the group alive, the snapshot contributes pane
	// order but the live set decides membership. Another TUI killed ff00
	// (dropped, not resurrected) and added 3421 (appended); sessions
	// outside the group prefix are ignored.
	sg := savedGroup{
		GroupID:      "abc123",
		InstanceName: "alpha",
		Sessions:     []string{"alpha~abc123", "alpha~abc123~ff00"},
		PaneCount:    2,
	}

	got := restoreSessionNames(
		"alpha~abc123\nalpha~abc123~3421\nalpha~other\n",
		"alpha~abc123",
		nil,
		&sg,
		"alpha",
	)
	want := []string{"alpha~abc123", "alpha~abc123~3421"}
	if len(got) != len(want) {
		t.Fatalf("len = %d, want %d: %#v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("session[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

// TestSnapshotMatchesRuntime covers the issue #158 stale-view guard: a
// snapshot may only be persisted when its session set is exactly the set
// of live inner-tmux sessions for the group.
func TestSnapshotMatchesRuntime(t *testing.T) {
	snapshot := savedGroup{
		GroupID:      "abc123",
		InstanceName: "alpha",
		Sessions:     []string{"alpha~abc123", "alpha~abc123~ff00"},
		PaneCount:    2,
	}

	cases := []struct {
		name string
		live []tmuxSession
		want bool
	}{
		{
			name: "exact match, order ignored",
			live: []tmuxSession{{Name: "alpha~abc123~ff00"}, {Name: "alpha~abc123"}},
			want: true,
		},
		{
			name: "sessions from other groups and bare names are ignored",
			live: []tmuxSession{
				{Name: "alpha~abc123"},
				{Name: "alpha~abc123~ff00"},
				{Name: "alpha~other"},
				{Name: "unrelated"},
			},
			want: true,
		},
		{
			name: "runtime has a group session the snapshot lacks (other TUI added a pane)",
			live: []tmuxSession{
				{Name: "alpha~abc123"},
				{Name: "alpha~abc123~ff00"},
				{Name: "alpha~abc123~3421"},
			},
			want: false,
		},
		{
			name: "snapshot has a session the runtime lacks (dead session / poll lag)",
			live: []tmuxSession{{Name: "alpha~abc123"}},
			want: false,
		},
		{
			name: "no runtime cached",
			live: nil,
			want: false,
		},
	}
	for _, tc := range cases {
		if got := snapshotMatchesRuntime(snapshot, tc.live); got != tc.want {
			t.Errorf("%s: snapshotMatchesRuntime = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestResolveSessionNameCanonicalizesBareNames(t *testing.T) {
	// A short, user/agent-supplied name becomes the root session of a
	// group named after it — the same name the TUI's create-session
	// dialog would mint.
	if got := ResolveSessionName("agent-1", "main"); got != "agent-1~main" {
		t.Fatalf("ResolveSessionName bare = %q, want %q", got, "agent-1~main")
	}
	// Instance names are sanitized the same way the TUI sanitizes them.
	if got := ResolveSessionName("my.fleet/agent", "main"); got != "my-fleet-agent~main" {
		t.Fatalf("ResolveSessionName sanitized instance = %q, want %q", got, "my-fleet-agent~main")
	}
	// The session name itself is sanitized too.
	if got := ResolveSessionName("agent-1", "my.task"); got != "agent-1~my-task" {
		t.Fatalf("ResolveSessionName sanitized name = %q, want %q", got, "agent-1~my-task")
	}
}

func TestResolveSessionNamePassesThroughConformingNames(t *testing.T) {
	// Root and pane names that already follow the convention for this
	// instance must not be double-prefixed.
	for _, name := range []string{"agent-1~a1b2c3", "agent-1~a1b2c3~ff", "agent-1~main"} {
		if got := ResolveSessionName("agent-1", name); got != name {
			t.Fatalf("ResolveSessionName(%q) = %q, want unchanged", name, got)
		}
	}
	// A name conforming to a *different* instance's convention is not a
	// group of this instance — it gets canonicalized under this instance.
	if got := ResolveSessionName("agent-1", "other~main"); got != "agent-1~other~main" {
		t.Fatalf("ResolveSessionName foreign = %q, want %q", got, "agent-1~other~main")
	}
}

// TestCliSpawnedSessionFormsRealGroup pins the CLI↔TUI naming contract
// behind the session-duplication bug: a session created via
// `fleet spawn-session <inst> main` must parse as a *real* group named
// "main" (not a pseudo-group), and the pane session a split mints for
// that group must land in the same group.
func TestCliSpawnedSessionFormsRealGroup(t *testing.T) {
	root := ResolveSessionName("agent-1", "main") // what spawn-session creates

	groups := groupSessions("agent-1", []tmuxSession{{Name: root}})
	if len(groups) != 1 {
		t.Fatalf("expected 1 group, got %d: %#v", len(groups), groups)
	}
	if groups[0].GroupID != "main" {
		t.Fatalf("GroupID = %q, want %q (pseudo-group would carry the full session name)", groups[0].GroupID, "main")
	}

	pane := NewGroupPaneSessionName("agent-1", groups[0].GroupID)
	groups = groupSessions("agent-1", []tmuxSession{{Name: root}, {Name: pane}})
	if len(groups) != 1 {
		t.Fatalf("split duplicated the group: %#v", groups)
	}
	if len(groups[0].Sessions) != 2 {
		t.Fatalf("group has %d sessions, want 2: %#v", len(groups[0].Sessions), groups[0])
	}
}

// TestSplitBindGroupID covers the guard on the split-key rebinding: a
// bare-named session's pseudo-group ID must never reach
// `fleet shell --group`, while a real group's ID passes through.
func TestSplitBindGroupID(t *testing.T) {
	if got := splitBindGroupID("agent-1", "agent-1~abc123", "abc123"); got != "abc123" {
		t.Fatalf("grouped session: got %q, want %q", got, "abc123")
	}
	if got := splitBindGroupID("agent-1", "agent-1~abc123~ff", "abc123"); got != "abc123" {
		t.Fatalf("grouped pane session: got %q, want %q", got, "abc123")
	}
	if got := splitBindGroupID("agent-1", "main", "main"); got != "" {
		t.Fatalf("bare session: got %q, want empty (pseudo-group ID must be stripped)", got)
	}
}
