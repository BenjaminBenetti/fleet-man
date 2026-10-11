//go:build linux

package podseed

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestNestedMounts(t *testing.T) {
	info := filepath.Join(t.TempDir(), "mountinfo")
	if err := os.WriteFile(info, []byte(`22 1 8:1 / / rw - ext4 /dev/sda1 rw
30 22 8:1 /ws /workspaces/repo rw - ext4 /dev/sda1 rw
31 30 8:1 /vol/nm /workspaces/repo/node_modules rw - ext4 /dev/sda1 rw
32 30 0:5 / /workspaces/repo/with\040space rw - tmpfs tmpfs rw
33 22 8:1 /other /workspaces/repo-other rw - ext4 /dev/sda1 rw
`), 0o644); err != nil {
		t.Fatal(err)
	}
	old := mountInfo
	mountInfo = info
	t.Cleanup(func() { mountInfo = old })
	got, err := NestedMounts("/workspaces/repo")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"/workspaces/repo/node_modules", "/workspaces/repo/with space"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("NestedMounts = %q, want %q", got, want)
	}
}

func TestDiskUsage(t *testing.T) {
	d, err := DiskUsage(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if d.Total <= 0 || d.Free < 0 || d.Free > d.Total {
		t.Errorf("DiskUsage = %+v", d)
	}
}
