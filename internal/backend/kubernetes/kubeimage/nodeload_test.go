package kubeimage

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
)

// fakeCachingMinikube is the fake minikube, which also caches what `image
// load` loads as the real one does: a tarball under
// $MINIKUBE_HOME/cache/images/amd64/<ref, each ':' as '_'>.
const fakeCachingMinikube = `#!/bin/sh
d=$(dirname "$0")
echo "$*" >> "$d/minikube.log"
for ref in "$@"; do :; done
case "$*" in
*"image load"*)
	f="$MINIKUBE_HOME/cache/images/amd64/$(printf '%s' "$ref" | tr ':' '_')"
	mkdir -p "$(dirname "$f")" && echo tarball > "$f" ;;
esac
if [ -f "$d/minikube.fail" ]; then cat "$d/minikube.fail" >&2; exit 1; fi
`

// minikubeCache is a minikube home for the test (MINIKUBE_HOME ending in
// .minikube, as minikube takes it) with entries of others' in its image
// cache; it returns the cache's amd64 folder.
func minikubeCache(t *testing.T, h *harness) string {
	t.Helper()
	home := filepath.Join(t.TempDir(), ".minikube")
	t.Setenv("MINIKUBE_HOME", home)
	h.tools.write(t, "minikube", fakeCachingMinikube, 0o755)
	images := filepath.Join(home, "cache", "images", "amd64")
	for _, other := range []string{
		"fleet-probe/local_1",                       // the user's own load
		"docker.io/library/busybox_1.37",            // a pulled image's
		"fleet.invalid/ffffffff/devcontainer_other", // another daemon's fleet tag
	} {
		f := filepath.Join(images, filepath.FromSlash(other))
		if err := os.MkdirAll(filepath.Dir(f), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(f, []byte("keep"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return images
}

// cacheFile is where minikube caches ref under images.
func cacheFile(images, ref string) string {
	return filepath.Join(images, filepath.FromSlash(strings.ReplaceAll(ref, ":", "_")))
}

func exists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

// A node load leaves no tarball of fleet's tag in minikube's image cache
// (minikube keeps one of every image it loads, and fleet loads every image
// it builds): it goes once the load is done, failed or not; nothing else in
// the cache is touched.
func TestNodeLoadForgetsMinikubesCachedTarball(t *testing.T) {
	h := newHarness(t)
	images := minikubeCache(t, h)
	tgt := h.target(h.cluster(), minikubeDev)
	id := imageID("cached")
	tag := NodeLoadTag(testDaemonID, id)
	if _, err := h.m.Deliver(context.Background(), tgt, builtImage(id), "inst"); err != nil {
		t.Fatal(err)
	}
	if l := h.tools.lines(t, "minikube.log"); len(l) != 1 || !strings.Contains(l[0], "image load") {
		t.Fatalf("minikube saw %q", l)
	}
	if exists(cacheFile(images, tag)) {
		t.Errorf("minikube's cache still holds %s", tag)
	}
	if exists(filepath.Join(images, "fleet.invalid", daemon8(testDaemonID))) {
		t.Error("the daemon's emptied folder in minikube's cache is still there")
	}
	for _, other := range []string{"fleet-probe/local_1", "docker.io/library/busybox_1.37", "fleet.invalid/ffffffff/devcontainer_other"} {
		if !exists(filepath.Join(images, filepath.FromSlash(other))) {
			t.Errorf("%s went from minikube's cache", other)
		}
	}

	// A load that fails goes the same way.
	h.tools.write(t, "minikube.fail", "boom", 0o644)
	failed := imageID("failed")
	if _, err := h.m.Deliver(context.Background(), tgt, builtImage(failed), "other"); err == nil {
		t.Fatal("a failed load delivered")
	}
	if exists(cacheFile(images, NodeLoadTag(testDaemonID, failed))) {
		t.Error("a failed load left its tarball in minikube's cache")
	}
}

// Removing an image (the GC) removes a tarball a load left in the cache (an
// older fleet's, or one cut short), and nothing of anyone else's.
func TestNodeLoadGCForgetsALeftoverTarball(t *testing.T) {
	h := newHarness(t)
	images := minikubeCache(t, h)
	tgt := h.target(h.cluster(func(c *fleet.KubeCluster) { c.Image.Retention = "0" }), minikubeDev)
	id := imageID("leftover")
	tag := NodeLoadTag(testDaemonID, id)
	h.deliverAndRelease(t, tgt, id)
	left := cacheFile(images, tag)
	if err := os.MkdirAll(filepath.Dir(left), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(left, []byte("tarball"), 0o644); err != nil {
		t.Fatal(err)
	}
	h.clock.advance(time.Hour)
	if res, err := h.m.GC(context.Background(), tgt, GCOptions{}); err != nil || len(res.Deleted) != 1 {
		t.Fatalf("GC = %+v, %v", res, err)
	}
	if exists(left) {
		t.Errorf("the GC left %s's tarball in minikube's cache", tag)
	}
	if !exists(filepath.Join(images, "fleet.invalid", "ffffffff", "devcontainer_other")) || !exists(filepath.Join(images, "fleet-probe", "local_1")) {
		t.Error("the GC removed another cache entry")
	}
}

// Only a node load tag of fleet's names a cache file; the home follows
// MINIKUBE_HOME as minikube reads it.
func TestMinikubeCacheFiles(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("MINIKUBE_HOME", dir)
	if got := minikubeHome(); got != filepath.Join(dir, ".minikube") {
		t.Errorf("minikubeHome = %q with MINIKUBE_HOME=%s", got, dir)
	}
	t.Setenv("MINIKUBE_HOME", filepath.Join(dir, ".minikube"))
	if got := minikubeHome(); got != filepath.Join(dir, ".minikube") {
		t.Errorf("minikubeHome = %q with MINIKUBE_HOME=%s/.minikube", got, dir)
	}
	for _, arch := range []string{"amd64", "arm64"} {
		if err := os.MkdirAll(filepath.Join(dir, ".minikube", "cache", "images", arch), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	files, depth := minikubeCacheFiles("fleet.invalid/abcd1234/devcontainer:0123456789ab")
	want := []string{
		filepath.Join(dir, ".minikube", "cache", "images", "amd64", "fleet.invalid", "abcd1234", "devcontainer_0123456789ab"),
		filepath.Join(dir, ".minikube", "cache", "images", "arm64", "fleet.invalid", "abcd1234", "devcontainer_0123456789ab"),
	}
	if strings.Join(files, "\n") != strings.Join(want, "\n") || depth != 2 {
		t.Errorf("minikubeCacheFiles = %q, %d; want %q, 2", files, depth, want)
	}
	for _, ref := range []string{
		"busybox:1.37", "fleet-probe/local:1", "registry.example.com/fleet/abcd1234/devcontainer:0123456789ab",
		"fleet.invalid/abcd1234/devcontainer", "fleet.invalid/abcd1234/devcontainer@sha256:" + strings.Repeat("a", 64),
		"fleet.invalid/../../x:1", "",
	} {
		if files, _ := minikubeCacheFiles(ref); files != nil {
			t.Errorf("minikubeCacheFiles(%q) = %q; want none", ref, files)
		}
	}
}
