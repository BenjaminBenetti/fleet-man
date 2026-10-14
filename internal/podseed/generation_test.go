package podseed

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

var errInjected = errors.New("injected crash")

// injectAt makes the failpoint named step fail once.
func injectAt(t *testing.T, step string) *bool {
	t.Helper()
	hit := false
	old := failpoint
	failpoint = func(s string) error {
		if s == step && !hit {
			hit = true
			return errInjected
		}
		return nil
	}
	t.Cleanup(func() { failpoint = old })
	return &hit
}

// genPVC is a PVC holding generation gen: a marker in root/ and root-extra/
// and its manifest.
func genPVC(t *testing.T, gen int) string {
	t.Helper()
	pvc := t.TempDir()
	writeGen(t, pvc, gen)
	return pvc
}

func writeGen(t *testing.T, pvc string, gen int) {
	t.Helper()
	for _, dir := range persistedDirs {
		if err := os.MkdirAll(filepath.Join(pvc, dir, "etc"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(pvc, dir, "etc", "gen"), []byte(fmt.Sprint(gen)), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(filepath.Join(pvc, DirMeta), 0o755); err != nil {
		t.Fatal(err)
	}
	data, _ := Manifest{V: 1, Gen: gen, Dirs: []string{"etc"}}.encode()
	if err := os.WriteFile(filepath.Join(pvc, ManifestFile), data, 0o644); err != nil {
		t.Fatal(err)
	}
}

// genOf reads which generation each persisted dir and the manifest hold
// ("-" when missing).
func genOf(t *testing.T, pvc string) string {
	t.Helper()
	var parts []string
	for _, dir := range persistedDirs {
		data, err := os.ReadFile(filepath.Join(pvc, dir, "etc", "gen"))
		if err != nil {
			parts = append(parts, "-")
			continue
		}
		parts = append(parts, string(data))
	}
	if m, err := ReadManifest(filepath.Join(pvc, ManifestFile)); err == nil {
		parts = append(parts, fmt.Sprint(m.Gen))
	} else {
		parts = append(parts, "-")
	}
	return strings.Join(parts, " ")
}

func entries(t *testing.T, dir string) string {
	t.Helper()
	es, _ := os.ReadDir(dir)
	var names []string
	for _, e := range es {
		names = append(names, e.Name())
	}
	sort.Strings(names)
	return strings.Join(names, " ")
}

func TestResetRollbackCommit(t *testing.T) {
	pvc := genPVC(t, 1)
	if err := resetGeneration(pvc, 1, 2); err != nil {
		t.Fatal(err)
	}
	if got := genOf(t, pvc); got != "- - -" {
		t.Fatalf("after the reset: %s, want nothing current", got)
	}
	if got := entries(t, pvc); got != "meta root-extra.g1.trash root.g1.trash" {
		t.Fatalf("after the reset: %s", got)
	}
	writeGen(t, pvc, 2)
	// A rollback restores generation 1, discarding 2.
	if err := rollbackGeneration(pvc, 1); err != nil {
		t.Fatal(err)
	}
	if got := genOf(t, pvc); got != "1 1 1" {
		t.Fatalf("after the rollback: %s", got)
	}
	if got := entries(t, pvc); got != "meta root root-extra" {
		t.Fatalf("after the rollback: %s", got)
	}
	// Rolling back twice is harmless.
	if err := rollbackGeneration(pvc, 1); err != nil || genOf(t, pvc) != "1 1 1" {
		t.Fatalf("second rollback: %v, %s", err, genOf(t, pvc))
	}

	// A successful rebuild is committed: the trash goes.
	if err := resetGeneration(pvc, 1, 2); err != nil {
		t.Fatal(err)
	}
	writeGen(t, pvc, 2)
	if err := commitGeneration(pvc, 2); err != nil {
		t.Fatal(err)
	}
	if got := entries(t, pvc); got != "meta root root-extra" {
		t.Fatalf("after the commit: %s", got)
	}
	if got := entries(t, filepath.Join(pvc, DirMeta)); got != "rootfs.json" {
		t.Fatalf("meta after the commit: %s", got)
	}
	if got := genOf(t, pvc); got != "2 2 2" {
		t.Fatalf("after the commit: %s", got)
	}
}

func TestResetOfAGenerationWithoutRootExtra(t *testing.T) {
	pvc := genPVC(t, 1)
	if err := os.RemoveAll(filepath.Join(pvc, DirRootExtra)); err != nil {
		t.Fatal(err)
	}
	if err := resetGeneration(pvc, 1, 2); err != nil {
		t.Fatal(err)
	}
	writeGen(t, pvc, 2)
	if err := rollbackGeneration(pvc, 1); err != nil {
		t.Fatal(err)
	}
	// Generation 1 had no root-extra content, so none comes back.
	if got := genOf(t, pvc); got != "1 - 1" {
		t.Fatalf("after the rollback: %s", got)
	}
}

// TestCrashAtEveryStepIsRecoverable kills reset and rollback at each step,
// then runs them again (what a retried Job does) and checks the PVC ends where
// an uninterrupted run would.
func TestCrashAtEveryStepIsRecoverable(t *testing.T) {
	resetSteps := []string{"reset:root", "reset:root-extra", "reset:manifest", "discard:root", "discard:root-extra"}
	for _, step := range resetSteps {
		t.Run("reset/"+step, func(t *testing.T) {
			pvc := genPVC(t, 1)
			// An earlier attempt at generation 2 got as far as copying.
			if err := resetGeneration(pvc, 1, 2); err != nil {
				t.Fatal(err)
			}
			writeGen(t, pvc, 2)
			hit := injectAt(t, step)
			err := resetGeneration(pvc, 1, 2)
			if !*hit || !errors.Is(err, errInjected) {
				t.Fatalf("the crash at %s was not hit or not reported: %v", step, err)
			}
			if err := resetGeneration(pvc, 1, 2); err != nil {
				t.Fatalf("rerun after the crash: %v", err)
			}
			if got := genOf(t, pvc); got != "- - -" {
				t.Fatalf("after the rerun: %s, want a clean slate", got)
			}
			if err := rollbackGeneration(pvc, 1); err != nil || genOf(t, pvc) != "1 1 1" {
				t.Fatalf("rollback after it: %v %s", err, genOf(t, pvc))
			}
		})
	}
	rollbackSteps := []string{"rollback:discard:root", "rollback:restore:root", "rollback:discard:root-extra",
		"rollback:restore:root-extra", "rollback:manifest", "discard:root", "discard:root-extra"}
	for _, step := range rollbackSteps {
		t.Run("rollback/"+step, func(t *testing.T) {
			pvc := genPVC(t, 1)
			if err := resetGeneration(pvc, 1, 2); err != nil {
				t.Fatal(err)
			}
			writeGen(t, pvc, 2)
			hit := injectAt(t, step)
			err := rollbackGeneration(pvc, 1)
			if !*hit || !errors.Is(err, errInjected) {
				t.Fatalf("the crash at %s was not hit or not reported: %v", step, err)
			}
			if err := rollbackGeneration(pvc, 1); err != nil {
				t.Fatalf("rerun after the crash: %v", err)
			}
			if got := genOf(t, pvc); got != "1 1 1" {
				t.Fatalf("after the rerun: %s", got)
			}
			if got := entries(t, pvc); got != "meta root root-extra" {
				t.Fatalf("leftovers: %s", got)
			}
		})
	}
	t.Run("a crash between reset renames", func(t *testing.T) {
		pvc := genPVC(t, 1)
		injectAt(t, "reset:root")
		if err := resetGeneration(pvc, 1, 2); !errors.Is(err, errInjected) {
			t.Fatal(err)
		}
		// root is in trash, root-extra and the manifest are still generation 1:
		// a rollback now must restore root and leave the rest.
		if err := rollbackGeneration(pvc, 1); err != nil || genOf(t, pvc) != "1 1 1" {
			t.Fatalf("%v %s", err, genOf(t, pvc))
		}
	})
}

func TestCommitKeepsTheCurrentGenerationAndNewerTrash(t *testing.T) {
	pvc := genPVC(t, 3)
	for _, name := range []string{"root.g1.trash", "root-extra.g2.trash", "root.g3.trash", "root.g2.partial"} {
		if err := os.Mkdir(filepath.Join(pvc, name), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range []string{"rootfs.g1.json", "rootfs.g3.json"} {
		if err := os.WriteFile(filepath.Join(pvc, DirMeta, name), nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := commitGeneration(pvc, 3); err != nil {
		t.Fatal(err)
	}
	if got := entries(t, pvc); got != "meta root root-extra root.g3.trash" {
		t.Fatalf("after the commit: %s", got)
	}
	if got := entries(t, filepath.Join(pvc, DirMeta)); got != "rootfs.g3.json rootfs.json" {
		t.Fatalf("meta: %s", got)
	}
}
