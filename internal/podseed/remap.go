package podseed

import (
	"bytes"
	"fmt"
	"strconv"
	"strings"
)

// RemapResult is what the share-uid remap did to remoteUser.
type RemapResult struct {
	User    string `json:"user"`
	FromUID int    `json:"from_uid"`
	FromGID int    `json:"from_gid"`
	UID     int    `json:"uid"`
	GID     int    `json:"gid"`
	// Home is the user's home dir, chowned when Changed.
	Home string `json:"home,omitempty"`
	// Changed is whether passwd (and maybe group) were rewritten.
	Changed bool `json:"changed"`
	// Note says why nothing changed, or that the group kept its gid; the
	// devcontainer CLI's own wording.
	Note string `json:"note,omitempty"`
}

// RemapUser is the devcontainer CLI's updateUID.Dockerfile, applied to the
// text of an image's /etc/passwd and /etc/group: remoteUser user takes uid
// and gid, and every group with the user's old gid moves to the new one. Like
// the CLI it changes nothing when the user is missing, already has both ids,
// or another user holds uid; and when another group holds gid, the user keeps
// its own gid. Unlike the CLI's sed it matches fields exactly, so a user whose
// name merely ends in user's, or a gid equal to uid, is never touched. Lines
// it does not change are kept byte for byte.
func RemapUser(passwd, group []byte, user string, uid, gid int) (newPasswd, newGroup []byte, res RemapResult) {
	res = RemapResult{User: user, FromUID: -1, FromGID: -1, UID: uid, GID: gid}
	pLines := splitLines(passwd)
	gLines := splitLines(group)

	found := false
	existingUser := ""
	for _, line := range pLines {
		f := strings.Split(line, ":")
		if len(f) < 4 {
			continue
		}
		if f[0] == user {
			if u, err := strconv.Atoi(f[2]); err == nil {
				res.FromUID = u
			}
			if g, err := strconv.Atoi(f[3]); err == nil {
				res.FromGID = g
			}
			if len(f) > 5 {
				res.Home = f[5]
			}
			found = true
		}
		if f[2] == strconv.Itoa(uid) {
			existingUser = f[0]
		}
	}
	existingGroup := ""
	for _, line := range gLines {
		if f := strings.Split(line, ":"); len(f) >= 3 && f[2] == strconv.Itoa(gid) {
			existingGroup = f[0]
		}
	}

	switch {
	case !found:
		res.Note = fmt.Sprintf("Remote user not found in /etc/passwd (%s).", user)
		return passwd, group, res
	case res.FromUID == uid && res.FromGID == gid:
		res.Note = fmt.Sprintf("UIDs and GIDs are the same (%d:%d).", uid, gid)
		return passwd, group, res
	case res.FromUID != uid && existingUser != "":
		res.Note = fmt.Sprintf("User with UID exists (%s=%d).", existingUser, uid)
		return passwd, group, res
	}
	if res.FromGID != gid && existingGroup != "" {
		res.Note = fmt.Sprintf("Group with GID exists (%s=%d).", existingGroup, gid)
		res.GID = res.FromGID
	}
	res.Changed = true

	for i, line := range pLines {
		f := strings.Split(line, ":")
		if len(f) >= 4 && f[0] == user {
			f[2], f[3] = strconv.Itoa(res.UID), strconv.Itoa(res.GID)
			pLines[i] = strings.Join(f, ":")
		}
	}
	if res.FromGID != res.GID {
		for i, line := range gLines {
			f := strings.Split(line, ":")
			if len(f) >= 3 && f[2] == strconv.Itoa(res.FromGID) {
				f[2] = strconv.Itoa(res.GID)
				gLines[i] = strings.Join(f, ":")
			}
		}
	}
	return joinLines(pLines, passwd), joinLines(gLines, group), res
}

// splitLines splits text into lines without their newlines.
func splitLines(text []byte) []string {
	s := strings.TrimSuffix(string(text), "\n")
	if s == "" {
		return nil
	}
	return strings.Split(s, "\n")
}

// joinLines joins lines back, ending in a newline when orig did.
func joinLines(lines []string, orig []byte) []byte {
	out := []byte(strings.Join(lines, "\n"))
	if len(lines) > 0 && (len(orig) == 0 || bytes.HasSuffix(orig, []byte("\n"))) {
		out = append(out, '\n')
	}
	return out
}
