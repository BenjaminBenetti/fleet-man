//go:build !nok8s

package kubernetes

import (
	"archive/tar"
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeGroupDocker is a docker that creates container cid-1 of any image and
// copies out etc/group from dir; every call goes to dir/calls.
func fakeGroupDocker(t *testing.T, group string) (docker, dir string) {
	t.Helper()
	dir = t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "etc"), 0o755); err != nil {
		t.Fatal(err)
	}
	if group != "" {
		if err := os.WriteFile(filepath.Join(dir, "etc", "group"), []byte(group), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	docker = filepath.Join(dir, "docker")
	script := `#!/bin/sh
echo "$*" >> "` + dir + `/calls"
case "$1" in
create) echo cid-1 ;;
cp) [ -f "` + dir + `/etc/group" ] || { echo "Could not find the file /etc/group in container cid-1" >&2; exit 1; }
    tar -C "` + dir + `/etc" -cf - group ;;
rm) ;;
esac
`
	if err := os.WriteFile(docker, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return docker, dir
}

// A named --group-add resolves against the image's /etc/group, read once
// from a container created for it (no pull, no network, never started) and
// removed after.
func TestImageGroups(t *testing.T) {
	docker, dir := fakeGroupDocker(t, "root:x:0:\ndocker:x:998:vscode\nusers:x:100:\ndocker:x:12:\n")
	var reported []error
	g := &imageGroups{docker: docker, image: "sha256:abc", platform: "linux/arm64", report: func(err error) { reported = append(reported, err) }}
	lookup := g.lookup(context.Background())
	for name, want := range map[string]int64{"docker": 998, "users": 100, "root": 0} {
		if gid, ok := lookup(name); !ok || gid != want {
			t.Errorf("%s = %d, %v; want %d", name, gid, ok, want)
		}
	}
	if _, ok := lookup("wheel"); ok {
		t.Error("a group the image does not name resolved")
	}
	calls, _ := os.ReadFile(filepath.Join(dir, "calls"))
	want := "create --pull=never --network=none --entrypoint=/fleet-never-runs --platform=linux/arm64 sha256:abc\ncp cid-1:/etc/group -\nrm -f cid-1\n"
	if string(calls) != want {
		t.Errorf("docker calls\n%s\nwant\n%s", calls, want)
	}
	if len(reported) != 0 {
		t.Errorf("reported %v", reported)
	}
}

// An image without /etc/group resolves no name, says why, and its container
// is still removed.
func TestImageGroupsWithoutTheFile(t *testing.T) {
	docker, dir := fakeGroupDocker(t, "")
	var reported []error
	g := &imageGroups{docker: docker, image: "img", report: func(err error) { reported = append(reported, err) }}
	lookup := g.lookup(context.Background())
	if _, ok := lookup("docker"); ok {
		t.Error("resolved without a group file")
	}
	lookup("users")
	if len(reported) != 1 || !strings.Contains(reported[0].Error(), "Could not find the file /etc/group") {
		t.Errorf("reported %v", reported)
	}
	calls, _ := os.ReadFile(filepath.Join(dir, "calls"))
	if !strings.HasSuffix(string(calls), "rm -f cid-1\n") || strings.Contains(string(calls), "--platform") {
		t.Errorf("docker calls %s", calls)
	}
}

// A link at /etc/group is not followed out of the stream.
func TestParseGroupTarRefusesALink(t *testing.T) {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	if err := tw.WriteHeader(&tar.Header{Name: "group", Typeflag: tar.TypeSymlink, Linkname: "/host/etc/group"}); err != nil {
		t.Fatal(err)
	}
	_ = tw.Close()
	if _, err := parseGroupTar(&buf); err == nil {
		t.Error("a link read as the group file")
	}
	if _, err := readImageGroups(context.Background(), "docker", "", ""); err == nil {
		t.Error("no image read")
	}
}
