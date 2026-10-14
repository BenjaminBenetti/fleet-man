package sharesync

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// BenchmarkPollOfBoundFiles is one poll pass over the host folder of bound
// files: twenty files of one big folder ($HOME, as ~/.gitconfig is), a root
// holding only them, the folder holding 5000 other entries. On macOS and
// the BSDs, which have no inotify, a side scans its roots again every
// SideOptions.Poll (2s); the nightly macOS job holds a pass to its budget.
func BenchmarkPollOfBoundFiles(b *testing.B) {
	home := b.TempDir()
	for i := range 5000 {
		p := filepath.Join(home, fmt.Sprintf("entry-%04d", i))
		var err error
		if i%10 == 0 {
			err = os.Mkdir(p, 0o755)
		} else {
			err = os.WriteFile(p, []byte("x"), 0o644)
		}
		if err != nil {
			b.Fatal(err)
		}
	}
	var only []string
	for i := range 20 {
		name := fmt.Sprintf(".bound-%02d", i)
		if err := os.WriteFile(filepath.Join(home, name), []byte("bound\n"), 0o644); err != nil {
			b.Fatal(err)
		}
		only = append(only, name)
	}
	const root = ".hostbind/1a2b3c4d"
	tree, err := OpenTree([]TreeRoot{{Name: root, Dir: home, Only: only}}, TreeOptions{StateDir: b.TempDir(), Settle: -1})
	if err != nil {
		b.Fatal(err)
	}
	defer tree.Close()
	ctx := context.Background()
	if _, _, err := tree.Scan(ctx, root, nil); err != nil {
		b.Fatal(err)
	}
	b.ResetTimer()
	for b.Loop() {
		if _, _, err := tree.Scan(ctx, root, nil); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkPollOfALargeCustomMount is one poll pass over a custom mount's
// host folder of 20,000 files in 400 folders (a checked-out repository's
// size): on macOS and the BSDs every custom mount is scanned in full every
// SideOptions.Poll (2s), so this is that pass's cost per 20,000 files; the
// nightly poll-cost job records it beside the bound files' budget.
func BenchmarkPollOfALargeCustomMount(b *testing.B) {
	dir := b.TempDir()
	for d := range 400 {
		sub := filepath.Join(dir, fmt.Sprintf("pkg-%03d", d/20), fmt.Sprintf("mod-%03d", d))
		if err := os.MkdirAll(sub, 0o755); err != nil {
			b.Fatal(err)
		}
		for f := range 50 {
			if err := os.WriteFile(filepath.Join(sub, fmt.Sprintf("file-%02d.go", f)), []byte("package mod\n"), 0o644); err != nil {
				b.Fatal(err)
			}
		}
	}
	const root = ".mnt"
	tree, err := OpenTree([]TreeRoot{{Name: root, Dir: dir}}, TreeOptions{StateDir: b.TempDir(), Settle: -1})
	if err != nil {
		b.Fatal(err)
	}
	defer tree.Close()
	ctx := context.Background()
	if _, _, err := tree.Scan(ctx, root, nil); err != nil {
		b.Fatal(err)
	}
	b.ResetTimer()
	for b.Loop() {
		if _, _, err := tree.Scan(ctx, root, nil); err != nil {
			b.Fatal(err)
		}
	}
}
