package podseed

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
)

// failpoint is called at every step of reset, rollback and commit; a test sets
// it to return an error at one step, which is how a Job killed there looks.
var failpoint = func(step string) error { return nil }

// resetGeneration moves root generation prev out of the way so a new one can
// be seeded: root and root-extra become <dir>.g<prev>.trash and the manifest
// meta/rootfs.g<prev>.json. Run again after a crash at any point, it finishes
// the job: a dir whose trash already exists is a partly seeded newer
// generation, and is discarded instead. A dir prev never had gets an empty
// trash, so a rollback restores "nothing there" too.
func resetGeneration(pvc string, prev, gen int) error {
	if err := removePartials(pvc); err != nil {
		return err
	}
	for _, dir := range persistedDirs {
		cur := filepath.Join(pvc, dir)
		trash := filepath.Join(pvc, trashDir(dir, prev))
		switch {
		case exists(trash):
			if err := discard(cur, filepath.Join(pvc, partialDir(dir, gen))); err != nil {
				return err
			}
		case exists(cur):
			if err := os.Rename(cur, trash); err != nil {
				return fmt.Errorf("move generation %d's %s to trash: %w", prev, dir, err)
			}
		default:
			if err := os.Mkdir(trash, 0o755); err != nil {
				return fmt.Errorf("mark generation %d's missing %s: %w", prev, dir, err)
			}
		}
		if err := failpoint("reset:" + dir); err != nil {
			return err
		}
	}
	cur := filepath.Join(pvc, ManifestFile)
	trash := filepath.Join(pvc, trashManifest(prev))
	switch {
	case exists(trash):
		if err := removeIfExists(cur); err != nil {
			return err
		}
	case exists(cur):
		if err := os.Rename(cur, trash); err != nil {
			return fmt.Errorf("keep generation %d's manifest: %w", prev, err)
		}
	}
	if err := failpoint("reset:manifest"); err != nil {
		return err
	}
	return syncAll(pvc)
}

// rollbackGeneration restores root generation gen from trash, discarding
// whatever a failed newer generation left in its place. A dir with no trash
// was never moved, so it still is generation gen. It is idempotent: a
// rollback killed halfway is finished by running it again.
func rollbackGeneration(pvc string, gen int) error {
	if err := removePartials(pvc); err != nil {
		return err
	}
	for _, dir := range persistedDirs {
		cur := filepath.Join(pvc, dir)
		trash := filepath.Join(pvc, trashDir(dir, gen))
		if !exists(trash) {
			continue
		}
		if err := discard(cur, filepath.Join(pvc, partialDir(dir, gen+1))); err != nil {
			return err
		}
		if err := failpoint("rollback:discard:" + dir); err != nil {
			return err
		}
		if err := os.Rename(trash, cur); err != nil {
			return fmt.Errorf("restore generation %d's %s: %w", gen, dir, err)
		}
		if err := failpoint("rollback:restore:" + dir); err != nil {
			return err
		}
	}
	cur := filepath.Join(pvc, ManifestFile)
	trash := filepath.Join(pvc, trashManifest(gen))
	if exists(trash) {
		if err := os.Rename(trash, cur); err != nil {
			return fmt.Errorf("restore generation %d's manifest: %w", gen, err)
		}
	}
	if err := failpoint("rollback:manifest"); err != nil {
		return err
	}
	if err := removePartials(pvc); err != nil {
		return err
	}
	return syncAll(pvc)
}

// trashName matches the PVC entries resets leave: <dir>.g<N>.trash.
var trashName = regexp.MustCompile(`^(root|root-extra)\.g([0-9]+)\.trash$`)

// trashManifestName matches the manifests resets keep: rootfs.g<N>.json.
var trashManifestName = regexp.MustCompile(`^rootfs\.g([0-9]+)\.json$`)

// commitGeneration deletes every trashed generation older than gen, once a
// rebuild to gen has succeeded, and any partial leftovers.
func commitGeneration(pvc string, gen int) error {
	if err := removePartials(pvc); err != nil {
		return err
	}
	entries, err := os.ReadDir(pvc)
	if err != nil {
		return err
	}
	for _, e := range entries {
		m := trashName.FindStringSubmatch(e.Name())
		if m == nil {
			continue
		}
		if n, _ := strconv.Atoi(m[2]); n < gen {
			if err := removeTree(filepath.Join(pvc, e.Name())); err != nil {
				return fmt.Errorf("delete %s: %w", e.Name(), err)
			}
			if err := failpoint("commit:" + e.Name()); err != nil {
				return err
			}
		}
	}
	metas, err := os.ReadDir(filepath.Join(pvc, DirMeta))
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	for _, e := range metas {
		if m := trashManifestName.FindStringSubmatch(e.Name()); m != nil {
			if n, _ := strconv.Atoi(m[1]); n < gen {
				if err := removeIfExists(filepath.Join(pvc, DirMeta, e.Name())); err != nil {
					return err
				}
			}
		}
	}
	return syncAll(pvc)
}

// partialName matches a discarded generation on its way out.
var partialName = regexp.MustCompile(`^(root|root-extra)\.g[0-9]+\.partial$`)

// removePartials finishes deleting discarded generations an earlier run was
// killed in the middle of deleting.
func removePartials(pvc string) error {
	entries, err := os.ReadDir(pvc)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	for _, e := range entries {
		if partialName.MatchString(e.Name()) {
			if err := removeTree(filepath.Join(pvc, e.Name())); err != nil {
				return fmt.Errorf("delete %s: %w", e.Name(), err)
			}
		}
	}
	return nil
}

// discard removes dir by renaming it to partial first, so it is gone in one
// step and a crash mid-delete leaves only a .partial for the next run.
func discard(dir, partial string) error {
	if !exists(dir) {
		return nil
	}
	if err := os.Rename(dir, partial); err != nil {
		return fmt.Errorf("discard %s: %w", filepath.Base(dir), err)
	}
	if err := failpoint("discard:" + filepath.Base(dir)); err != nil {
		return err
	}
	return removeTree(partial)
}

func exists(p string) bool {
	_, err := os.Lstat(p)
	return err == nil
}

func removeIfExists(p string) error {
	if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// syncAll makes the PVC's top-level renames durable.
func syncAll(pvc string) error {
	if err := syncDir(pvc); err != nil {
		return err
	}
	if exists(filepath.Join(pvc, DirMeta)) {
		return syncDir(filepath.Join(pvc, DirMeta))
	}
	return nil
}

// removeTree is os.RemoveAll that also deletes a tree holding read-only dirs
// when the job is not root (capability tier C): the image's 0555 dirs, copied
// faithfully, would refuse their entries' deletion. Root needs no help.
func removeTree(p string) error {
	err := os.RemoveAll(p)
	if err == nil || !errors.Is(err, fs.ErrPermission) || os.Geteuid() == 0 {
		return err
	}
	_ = filepath.WalkDir(p, func(dir string, d fs.DirEntry, err error) error {
		if err == nil && d.IsDir() {
			_ = os.Chmod(dir, 0o700)
		}
		return nil
	})
	return os.RemoveAll(p)
}
