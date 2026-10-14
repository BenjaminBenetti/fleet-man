package execas

import (
	"errors"
	"fmt"
	"slices"
	"strings"
)

// Identity is whom a command runs as.
type Identity struct {
	UID uint32
	GID uint32
	// Groups is the supplementary group list, as setgroups(2) takes it.
	Groups []uint32
	// Name is the passwd entry's name; "" for a uid without one.
	Name string
	// Home is the entry's home directory; docker's "/" without one.
	Home string
	// Shell is the entry's login shell; "" without one.
	Shell string
}

// Creds are a process's own uid, gid and supplementary groups.
type Creds struct {
	UID    uint32
	GID    uint32
	Groups []uint32
}

// Errors Lookup wraps, docker's wording.
var (
	ErrNoPasswdEntry = errors.New("no matching entries in passwd file")
	ErrNoGroupEntry  = errors.New("no matching entries in group file")
)

// Lookup resolves a docker -u spec ("name", "uid", "name:group", "uid:gid" and
// mixes) the way `docker exec -u` does: a name must exist; a number that has
// no passwd entry runs as that uid with gid 0 and home "/"; an explicit group
// sets the primary group, which must exist when named. Supplementary groups
// follow initgroups(3) for a matched name (an explicit group gets none but
// itself).
func (db *DB) Lookup(spec string) (Identity, error) {
	userArg, groupArg, hasGroup := strings.Cut(spec, ":")
	if userArg == "" {
		return Identity{}, fmt.Errorf("invalid user %q", spec)
	}
	if hasGroup && groupArg == "" {
		return Identity{}, fmt.Errorf("invalid user %q: empty group", spec)
	}
	uidArg, uidNumeric := parseID(userArg)
	if !uidNumeric && isNumber(userArg) {
		return Identity{}, fmt.Errorf("uid %s is out of range 0-%d", userArg, maxID)
	}

	id := Identity{Home: "/"}
	var u User
	var found bool
	if uidNumeric {
		u, found = db.UserByUID(uidArg)
	} else {
		u, found = db.UserByName(userArg)
	}
	switch {
	case found:
		id.UID, id.GID = uint32(u.UID), uint32(u.GID)
		id.Name, id.Shell = u.Name, u.Shell
		if u.Home != "" {
			id.Home = u.Home
		}
	case uidNumeric:
		id.UID = uint32(uidArg)
	default:
		return Identity{}, fmt.Errorf("unable to find user %s: %w", userArg, ErrNoPasswdEntry)
	}

	if !hasGroup {
		id.Groups = db.InitGroups(id.Name, int(id.GID))
		return id, nil
	}
	gidArg, gidNumeric := parseID(groupArg)
	if !gidNumeric && isNumber(groupArg) {
		return Identity{}, fmt.Errorf("gid %s is out of range 0-%d", groupArg, maxID)
	}
	var g Group
	if gidNumeric {
		g, found = db.GroupByGID(gidArg)
	} else {
		g, found = db.GroupByName(groupArg)
	}
	switch {
	case found:
		id.GID = uint32(g.GID)
	case gidNumeric:
		id.GID = uint32(gidArg)
	default:
		return Identity{}, fmt.Errorf("unable to find group %s: %w", groupArg, ErrNoGroupEntry)
	}
	id.Groups = []uint32{id.GID}
	return id, nil
}

// identityOf is the Identity a process with creds has: its own ids and groups,
// with the name, home and shell of its uid's passwd entry if there is one.
func (db *DB) identityOf(creds Creds) Identity {
	id := Identity{UID: creds.UID, GID: creds.GID, Groups: slices.Clone(creds.Groups), Home: "/"}
	if u, ok := db.UserByUID(int(creds.UID)); ok {
		id.Name, id.Shell = u.Name, u.Shell
		if u.Home != "" {
			id.Home = u.Home
		}
	}
	return id
}

// runtimeGroups are the supplementary groups the container runtime gave the
// whole container on top of a user's own (the pod's fsGroup and
// supplementalGroups, docker's --group-add): the groups this process has
// beyond what initgroups would give its own user. docker keeps them for every
// exec user, so execas does too.
func (db *DB) runtimeGroups(self Creds) []uint32 {
	own := db.InitGroups(db.identityOf(self).Name, int(self.GID))
	var extra []uint32
	for _, g := range self.Groups {
		if !slices.Contains(own, g) && !slices.Contains(extra, g) {
			extra = append(extra, g)
		}
	}
	return extra
}

// isNumber reports whether s is all ASCII digits (a uid that failed parseID
// for its range, not for being a name).
func isNumber(s string) bool {
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return s != ""
}
