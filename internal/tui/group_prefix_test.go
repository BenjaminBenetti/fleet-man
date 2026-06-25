package tui

import (
	"strings"
	"testing"
)

// These tests pin down the group-boundary bug: a session group's ID can be a
// string prefix of a sibling group's ID (the user creates "dog" then "dog-2"),
// and every group operation that filtered membership with a raw
// strings.HasPrefix(name, "<inst>~<gid>") wrongly swallowed the sibling. The
// reported symptom was opening "dog" showing the panes of both "dog" and
// "dog-2"; the same flaw also made deleting/renaming "dog" hit "dog-2".

// fourSessions is the live session list for an instance "alpha" that has two
// sibling groups, "dog" and "dog-2", each with one extra split pane — exactly
// the ps-output shape from the bug report.
var fourSessions = []tmuxSession{
	{Name: "alpha~dog"},
	{Name: "alpha~dog~ff00"},
	{Name: "alpha~dog-2"},
	{Name: "alpha~dog-2~1177"},
}

func TestSessionInGroupBoundary(t *testing.T) {
	cases := []struct {
		name    string
		session string
		groupID string
		want    bool
	}{
		{"root matches own group", "alpha~dog", "dog", true},
		{"pane matches own group", "alpha~dog~ff00", "dog", true},
		{"sibling root not in shorter group", "alpha~dog-2", "dog", false},
		{"sibling pane not in shorter group", "alpha~dog-2~1177", "dog", false},
		{"longer group owns its own root", "alpha~dog-2", "dog-2", true},
		{"longer group owns its own pane", "alpha~dog-2~1177", "dog-2", true},
		{"shorter root not in longer group", "alpha~dog", "dog-2", false},
		{"other instance never matches", "beta~dog", "dog", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := sessionInGroup("alpha", tc.session, tc.groupID); got != tc.want {
				t.Fatalf("sessionInGroup(alpha, %q, %q) = %v, want %v", tc.session, tc.groupID, got, tc.want)
			}
		})
	}
}

// TestRestoreSessionNamesExcludesSiblingPrefixGroup is the direct unit-level
// repro of the visible bug: restoring "dog" must produce panes only for "dog",
// never the "dog-2" sessions whose names start with "alpha~dog".
func TestRestoreSessionNamesExcludesSiblingPrefixGroup(t *testing.T) {
	discovered := "alpha~dog\nalpha~dog~ff00\nalpha~dog-2\nalpha~dog-2~1177\n"

	got := restoreSessionNames(discovered, "dog", nil, nil, "alpha")
	want := []string{"alpha~dog", "alpha~dog~ff00"}
	if len(got) != len(want) {
		t.Fatalf("restored %d sessions %#v, want %d %#v", len(got), got, len(want), want)
	}
	for _, name := range got {
		if strings.HasPrefix(name, "alpha~dog-2") {
			t.Fatalf("sibling group session leaked into the restore: %q (full set %#v)", name, got)
		}
	}
}

func TestSessionStillExistsBoundary(t *testing.T) {
	cases := []struct {
		name string
		last lastSession
		sess []tmuxSession
		want bool
	}{
		{
			name: "dead group not kept alive by a sibling prefix group",
			last: lastSession{sessionName: "alpha~dog", groupID: "dog"},
			sess: []tmuxSession{{Name: "alpha~dog-2"}, {Name: "alpha~dog-2~1177"}},
			want: false,
		},
		{
			name: "group alive via a surviving pane after the root is killed",
			last: lastSession{sessionName: "alpha~dog", groupID: "dog"},
			sess: []tmuxSession{{Name: "alpha~dog~ff00"}},
			want: true,
		},
		{
			name: "group alive when its own root is present",
			last: lastSession{sessionName: "alpha~dog", groupID: "dog"},
			sess: fourSessions,
			want: true,
		},
		{
			name: "ad-hoc bare session kept by exact-name fallthrough",
			last: lastSession{sessionName: "foo", groupID: "foo"},
			sess: []tmuxSession{{Name: "foo"}},
			want: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := sessionStillExists("alpha", tc.last, tc.sess); got != tc.want {
				t.Fatalf("sessionStillExists = %v, want %v", got, tc.want)
			}
		})
	}
}

// stubGroupSessionList makes runInstanceCommand answer a `tmux list-sessions`
// with the four-session sibling fixture and records every other tmux script the
// command runs, so a test can assert which sessions a group op touched.
func stubGroupSessionList(t *testing.T) *[]string {
	t.Helper()
	orig := runInstanceCommand
	t.Cleanup(func() { runInstanceCommand = orig })

	ran := &[]string{}
	runInstanceCommand = func(_, _ string, argv []string) (string, int, error) {
		script := argv[len(argv)-1]
		if strings.Contains(script, "list-sessions") {
			return "alpha~dog\nalpha~dog~ff00\nalpha~dog-2\nalpha~dog-2~1177\n", 0, nil
		}
		*ran = append(*ran, script)
		return "", 0, nil
	}
	return ran
}

// TestDeleteGroupSessionsCmdExcludesSiblingPrefixGroup proves the data-loss
// variant of the bug is gone: deleting "dog" kills only the "dog" sessions and
// leaves "dog-2" untouched.
func TestDeleteGroupSessionsCmdExcludesSiblingPrefixGroup(t *testing.T) {
	ran := stubGroupSessionList(t)

	cmd := deleteGroupSessionsCmd(InstanceRef{Fleet: "f", Instance: "alpha"}, "alpha", "dog")
	msg, ok := cmd().(sessionDeletedMsg)
	if !ok {
		t.Fatalf("expected sessionDeletedMsg")
	}
	if msg.err != nil {
		t.Fatalf("unexpected error: %v", msg.err)
	}

	joined := strings.Join(*ran, "\n")
	for _, want := range []string{shQuote("alpha~dog"), shQuote("alpha~dog~ff00")} {
		if !strings.Contains(joined, want) {
			t.Fatalf("expected %s to be killed; scripts: %v", want, *ran)
		}
	}
	for _, forbidden := range []string{shQuote("alpha~dog-2"), shQuote("alpha~dog-2~1177")} {
		if strings.Contains(joined, forbidden) {
			t.Fatalf("sibling group session %s was wrongly killed; scripts: %v", forbidden, *ran)
		}
	}
}

// TestRenameGroupCmdExcludesSiblingPrefixGroup proves renaming "dog" reprefixes
// only the "dog" sessions and never touches "dog-2".
func TestRenameGroupCmdExcludesSiblingPrefixGroup(t *testing.T) {
	ran := stubGroupSessionList(t)

	cmd := renameGroupCmd(InstanceRef{Fleet: "f", Instance: "alpha"}, "alpha", "dog", "renamed")
	msg, ok := cmd().(sessionRenamedMsg)
	if !ok {
		t.Fatalf("expected sessionRenamedMsg")
	}
	if msg.err != nil {
		t.Fatalf("unexpected error: %v", msg.err)
	}

	joined := strings.Join(*ran, "\n")
	// dog's root and pane get reprefixed to the new group ID.
	for _, want := range []string{shQuote("alpha~dog"), shQuote("alpha~renamed"), shQuote("alpha~renamed~ff00")} {
		if !strings.Contains(joined, want) {
			t.Fatalf("expected %s in a rename; scripts: %v", want, *ran)
		}
	}
	// dog-2 must not be a rename source.
	for _, forbidden := range []string{shQuote("alpha~dog-2"), shQuote("alpha~dog-2~1177")} {
		if strings.Contains(joined, forbidden) {
			t.Fatalf("sibling group session %s was wrongly renamed; scripts: %v", forbidden, *ran)
		}
	}
}
