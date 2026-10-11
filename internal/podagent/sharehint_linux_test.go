//go:build linux

package podagent

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/BenjaminBenetti/fleet-man/internal/podshare"
	"github.com/BenjaminBenetti/fleet-man/internal/podwire"
)

// TestShareHintsReachFleetd: a desired state naming the share's folders has
// the main agent watch them and hint each change on a share-hint stream to
// fleetd (one stream, its folder named); a later state without them stops
// the hints.
func TestShareHintsReachFleetd(t *testing.T) {
	defer func(d time.Duration) { hintDebounce = d }(hintDebounce)
	hintDebounce = 20 * time.Millisecond
	d := newTestDirs(t)
	syncDir, share, _ := shareTree(t, d)
	hints := make(chan podwire.ShareHintMsg, 64)
	streams := make(chan struct{}, 8)
	handlers := map[podwire.Tag]podwire.Handler{podwire.TagShareHint: func(st *podwire.Stream) {
		streams <- struct{}{}
		conn, err := podwire.AcceptSock(st)
		if err != nil {
			return
		}
		defer conn.Close()
		r := podwire.NewShareHintReader(conn)
		for {
			m, err := r.Next()
			if err != nil {
				return
			}
			hints <- m
		}
	}}
	o := d.options()
	o.ControlCheck = controlCheck
	o.ShareDir, o.ShareSync = share, syncDir
	s, err := tryStartAgent(t, o, podwire.ClientOptions{Handlers: handlers})
	if err != nil {
		t.Fatal(err)
	}
	if ack := apply(t, s, podwire.DesiredState{Gen: 1, ShareHint: &podwire.ShareHint{Roots: []podwire.ShareHintRoot{{Name: ".claude"}, {Name: "files"}}}}); ack.Error != "" {
		t.Fatalf("ack %q", ack.Error)
	}
	write := func(rel string) {
		t.Helper()
		p := filepath.Join(share, podshare.RootsDir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// The watch starts in the background: write until a hint names the
	// file.
	deadline := time.Now().Add(10 * time.Second)
	got := false
	for !got && time.Now().Before(deadline) {
		write(".claude/projects/a/s.jsonl")
		select {
		case m := <-hints:
			got = m.Root == ".claude" && (slices.Contains(m.Paths, "projects/a/s.jsonl") || slices.Contains(m.Paths, "projects") || slices.Contains(m.Paths, "projects/a"))
		case <-time.After(200 * time.Millisecond):
		}
	}
	if !got {
		t.Fatal("no hint named the file a pod wrote on the share")
	}
	write("files/.claude.json")
	waitHint(t, hints, func(m podwire.ShareHintMsg) bool {
		return m.Root == "files" && slices.Contains(m.Paths, ".claude.json")
	})
	if n := len(streams); n != 1 {
		t.Fatalf("%d share-hint streams, want the one", n)
	}

	if ack := apply(t, s, podwire.DesiredState{Gen: 2}); ack.Error != "" {
		t.Fatalf("ack %q", ack.Error)
	}
	time.Sleep(100 * time.Millisecond)
	for len(hints) > 0 {
		<-hints
	}
	write(".claude/later.txt")
	select {
	case m := <-hints:
		t.Fatalf("a hint after the state dropped the share's folders: %+v", m)
	case <-time.After(500 * time.Millisecond):
	}
}

func waitHint(t *testing.T, hints <-chan podwire.ShareHintMsg, ok func(podwire.ShareHintMsg) bool) {
	t.Helper()
	deadline := time.After(10 * time.Second)
	for {
		select {
		case m := <-hints:
			if ok(m) {
				return
			}
		case <-deadline:
			t.Fatal("the hint never came")
		}
	}
}

// TestHintRoots: a changed path is its longest root's; the watch skips what
// is in no root and above none.
func TestHintRoots(t *testing.T) {
	h := newHintRoots([]podwire.ShareHintRoot{{Name: ".mnt"}, {Name: ".mnt/opt/data"}, {Name: ".config/gh"}})
	for rel, want := range map[string][2]string{
		".mnt/x":               {".mnt", "x"},
		".mnt/opt/data/y":      {".mnt/opt/data", "y"},
		".mnt/opt/data":        {".mnt/opt/data", "."},
		".config/gh/hosts.yml": {".config/gh", "hosts.yml"},
	} {
		root, inner, ok := h.of(rel)
		if !ok || root != want[0] || inner != want[1] {
			t.Errorf("of(%q) = %q %q %v, want %v", rel, root, inner, ok, want)
		}
	}
	if _, _, ok := h.of(".config/other"); ok {
		t.Error(".config/other is in a root")
	}
	for rel, skip := range map[string]bool{".config": false, ".config/other": true, ".mnt/opt": false, "elsewhere": true} {
		if h.skip(rel) != skip {
			t.Errorf("skip(%q) = %v", rel, !skip)
		}
	}
}
