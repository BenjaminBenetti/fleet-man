//go:build unix

package execas

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestLookPathSearchesAsTheTargetUser(t *testing.T) {
	me := CurrentCreds()
	stranger := Identity{UID: me.UID + 1000, GID: me.GID + 1000, Groups: []uint32{me.GID + 1000}}
	if me.UID == 0 {
		stranger = Identity{UID: 4242, GID: 4242, Groups: []uint32{4242}}
	}
	ownerOnly, groupOnly, public := t.TempDir(), t.TempDir(), t.TempDir()
	writeExecutable(t, filepath.Join(ownerOnly, "tool"), 0o700)
	writeExecutable(t, filepath.Join(groupOnly, "tool"), 0o750)
	writeExecutable(t, filepath.Join(public, "tool"), 0o755)
	pathEnv := ownerOnly + ":" + groupOnly + ":" + public

	owner := Identity{UID: me.UID, GID: me.GID, Groups: me.Groups}
	if got, err := lookPath("tool", pathEnv, "/", owner); err != nil || got != filepath.Join(ownerOnly, "tool") {
		t.Errorf("as the owner: %q, %v", got, err)
	}
	inGroup := Identity{UID: stranger.UID, GID: stranger.GID, Groups: []uint32{stranger.GID, me.GID}}
	if got, err := lookPath("tool", pathEnv, "/", inGroup); err != nil || got != filepath.Join(groupOnly, "tool") {
		t.Errorf("as a group member: %q, %v", got, err)
	}
	if got, err := lookPath("tool", pathEnv, "/", stranger); err != nil || got != filepath.Join(public, "tool") {
		t.Errorf("as a stranger: %q, %v", got, err)
	}
	root := Identity{UID: 0}
	if got, err := lookPath("tool", pathEnv, "/", root); err != nil || got != filepath.Join(ownerOnly, "tool") {
		t.Errorf("as root (any x bit): %q, %v", got, err)
	}

	// A path with a slash is not searched, and one the user may not run is 126.
	_, err := lookPath(filepath.Join(ownerOnly, "tool"), "", "/", stranger)
	var exitErr *ExitError
	if !errors.As(err, &exitErr) || exitErr.Code != ExitCannotRun {
		t.Errorf("an owner-only path as a stranger: %v", err)
	}
	// Relative names resolve against the working directory, and an empty PATH
	// entry is that directory.
	if got, err := lookPath("./tool", "", public, stranger); err != nil || got != "./tool" {
		t.Errorf("./tool in %s: %q, %v", public, got, err)
	}
	if got, err := lookPath("tool", ":", public, stranger); err != nil || got != "./tool" {
		t.Errorf("an empty PATH entry: %q, %v", got, err)
	}
	// Directories and non-executables are skipped; nothing found is 127.
	dirNamedTool, dataFile := t.TempDir(), t.TempDir()
	if err := os.Mkdir(filepath.Join(dirNamedTool, "tool"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeExecutable(t, filepath.Join(dataFile, "tool"), 0o644)
	_, err = lookPath("tool", dirNamedTool+":"+dataFile, "/", owner)
	if !errors.As(err, &exitErr) || exitErr.Code != ExitNotFound {
		t.Errorf("nothing runnable on PATH: %v", err)
	}
}
