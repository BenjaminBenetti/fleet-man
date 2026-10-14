package podseed

import (
	"fmt"
	"os"
	"path"
	"path/filepath"
)

// A seed cache is an image's seeded root on a disk of its own: an
// automation instance's disk is cloned from it (a CSI volume clone), so its
// seed copies only the workspace, the volumes and the binary instead of the
// whole image. A cache is made from the clone of the first such instance's
// disk, taken right after its seed and before its pod writes anything,
// which an OpSeedCache trims to the root generation's root/ and manifest.

// lostAndFound is the dir an ext4 volume's root holds from its mkfs.
const lostAndFound = "lost+found"

// trimSeedCache is an OpSeedCache on pvc, the clone of an instance disk whose
// seed made root generation gen: everything but that generation's root/ and
// its manifest goes (the instance's workspace, volumes, binary and
// root-extra), and the manifest marks the disk as seed cache id. Run again
// on a disk it already trimmed for id, it does nothing more.
func trimSeedCache(pvc string, gen int, id string) error {
	file := filepath.Join(pvc, ManifestFile)
	m, err := ReadManifest(file)
	switch {
	case err != nil:
		return fmt.Errorf("the cloned disk has no seeded root: %w", err)
	case m.Gen != gen:
		return fmt.Errorf("the cloned disk holds root generation %d, not %d", m.Gen, gen)
	case m.SeedCache != "" && m.SeedCache != id:
		return fmt.Errorf("the cloned disk is already seed cache %s", m.SeedCache)
	}
	if fi, err := os.Lstat(filepath.Join(pvc, DirRoot)); err != nil || !fi.IsDir() {
		return fmt.Errorf("the cloned disk has no %s/ dir", DirRoot)
	}
	entries, err := os.ReadDir(pvc)
	if err != nil {
		return err
	}
	for _, e := range entries {
		switch e.Name() {
		case DirRoot, DirMeta, lostAndFound:
			continue
		}
		if err := removeTree(filepath.Join(pvc, e.Name())); err != nil {
			return fmt.Errorf("trim %s: %w", e.Name(), err)
		}
	}
	metaEntries, err := os.ReadDir(filepath.Join(pvc, DirMeta))
	if err != nil {
		return err
	}
	for _, e := range metaEntries {
		if e.Name() == path.Base(ManifestFile) {
			continue
		}
		if err := removeTree(filepath.Join(pvc, DirMeta, e.Name())); err != nil {
			return fmt.Errorf("trim %s/%s: %w", DirMeta, e.Name(), err)
		}
	}
	m.SeedCache, m.FromCache = id, ""
	m.RootExtra, m.Volumes, m.Binary = nil, nil, ""
	data, _ := m.encode()
	if err := writeDurable(file, data, 0o644); err != nil {
		return fmt.Errorf("write the seed cache's manifest: %w", err)
	}
	return syncAll(pvc)
}

// cachedRoot is the root a seed cache gave a disk cloned from it: its
// manifest, when it marks the disk as seed cache id at generation gen.
func cachedRoot(pvc string, gen int, id string) (*Manifest, bool) {
	if id == "" {
		return nil, false
	}
	m, err := ReadManifest(filepath.Join(pvc, ManifestFile))
	if err != nil || m.Gen != gen || m.SeedCache != id {
		return nil, false
	}
	if fi, err := os.Lstat(filepath.Join(pvc, DirRoot)); err != nil || !fi.IsDir() {
		return nil, false
	}
	return &m, true
}
