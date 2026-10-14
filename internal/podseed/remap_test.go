package podseed

import (
	"strings"
	"testing"
)

const imagePasswd = `root:x:0:0:root:/root:/bin/bash
daemon:x:1:1:daemon:/usr/sbin:/usr/sbin/nologin
vscode:x:1000:1000:,,,:/home/vscode:/bin/zsh
myvscode:x:1001:1001::/home/myvscode:/bin/sh
`

const imageGroup = `root:x:0:
docker:x:999:vscode
vscode:x:1000:
myvscode:x:1001:
`

func TestRemapUserFollowsTheCLIsUIDLayer(t *testing.T) {
	cases := []struct {
		name                string
		user                string
		uid, gid            int
		changed             bool
		wantUID, wantGID    int
		note                string
		passwdHas, groupHas []string
	}{
		{name: "remap both", user: "vscode", uid: 1234, gid: 1234, changed: true, wantUID: 1234, wantGID: 1234,
			passwdHas: []string{"vscode:x:1234:1234:,,,:/home/vscode:/bin/zsh", "myvscode:x:1001:1001:"},
			groupHas:  []string{"vscode:x:1234:", "docker:x:999:vscode", "myvscode:x:1001:"}},
		{name: "same ids", user: "vscode", uid: 1000, gid: 1000, wantUID: 1000, wantGID: 1000, note: "UIDs and GIDs are the same (1000:1000)."},
		{name: "missing user", user: "node", uid: 1000, gid: 1000, wantUID: 1000, wantGID: 1000, note: "Remote user not found in /etc/passwd (node)."},
		{name: "uid taken", user: "vscode", uid: 1001, gid: 1500, wantUID: 1001, wantGID: 1500, note: "User with UID exists (myvscode=1001)."},
		{name: "gid taken keeps the group", user: "vscode", uid: 1500, gid: 999, changed: true, wantUID: 1500, wantGID: 1000,
			note:      "Group with GID exists (docker=999).",
			passwdHas: []string{"vscode:x:1500:1000:"}, groupHas: []string{"vscode:x:1000:", "docker:x:999:vscode"}},
		{name: "only the gid", user: "vscode", uid: 1000, gid: 2000, changed: true, wantUID: 1000, wantGID: 2000,
			passwdHas: []string{"vscode:x:1000:2000:"}, groupHas: []string{"vscode:x:2000:"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p, g, res := RemapUser([]byte(imagePasswd), []byte(imageGroup), tc.user, tc.uid, tc.gid)
			if res.Changed != tc.changed || res.UID != tc.wantUID || res.GID != tc.wantGID || res.Note != tc.note {
				t.Fatalf("result %+v", res)
			}
			if !tc.changed {
				if string(p) != imagePasswd || string(g) != imageGroup {
					t.Fatal("an unchanged remap rewrote the files")
				}
				return
			}
			if res.FromUID != 1000 || res.FromGID != 1000 || res.Home != "/home/vscode" {
				t.Fatalf("result %+v", res)
			}
			for _, want := range tc.passwdHas {
				if !strings.Contains(string(p), want) {
					t.Errorf("passwd lacks %q:\n%s", want, p)
				}
			}
			for _, want := range tc.groupHas {
				if !strings.Contains(string(g), want) {
					t.Errorf("group lacks %q:\n%s", want, g)
				}
			}
			if !strings.HasPrefix(string(p), "root:x:0:0:root:/root:/bin/bash\n") || !strings.HasSuffix(string(p), "\n") {
				t.Errorf("untouched lines changed:\n%s", p)
			}
		})
	}
}

func TestRemapUserMatchesFieldsExactly(t *testing.T) {
	// The CLI's unanchored sed would read a gid of 1234 as a user with uid
	// 1234; field matching does not.
	passwd := "vscode:x:1000:1000::/home/vscode:/bin/sh\nother:x:5:1234::/:/bin/sh\n"
	p, _, res := RemapUser([]byte(passwd), []byte("vscode:x:1000:\n"), "vscode", 1234, 1234)
	if !res.Changed || !strings.Contains(string(p), "vscode:x:1234:1234:") || !strings.Contains(string(p), "other:x:5:1234:") {
		t.Fatalf("%+v\n%s", res, p)
	}
	// No trailing newline in, none out.
	p, _, _ = RemapUser([]byte("vscode:x:1:1::/h:/bin/sh"), nil, "vscode", 2, 2)
	if string(p) != "vscode:x:2:2::/h:/bin/sh" {
		t.Fatalf("%q", p)
	}
}
