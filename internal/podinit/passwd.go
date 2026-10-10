package podinit

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/BenjaminBenetti/fleet-man/internal/execas"
)

// podUserName names the entry pod-init appends for a uid the image does not
// know.
const podUserName = "fleet"

// passwdEntry is the /etc/passwd line pod-init appends for uid (with gid) when
// passwd has none: named "fleet" ("fleet-<uid>" when that name is taken),
// with remoteUser's home and shell, else / and /bin/sh. ok is false when
// passwd already has the uid.
func passwdEntry(passwd []byte, uid, gid int, remoteUser string) (line string, ok bool) {
	users, _ := execas.ParsePasswd(bytes.NewReader(passwd))
	name, home, shell := podUserName, "/", "/bin/sh"
	for _, u := range users {
		if u.UID == uid {
			return "", false
		}
		if u.Name == podUserName {
			name = podUserName + "-" + strconv.Itoa(uid)
		}
		if remoteUser != "" && u.Name == remoteUser {
			if u.Home != "" {
				home = u.Home
			}
			if u.Shell != "" {
				shell = u.Shell
			}
		}
	}
	return fmt.Sprintf("%s:x:%d:%d:fleet pod user:%s:%s", name, uid, gid, home, shell), true
}

// groupEntry is the /etc/group line for gid when group has none.
func groupEntry(group []byte, gid int) (line string, ok bool) {
	groups, _ := execas.ParseGroup(bytes.NewReader(group))
	name := podUserName
	for _, g := range groups {
		if g.GID == gid {
			return "", false
		}
		if g.Name == podUserName {
			name = podUserName + "-" + strconv.Itoa(gid)
		}
	}
	return fmt.Sprintf("%s:x:%d:", name, gid), true
}

// appendLine appends line to file as one write, after a newline when the file
// does not end in one.
func appendLine(file string, line string) error {
	data, err := os.ReadFile(file)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	f, err := os.OpenFile(file, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o644)
	if err != nil {
		return err
	}
	if len(data) > 0 && !strings.HasSuffix(string(data), "\n") {
		line = "\n" + line
	}
	_, err = f.WriteString(line + "\n")
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return err
}

// ensurePodUser appends the pod's uid (and gid) to passwd (and group) when the
// image has no entry for it, as OpenShift's arbitrary uids need: the shell,
// ssh and git look the user up. It returns what it appended.
func ensurePodUser(passwdFile, groupFile string, uid, gid int, remoteUser string) ([]string, error) {
	var added []string
	passwd, err := os.ReadFile(passwdFile)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	if line, ok := passwdEntry(passwd, uid, gid, remoteUser); ok {
		if err := appendLine(passwdFile, line); err != nil {
			return nil, fmt.Errorf("append the pod's uid %d to %s: %w", uid, passwdFile, err)
		}
		added = append(added, line)
	}
	group, err := os.ReadFile(groupFile)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return added, err
	}
	if line, ok := groupEntry(group, gid); ok {
		if err := appendLine(groupFile, line); err != nil {
			return added, fmt.Errorf("append the pod's gid %d to %s: %w", gid, groupFile, err)
		}
		added = append(added, line)
	}
	return added, nil
}
