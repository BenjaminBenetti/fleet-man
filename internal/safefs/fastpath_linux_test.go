//go:build linux

package safefs

import (
	"errors"
	"os"
	"testing"

	"golang.org/x/sys/unix"
)

// TestFastPathResolvesInOneCall checks openat2 is what Linux walks use here:
// a real folder opens, and a symlink or a file element fails the call (the
// element walk then names it).
func TestFastPathResolvesInOneCall(t *testing.T) {
	if !FastPath() {
		t.Skip("openat2 unavailable on this kernel or sandbox")
	}
	fx := newFixture(t, false)
	d, err := fx.r.openFast("a/b")
	if err != nil {
		t.Fatalf("openFast(a/b): %v", err)
	}
	defer d.close()
	if d.path != fx.path("a/b") || d.raw == nil {
		t.Errorf("openFast(a/b) = %+v", d)
	}
	opened, err := d.root.Stat(".")
	want, _ := os.Lstat(fx.path("a/b"))
	if err != nil || !os.SameFile(opened, want) {
		t.Errorf("openFast(a/b) opened %v, %v; want %v", opened, err, want)
	}
	for name, wantErr := range map[string]error{
		"in":      unix.ELOOP,
		"a/up":    unix.ELOOP,
		"rel/sub": unix.ELOOP,
		"a/f":     unix.ENOTDIR,
		"a/none":  unix.ENOENT,
	} {
		if d, err := fx.r.openFast(name); !errors.Is(err, wantErr) {
			if err == nil {
				d.close()
			}
			t.Errorf("openFast(%s): %v, want %v", name, err, wantErr)
		}
	}
	if !FastPath() {
		t.Error("a refused walk turned the fast path off")
	}
}
