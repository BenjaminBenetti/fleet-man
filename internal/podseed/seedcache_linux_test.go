//go:build linux

package podseed

import (
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
)

// clonePVC stands in for a CSI volume clone: a copy of src's whole tree.
func clonePVC(t *testing.T, src string) string {
	t.Helper()
	dst := filepath.Join(t.TempDir(), "clone")
	if out, err := exec.Command("cp", "-a", src, dst).CombinedOutput(); err != nil {
		t.Fatalf("clone %s: %v\n%s", src, err, out)
	}
	t.Cleanup(func() { _ = removeTree(dst) })
	return dst
}

func topLevel(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

// A seed cache is the clone of a freshly seeded disk trimmed to its root
// generation; a disk cloned from it seeds the workspace, the volumes and the
// binary but keeps the cached root, and a disk whose clone is not that cache
// (another id, other dirs) is seeded in full.
func TestSeedCacheTrimAndAClonedSeed(t *testing.T) {
	e := newJobEnv(t)
	const id = "abc234def567"
	// The homes copy-up volume carries the remapped user's home, which a
	// cached seed copies up again.
	homes := Volume{Name: "homes", Target: "/home", CopyUp: true}
	first := e.seedSpec(1)
	first.Seed.Workspace = &Workspace{}
	first.Seed.Volumes = append(first.Seed.Volumes, homes)
	if code, res, log := e.runJob(t, first, true, workspaceTar(t, map[string]string{"first.txt": "first"})); code != 0 || !res.OK {
		t.Fatalf("first seed: %d %+v\n%s", code, res, log)
	}
	full, err := ReadManifest(filepath.Join(e.pvc, ManifestFile))
	if err != nil {
		t.Fatal(err)
	}

	cache := clonePVC(t, e.pvc)
	trim := e.spec(OpSeedCache, 1)
	trim.PVC, trim.Cache = cache, id
	code, res, log := e.runJob(t, trim, true, nil)
	if code != 0 || !res.OK {
		t.Fatalf("trim: %d %+v\n%s", code, res, log)
	}
	if got := topLevel(t, cache); !reflect.DeepEqual(got, []string{DirMeta, DirRoot}) {
		t.Fatalf("the trimmed cache holds %v, want only meta and root", got)
	}
	if got := topLevel(t, filepath.Join(cache, DirMeta)); !reflect.DeepEqual(got, []string{"rootfs.json"}) {
		t.Fatalf("the trimmed cache's meta holds %v", got)
	}
	cm, err := ParseManifest(res.Output)
	if err != nil {
		t.Fatal(err)
	}
	if cm.SeedCache != id || cm.Binary != "" || cm.Volumes != nil || cm.RootExtra != nil || !reflect.DeepEqual(cm.Dirs, full.Dirs) || cm.Files != full.Files {
		t.Fatalf("cache manifest %+v (from %+v)", cm, full)
	}
	// Trimming again is a no-op; as another cache it is refused.
	if code, res, log := e.runJob(t, trim, true, nil); code != 0 || !res.OK {
		t.Fatalf("trim again: %d %+v\n%s", code, res, log)
	}
	other := trim
	other.Cache = "zzz234zzz567"
	if code, res, _ := e.runJob(t, other, true, nil); code == 0 || res.OK || !strings.Contains(res.Error, "already seed cache") {
		t.Fatalf("trim as another cache: %d %+v", code, res)
	}
	// A sentinel only the cache's root has, and an image file changed since:
	// a disk cloned from the cache keeps the cache's.
	if err := os.WriteFile(filepath.Join(cache, "root/usr/cache-sentinel"), []byte("cached"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(e.img.root, "usr/bin/tool"), []byte("changed since\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	cloned := clonePVC(t, cache)
	second := e.seedSpec(1)
	second.PVC = cloned
	second.Seed.Cache = id
	second.Seed.Workspace = &Workspace{}
	second.Seed.Volumes = append(second.Seed.Volumes, homes)
	code, res, log = e.runJob(t, second, true, workspaceTar(t, map[string]string{"second.txt": "second"}))
	if code != 0 || !res.OK {
		t.Fatalf("cached seed: %d %+v\n%s", code, res, log)
	}
	m, err := ParseManifest(res.Output)
	if err != nil {
		t.Fatal(err)
	}
	if m.FromCache != id || m.SeedCache != "" || m.Binary != e.binarySHA || !reflect.DeepEqual(m.Dirs, full.Dirs) || !reflect.DeepEqual(m.RootExtra, full.RootExtra) {
		t.Fatalf("cached seed manifest %+v", m)
	}
	if !reflect.DeepEqual(m.Volumes, full.Volumes) || m.Files <= cm.Files || !reflect.DeepEqual(m.Remap, full.Remap) {
		t.Errorf("cached seed volumes %v files %d remap %+v; want %v, more than the cache's %d, %+v", m.Volumes, m.Files, m.Remap, full.Volumes, cm.Files, full.Remap)
	}
	read := func(pvc, p string) string {
		data, _ := os.ReadFile(filepath.Join(pvc, p))
		return string(data)
	}
	if read(cloned, "root/usr/cache-sentinel") != "cached" || read(cloned, "root/usr/bin/tool") == "changed since\n" {
		t.Error("the cached seed recopied the root")
	}
	if read(cloned, "ws/second.txt") != "second" || read(cloned, "ws/first.txt") != "" {
		t.Error("the cached seed's workspace is not its own")
	}
	if read(cloned, "vol/docker/image-content") != "under a volume target" || read(cloned, HostnameFile) != "fi-fleet-man-alpha-1a2b3c4d-0\n" {
		t.Error("the cached seed did not seed the volumes and meta files")
	}
	if target, _ := os.Readlink(filepath.Join(cloned, "root/usr/bin/fleet")); target != CurrentBinary {
		t.Errorf("/usr/bin/fleet -> %q", target)
	}
	if target, _ := os.Readlink(filepath.Join(cloned, FleetBinDir, "fleet")); target != BinaryName(e.binarySHA) {
		t.Errorf("fleet/bin/fleet -> %q", target)
	}
	for _, dir := range []string{"root-extra/workspaces", "root-extra/fleet-mounts"} {
		if fi, err := os.Stat(filepath.Join(cloned, dir)); err != nil || !fi.IsDir() {
			t.Errorf("%s: %v", dir, err)
		}
	}
	if !slices.Contains(m.Volumes, "homes") {
		t.Errorf("the cached seed's volumes %v lack homes", m.Volumes)
	} else if os.Geteuid() == 0 {
		if st := lstat(t, filepath.Join(cloned, "vol/homes/vscode")); st.Uid != 4321 {
			t.Errorf("a volume copied up in a cached seed lost the remap: owner %d", st.Uid)
		}
	}

	// A clone the seed cannot trust as the cache: seeded in full.
	for name, mutate := range map[string]func(spec *JobSpec, pvc string){
		"another cache id": func(spec *JobSpec, _ string) { spec.Seed.Cache = "zzz234zzz567" },
		"other root dirs": func(_ *JobSpec, pvc string) {
			bad := cm
			bad.Dirs = []string{"etc"}
			data, _ := bad.encode()
			if err := os.WriteFile(filepath.Join(pvc, ManifestFile), data, 0o644); err != nil {
				t.Fatal(err)
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			pvc := clonePVC(t, cache)
			spec := e.seedSpec(1)
			spec.PVC = pvc
			spec.Seed.Cache = id
			mutate(&spec, pvc)
			code, res, log := e.runJob(t, spec, true, nil)
			if code != 0 || !res.OK {
				t.Fatalf("seed: %d %+v\n%s", code, res, log)
			}
			m, err := ParseManifest(res.Output)
			if err != nil {
				t.Fatal(err)
			}
			if m.FromCache != "" || read(pvc, "root/usr/cache-sentinel") != "" || read(pvc, "root/usr/bin/tool") != "changed since\n" {
				t.Errorf("%s: the seed kept the cached root (from_cache %q)", name, m.FromCache)
			}
		})
	}
}

// A trim needs the seeded root it is meant to keep.
func TestSeedCacheTrimRefusesAnUnseededDisk(t *testing.T) {
	pvc := t.TempDir()
	if err := trimSeedCache(pvc, 1, "abc"); err == nil || !strings.Contains(err.Error(), "no seeded root") {
		t.Fatalf("trim of an empty disk: %v", err)
	}
	m := Manifest{V: ManifestVersion, Gen: 2, Dirs: []string{"etc"}}
	data, _ := m.encode()
	mustMkdir(t, filepath.Join(pvc, DirMeta), 0o755)
	if err := os.WriteFile(filepath.Join(pvc, ManifestFile), data, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := trimSeedCache(pvc, 1, "abc"); err == nil || !strings.Contains(err.Error(), "generation 2") {
		t.Fatalf("trim of another generation: %v", err)
	}
	if err := trimSeedCache(pvc, 2, "abc"); err == nil || !strings.Contains(err.Error(), "no root/") {
		t.Fatalf("trim without root/: %v", err)
	}
}
