package execas

import (
	"errors"
	"reflect"
	"strings"
	"testing"
)

// matrixPasswd and matrixGroup are the container /etc the identity tests run
// against: an image's own users, a remoteUser in several groups, and the uid
// pod-init appends on a no-root (tier C) cluster.
const matrixPasswd = `root:x:0:0:root:/root:/bin/bash
daemon:x:1:1:daemon:/usr/sbin:/usr/sbin/nologin
vscode:x:1000:1000:,,,:/home/vscode:/bin/zsh
node:x:1001:1001::/home/node:/bin/bash
svc:x:2000:2000::/srv/svc:/bin/sh
nohome:x:2100:2100:::/bin/sh
fleetpod:x:1000650000:0:fleet pod user:/home/fleetpod:/bin/sh
`

const matrixGroup = `root:x:0:
daemon:x:1:
tty:x:5:
sudo:x:27:vscode
docker:x:998:vscode,node
vscode:x:1000:
node:x:1001:
svc:x:2000:
media:x:3000:svc
`

func matrixDB(t *testing.T) *DB {
	t.Helper()
	db, err := LoadDB(writeRoot(t, matrixPasswd, matrixGroup))
	if err != nil {
		t.Fatal(err)
	}
	return db
}

func TestLookupFollowsDockerUserRules(t *testing.T) {
	db := matrixDB(t)
	for _, tc := range []struct {
		spec string
		want Identity
	}{
		{"vscode", Identity{UID: 1000, GID: 1000, Groups: []uint32{1000, 27, 998}, Name: "vscode", Home: "/home/vscode", Shell: "/bin/zsh"}},
		{"1000", Identity{UID: 1000, GID: 1000, Groups: []uint32{1000, 27, 998}, Name: "vscode", Home: "/home/vscode", Shell: "/bin/zsh"}},
		{"root", Identity{UID: 0, GID: 0, Groups: []uint32{0}, Name: "root", Home: "/root", Shell: "/bin/bash"}},
		{"0", Identity{UID: 0, GID: 0, Groups: []uint32{0}, Name: "root", Home: "/root", Shell: "/bin/bash"}},
		// A uid with no entry: gid 0, home /, no name.
		{"4242", Identity{UID: 4242, GID: 0, Groups: []uint32{0}, Home: "/"}},
		// An explicit group sets the primary group and is the only one.
		{"svc:media", Identity{UID: 2000, GID: 3000, Groups: []uint32{3000}, Name: "svc", Home: "/srv/svc", Shell: "/bin/sh"}},
		{"vscode:5", Identity{UID: 1000, GID: 5, Groups: []uint32{5}, Name: "vscode", Home: "/home/vscode", Shell: "/bin/zsh"}},
		{"4242:4343", Identity{UID: 4242, GID: 4343, Groups: []uint32{4343}, Home: "/"}},
		{"node:1000", Identity{UID: 1001, GID: 1000, Groups: []uint32{1000}, Name: "node", Home: "/home/node", Shell: "/bin/bash"}},
		// An entry with an empty home gets docker's /.
		{"nohome", Identity{UID: 2100, GID: 2100, Groups: []uint32{2100}, Name: "nohome", Home: "/", Shell: "/bin/sh"}},
	} {
		got, err := db.Lookup(tc.spec)
		if err != nil {
			t.Errorf("Lookup(%q): %v", tc.spec, err)
			continue
		}
		if !reflect.DeepEqual(got, tc.want) {
			t.Errorf("Lookup(%q):\n got %+v\nwant %+v", tc.spec, got, tc.want)
		}
	}
}

func TestLookupRefusesWhatDockerRefuses(t *testing.T) {
	db := matrixDB(t)
	for _, tc := range []struct {
		spec, want string
		is         error
	}{
		{"ghost", "unable to find user ghost: no matching entries in passwd file", ErrNoPasswdEntry},
		{"vscode:nogroup", "unable to find group nogroup: no matching entries in group file", ErrNoGroupEntry},
		{"", "invalid user", nil},
		{":1000", "invalid user", nil},
		{"vscode:", "empty group", nil},
		{"2147483648", "out of range", nil},
		{"vscode:99999999999", "out of range", nil},
	} {
		_, err := db.Lookup(tc.spec)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("Lookup(%q) = %v, want an error containing %q", tc.spec, err, tc.want)
		}
		if tc.is != nil && !errors.Is(err, tc.is) {
			t.Errorf("Lookup(%q) = %v, want it to wrap %v", tc.spec, err, tc.is)
		}
	}
}

func TestRuntimeGroupsAreWhatTheContainerAddedToItsOwnUser(t *testing.T) {
	db := matrixDB(t)
	// Root's own groups are just 0 here; 4000 (an fsGroup) and 998 came from
	// the runtime, even though 998 is a real group: root is not listed in it.
	got := db.runtimeGroups(Creds{UID: 0, GID: 0, Groups: []uint32{0, 4000, 998, 4000}})
	if want := []uint32{4000, 998}; !reflect.DeepEqual(got, want) {
		t.Fatalf("runtimeGroups = %v, want %v", got, want)
	}
	if got := db.runtimeGroups(Creds{UID: 1000, GID: 1000, Groups: []uint32{1000, 27, 998}}); len(got) != 0 {
		t.Fatalf("a process with only its own groups has no runtime extras, got %v", got)
	}
}

func TestIdentityOf(t *testing.T) {
	db := matrixDB(t)
	got := db.identityOf(Creds{UID: 1000650000, GID: 0, Groups: []uint32{0, 1000650000}})
	want := Identity{UID: 1000650000, GID: 0, Groups: []uint32{0, 1000650000}, Name: "fleetpod", Home: "/home/fleetpod", Shell: "/bin/sh"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("identityOf(pod uid):\n got %+v\nwant %+v", got, want)
	}
	got = db.identityOf(Creds{UID: 1000660000, GID: 0})
	if got.Name != "" || got.Home != "/" || got.UID != 1000660000 {
		t.Fatalf("identityOf(uid without entry) = %+v, want no name and home /", got)
	}
}
