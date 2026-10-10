package execas

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestParsePasswd(t *testing.T) {
	in := strings.Join([]string{
		"root:x:0:0:root:/root:/bin/bash",
		"",
		"# a comment",
		"+nisuser::::::",
		"-excluded::::::",
		"vscode:x:1000:1000:,,,:/home/vscode:/bin/zsh",
		"  short:x:7:8  ",                  // missing gecos, home and shell
		"badid:x:abc:1::/:/bin/sh",         // not a number: skipped
		"neg:x:-1:1::/:/bin/sh",            // negative: skipped
		"big:x:2147483648:1::/:/bin/sh",    // past docker's range: skipped
		"plus:x:+5:1::/:/bin/sh",           // a sign is not a number
		"lead:x:007:0::/:/bin/sh",          // leading zeros are
		":x:9:9::/:/bin/sh",                // no name
		"few:x:3",                          // too few fields
		"vscode:x:1001:1001::/dup:/bin/sh", // a later duplicate is kept; lookups take the first
	}, "\n")
	got, err := ParsePasswd(strings.NewReader(in))
	if err != nil {
		t.Fatal(err)
	}
	want := []User{
		{Name: "root", UID: 0, GID: 0, Gecos: "root", Home: "/root", Shell: "/bin/bash"},
		{Name: "vscode", UID: 1000, GID: 1000, Gecos: ",,,", Home: "/home/vscode", Shell: "/bin/zsh"},
		{Name: "short", UID: 7, GID: 8},
		{Name: "lead", UID: 7, GID: 0, Home: "/", Shell: "/bin/sh"},
		{Name: "vscode", UID: 1001, GID: 1001, Home: "/dup", Shell: "/bin/sh"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ParsePasswd:\n got %+v\nwant %+v", got, want)
	}
}

func TestParseGroup(t *testing.T) {
	in := strings.Join([]string{
		"root:x:0:",
		"sudo:x:27:vscode",
		"docker:x:998:vscode, node,,",
		"nomembers:x:5",
		"bad:x:nope:vscode",
		"#comment:x:1:vscode",
	}, "\n")
	got, err := ParseGroup(strings.NewReader(in))
	if err != nil {
		t.Fatal(err)
	}
	want := []Group{
		{Name: "root", GID: 0},
		{Name: "sudo", GID: 27, Members: []string{"vscode"}},
		{Name: "docker", GID: 998, Members: []string{"vscode", "node"}},
		{Name: "nomembers", GID: 5},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ParseGroup:\n got %+v\nwant %+v", got, want)
	}
}

func TestParsePasswdRefusesAnOverlongLine(t *testing.T) {
	line := "x:x:1:1::/:" + strings.Repeat("a", maxIDLine)
	if _, err := ParsePasswd(strings.NewReader(line)); err == nil {
		t.Fatal("a line past the cap should be an error, not a silent truncation")
	}
}

func TestLoadDBTreatsMissingFilesAsEmpty(t *testing.T) {
	db, err := LoadDB(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if len(db.Users) != 0 || len(db.Groups) != 0 {
		t.Fatalf("empty root: got %+v", db)
	}
}

func TestLoadDBReadsBothFiles(t *testing.T) {
	root := writeRoot(t, "a:x:1:2::/a:/bin/sh\n", "g:x:2:a\n")
	db, err := LoadDB(root)
	if err != nil {
		t.Fatal(err)
	}
	if u, ok := db.UserByName("a"); !ok || u.UID != 1 {
		t.Fatalf("UserByName(a) = %+v, %v", u, ok)
	}
	if g, ok := db.GroupByGID(2); !ok || g.Name != "g" {
		t.Fatalf("GroupByGID(2) = %+v, %v", g, ok)
	}
}

func TestLoadDBReportsAnUnreadableFile(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "etc", "passwd"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadDB(root); err == nil {
		t.Fatal("a passwd that is a directory should be an error")
	}
}

func TestInitGroups(t *testing.T) {
	db := &DB{Groups: []Group{
		{Name: "vscode", GID: 1000},
		{Name: "sudo", GID: 27, Members: []string{"vscode"}},
		{Name: "docker", GID: 998, Members: []string{"node", "vscode"}},
		{Name: "again", GID: 27, Members: []string{"vscode"}}, // same gid twice: listed once
		{Name: "primary", GID: 1000, Members: []string{"vscode"}},
		{Name: "other", GID: 5, Members: []string{"node"}},
	}}
	if got, want := db.InitGroups("vscode", 1000), []uint32{1000, 27, 998}; !reflect.DeepEqual(got, want) {
		t.Fatalf("InitGroups(vscode) = %v, want %v", got, want)
	}
	if got, want := db.InitGroups("", 0), []uint32{0}; !reflect.DeepEqual(got, want) {
		t.Fatalf("InitGroups of a nameless uid = %v, want %v", got, want)
	}
}

// writeRoot is a directory with etc/passwd and etc/group, for LoadDB.
func writeRoot(t *testing.T, passwd, group string) string {
	t.Helper()
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "etc"), 0o755); err != nil {
		t.Fatal(err)
	}
	for name, data := range map[string]string{"passwd": passwd, "group": group} {
		if err := os.WriteFile(filepath.Join(root, "etc", name), []byte(data), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func FuzzParsePasswd(f *testing.F) {
	f.Add("root:x:0:0:root:/root:/bin/bash\nvscode:x:1000:1000::/home/vscode:/bin/zsh\n")
	f.Add("a:b\n:::\n\x00:x:1:1\n")
	f.Fuzz(func(t *testing.T, in string) {
		users, err := ParsePasswd(strings.NewReader(in))
		if err != nil {
			return
		}
		for _, u := range users {
			if u.Name == "" || u.UID < 0 || u.UID > maxID || u.GID < 0 || u.GID > maxID {
				t.Fatalf("parsed an entry that should have been skipped: %+v", u)
			}
		}
	})
}

func FuzzParseGroup(f *testing.F) {
	f.Add("docker:x:998:vscode,node\nroot:x:0:\n")
	f.Fuzz(func(t *testing.T, in string) {
		groups, err := ParseGroup(strings.NewReader(in))
		if err != nil {
			return
		}
		for _, g := range groups {
			if g.Name == "" || g.GID < 0 || g.GID > maxID {
				t.Fatalf("parsed a group that should have been skipped: %+v", g)
			}
			for _, m := range g.Members {
				if m == "" {
					t.Fatalf("empty member in %+v", g)
				}
			}
		}
	})
}
