package execas

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// User is one /etc/passwd entry.
type User struct {
	Name  string
	UID   int
	GID   int
	Gecos string
	Home  string
	Shell string
}

// Group is one /etc/group entry.
type Group struct {
	Name    string
	GID     int
	Members []string
}

// maxIDLine caps one passwd or group line (a group with thousands of members
// is a long line).
const maxIDLine = 1 << 20

// maxID is the largest uid or gid docker accepts.
const maxID = math.MaxInt32

// ParsePasswd reads /etc/passwd entries from r. Blank lines, comments, NIS
// compat entries (+/-) and lines whose uid or gid is not a number are skipped,
// so one bad line costs only itself.
func ParsePasswd(r io.Reader) ([]User, error) {
	var users []User
	err := scanIDFile(r, func(fields []string) {
		if len(fields) < 4 || fields[0] == "" {
			return
		}
		uid, ok1 := parseID(fields[2])
		gid, ok2 := parseID(fields[3])
		if !ok1 || !ok2 {
			return
		}
		u := User{Name: fields[0], UID: uid, GID: gid}
		if len(fields) > 4 {
			u.Gecos = fields[4]
		}
		if len(fields) > 5 {
			u.Home = fields[5]
		}
		if len(fields) > 6 {
			u.Shell = fields[6]
		}
		users = append(users, u)
	})
	return users, err
}

// ParseGroup reads /etc/group entries from r, skipping what ParsePasswd
// skips.
func ParseGroup(r io.Reader) ([]Group, error) {
	var groups []Group
	err := scanIDFile(r, func(fields []string) {
		if len(fields) < 3 || fields[0] == "" {
			return
		}
		gid, ok := parseID(fields[2])
		if !ok {
			return
		}
		g := Group{Name: fields[0], GID: gid}
		if len(fields) > 3 {
			for member := range strings.SplitSeq(fields[3], ",") {
				if member = strings.TrimSpace(member); member != "" {
					g.Members = append(g.Members, member)
				}
			}
		}
		groups = append(groups, g)
	})
	return groups, err
}

// scanIDFile calls fn with the colon-separated fields of each entry line.
func scanIDFile(r io.Reader, fn func(fields []string)) error {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64<<10), maxIDLine)
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 || line[0] == '#' || line[0] == '+' || line[0] == '-' {
			continue
		}
		fn(strings.Split(string(line), ":"))
	}
	return sc.Err()
}

// parseID parses a uid or gid: decimal digits only, in docker's range.
func parseID(s string) (int, bool) {
	if !isNumber(s) {
		return 0, false
	}
	n, err := strconv.Atoi(s)
	if err != nil || n > maxID {
		return 0, false
	}
	return n, true
}

// DB is a container's user and group database, read from its own files.
type DB struct {
	Users  []User
	Groups []Group
}

// LoadDB reads etc/passwd and etc/group under root ("/" in a pod). A missing
// file is an empty database, as docker treats it.
func LoadDB(root string) (*DB, error) {
	db := &DB{}
	var err error
	if db.Users, err = parseIDFile(filepath.Join(root, "etc", "passwd"), ParsePasswd); err != nil {
		return nil, err
	}
	if db.Groups, err = parseIDFile(filepath.Join(root, "etc", "group"), ParseGroup); err != nil {
		return nil, err
	}
	return db, nil
}

func parseIDFile[T any](file string, parse func(io.Reader) ([]T, error)) ([]T, error) {
	f, err := os.Open(file)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	entries, err := parse(f)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", file, err)
	}
	return entries, nil
}

// UserByName is the first entry named name.
func (db *DB) UserByName(name string) (User, bool) {
	for _, u := range db.Users {
		if u.Name == name {
			return u, true
		}
	}
	return User{}, false
}

// UserByUID is the first entry with uid.
func (db *DB) UserByUID(uid int) (User, bool) {
	for _, u := range db.Users {
		if u.UID == uid {
			return u, true
		}
	}
	return User{}, false
}

// GroupByName is the first group named name.
func (db *DB) GroupByName(name string) (Group, bool) {
	for _, g := range db.Groups {
		if g.Name == name {
			return g, true
		}
	}
	return Group{}, false
}

// GroupByGID is the first group with gid.
func (db *DB) GroupByGID(gid int) (Group, bool) {
	for _, g := range db.Groups {
		if g.GID == gid {
			return g, true
		}
	}
	return Group{}, false
}

// InitGroups is the supplementary group list initgroups(3) gives user with
// primary group gid: gid first, then every group listing user as a member, in
// file order, each once. An empty user (a uid with no passwd entry) gets gid
// alone.
func (db *DB) InitGroups(user string, gid int) []uint32 {
	groups := []uint32{uint32(gid)}
	seen := map[int]bool{gid: true}
	if user == "" {
		return groups
	}
	for _, g := range db.Groups {
		if seen[g.GID] {
			continue
		}
		for _, member := range g.Members {
			if member == user {
				seen[g.GID] = true
				groups = append(groups, uint32(g.GID))
				break
			}
		}
	}
	return groups
}
