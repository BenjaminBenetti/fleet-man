//go:build !nok8s

package kubernetes

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/BenjaminBenetti/fleet-man/internal/backend/kubernetes/kubeimage"
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

type anonymous struct{}

func (anonymous) Lookup(context.Context, string) (kubeimage.Credential, error) {
	return kubeimage.Credential{}, nil
}

type roundTripper func(*http.Request) (*http.Response, error)

func (f roundTripper) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// groupRegistry serves registry.test/team/base:1, an image of one gzipped
// layer holding etc/group (none when group is ""), and returns options that
// reach it.
func groupRegistry(t *testing.T, group string) kubeimage.RemoteOptions {
	t.Helper()
	var layer bytes.Buffer
	gz := gzip.NewWriter(&layer)
	tw := tar.NewWriter(gz)
	if group != "" {
		if err := tw.WriteHeader(&tar.Header{Name: "etc/group", Typeflag: tar.TypeReg, Mode: 0o644, Size: int64(len(group))}); err != nil {
			t.Fatal(err)
		}
		_, _ = tw.Write([]byte(group))
	}
	_ = tw.Close()
	_ = gz.Close()
	digest := func(b []byte) string { return fmt.Sprintf("sha256:%x", sha256.Sum256(b)) }
	cfg := []byte(`{"architecture":"amd64","os":"linux","config":{}}`)
	manifest, _ := json.Marshal(map[string]any{"schemaVersion": 2, "mediaType": "application/vnd.oci.image.manifest.v1+json",
		"config": map[string]any{"digest": digest(cfg), "size": len(cfg)},
		"layers": []any{map[string]any{"mediaType": "application/vnd.oci.image.layer.v1.tar+gzip", "digest": digest(layer.Bytes()), "size": layer.Len()}}})
	blobs := map[string][]byte{digest(cfg): cfg, digest(layer.Bytes()): layer.Bytes()}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := strings.TrimPrefix(r.URL.Path, "/v2/team/base/")
		switch blob, ok := blobs[strings.TrimPrefix(p, "blobs/")]; {
		case p == "manifests/1":
			w.Header().Set("Content-Type", "application/vnd.oci.image.manifest.v1+json")
			_, _ = w.Write(manifest)
		case ok:
			_, _ = w.Write(blob)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	host := strings.TrimPrefix(srv.URL, "http://")
	return kubeimage.RemoteOptions{Credentials: anonymous{}, Transport: roundTripper(func(r *http.Request) (*http.Response, error) {
		out := r.Clone(r.Context())
		out.URL.Scheme, out.URL.Host, out.Host = "http", host, host
		return http.DefaultTransport.RoundTrip(out)
	})}
}

// An image the daemon host has not got (capture-up read it from its
// registry) has its /etc/group read from the registry; docker is never
// asked.
func TestImageGroupsFromTheRegistry(t *testing.T) {
	docker, dir := fakeGroupDocker(t, "users:x:7:\n")
	var reported []error
	report := func(err error) { reported = append(reported, err) }
	g := &imageGroups{docker: docker, image: "sha256:abc", platform: "linux/amd64", remote: "registry.test/team/base:1",
		registry: groupRegistry(t, "root:x:0:\nusers:x:100:\n"), report: report}
	lookup := g.lookup(context.Background())
	if gid, ok := lookup("users"); !ok || gid != 100 {
		t.Errorf("users = %d, %v; want the registry image's 100", gid, ok)
	}
	if _, ok := lookup("wheel"); ok {
		t.Error("a group the image does not name resolved")
	}
	if calls, _ := os.ReadFile(filepath.Join(dir, "calls")); len(calls) != 0 {
		t.Errorf("docker was asked:\n%s", calls)
	}
	if len(reported) != 0 {
		t.Errorf("reported %v", reported)
	}

	// An image without the file resolves no name and says why.
	g = &imageGroups{docker: docker, image: "sha256:abc", remote: "registry.test/team/base:1", registry: groupRegistry(t, ""), report: report}
	if _, ok := g.lookup(context.Background())("users"); ok {
		t.Error("resolved without a group file")
	}
	if len(reported) != 1 || !strings.Contains(reported[0].Error(), "the image's /etc/group, read from its registry") || !errors.Is(reported[0], kubeimage.ErrNoImageFile) {
		t.Errorf("reported %v", reported)
	}
}
