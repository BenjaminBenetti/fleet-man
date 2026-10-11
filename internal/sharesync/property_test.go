//go:build unix

package sharesync

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"math/rand/v2"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// The property test drives three sides with random edits (files, folders,
// symlinks, modes, deletes, appends to a history log) interleaved with
// partial scans, full flushes, side and hub restarts, crashes between a
// write and its record, and an unreliable side, then heals everything and
// checks:
//
//   - convergence: every side holds the same synced tree;
//   - no data loss: every file content and symlink a side wrote is either in
//     that tree or kept as a conflict copy, unless a side replaced or
//     deleted it while holding it (the user's own doing);
//   - every line appended to the history log is in it, once per append.

// propNames is the small path universe, so edits collide.
var propNames = []string{"f", "g", "a", "a/f", "a/b", "a/b/f", "c/h", "a/x.lock"}

func TestPropertyConvergesWithoutLoss(t *testing.T) {
	// SHARESYNC_SEEDS=N runs N seeds (a deep run); the default keeps the
	// unit job quick.
	seeds, steps := 16, 60
	if testing.Short() {
		seeds, steps = 6, 40
	}
	if n, err := strconv.Atoi(os.Getenv("SHARESYNC_SEEDS")); err == nil && n > 0 {
		seeds, steps = n, 80
	}
	for seed := range seeds {
		t.Run(fmt.Sprint(seed), func(t *testing.T) {
			runProperty(t, uint64(seed), steps)
		})
	}
}

// propState is what the test knows of what was written.
type propState struct {
	t      *testing.T
	r      *rig
	rng    *rand.Rand
	n      int
	faulty *faultyPeer
	// written is every content written; superseded those a side replaced
	// or deleted while holding them.
	written    map[string]bool
	superseded map[string]bool
	lines      []string
	// crashMu guards the crash dice, rolled on the sides' goroutines.
	crashMu  sync.Mutex
	crashRng *rand.Rand
	crashOn  bool
	crashes  int
	trace    bool
}

func runProperty(t *testing.T, seed uint64, steps int) {
	ps := &propState{
		t: t, rng: rand.New(rand.NewPCG(seed, seed^0x9e3779b97f4a7c15)),
		written: map[string]bool{}, superseded: map[string]bool{},
		crashRng: rand.New(rand.NewPCG(seed+1, seed)), trace: os.Getenv("SHARESYNC_TRACE") != "",
	}
	r := newRig(t, "host", "c1")
	for _, id := range r.order {
		r.sides[id].tree.afterWrite = ps.crashHook(id)
	}
	r.onOpen = func(s *rigSide) { s.tree.afterWrite = ps.crashHook(s.info.ID) }
	ps.r = r
	faulty := &faultyPeer{}
	ps.faulty = faulty
	s2 := &rigSide{info: SideInfo{ID: "c2", Location: "c2"}, dir: filepath.Join(r.base, "sides", "c2"), reserved: map[string][]string{}, peer: faulty}
	for _, root := range r.roots {
		if err := os.MkdirAll(filepath.Join(s2.dir, root), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	r.sides["c2"], r.order = s2, append(r.order, "c2")
	r.openSide(s2)
	ps.crashy(true)
	for range steps {
		ps.step()
	}
	// Heal: no faults, no crashes, everything delivered.
	faulty.set(func(f *faultyPeer) { f.down, f.lose, f.fetchFail = false, false, false })
	ps.crashy(false)
	r.hub.Kick("c2")
	r.sync()
	r.sync()
	r.assertConverged()
	ps.checkNoLoss()
	t.Logf("%d steps, %d crashes, %d conflict copies", steps, ps.crashes, len(r.conflicts()))
}

// crashy makes every side crash (stop between a write and its record) now
// and then, or never.
func (ps *propState) crashy(on bool) {
	ps.crashMu.Lock()
	ps.crashOn = on
	ps.crashMu.Unlock()
}

// crashHook is a side's afterWrite.
func (ps *propState) crashHook(id string) func(root, p string) error {
	return func(root, p string) error {
		ps.crashMu.Lock()
		defer ps.crashMu.Unlock()
		if ps.crashOn && ps.crashRng.IntN(25) == 0 {
			ps.crashes++
			ps.logf("crash %s %s/%s", id, root, p)
			return errors.New("crash between the write and its record")
		}
		return nil
	}
}

// logf traces the test's steps when SHARESYNC_TRACE is set.
func (ps *propState) logf(format string, args ...any) {
	if ps.trace {
		ps.t.Logf("%s "+format, append([]any{time.Now().UTC().Format("05.000000")}, args...)...)
	}
}

// step does one random thing.
func (ps *propState) step() {
	r := ps.r
	id := r.order[ps.rng.IntN(len(r.order))]
	root := []string{"tool", "user"}[ps.rng.IntN(2)]
	name := propNames[ps.rng.IntN(len(propNames))]
	k := ps.rng.IntN(20)
	ps.logf("step %d: %s %s/%s", k, id, root, name)
	switch {
	case k < 6:
		ps.edit(id, root, func() { ps.writeFile(id, root, name) })
	case k < 8:
		ps.edit(id, "tool", func() { ps.appendHistory(id) })
	case k < 10:
		ps.edit(id, root, func() { ps.remove(id, root, name) })
	case k < 11:
		ps.edit(id, root, func() { ps.symlink(id, root, name) })
	case k < 12:
		ps.edit(id, root, func() { ps.mkdir(id, root, name) })
	case k < 13:
		ps.edit(id, root, func() {
			if p := r.path(id, root, name); isFile(p) {
				os.Chmod(p, fs.FileMode(0o600+ps.rng.IntN(2)*0o155))
			}
		})
	case k < 15:
		err := r.sides[id].side.scan(context.Background(), root, []string{name})
		ps.logf("  scanned %s %s/%s: %v", id, root, name, err)
	case k < 17:
		r.flushErr(50 * time.Millisecond)
	case k < 18:
		ps.faulty.set(func(f *faultyPeer) {
			f.down = ps.rng.IntN(3) == 0
			f.lose = ps.rng.IntN(4) == 0
			f.fetchFail = ps.rng.IntN(4) == 0
		})
	case k < 19:
		r.restartSide(id)
	default:
		if ps.rng.IntN(3) == 0 {
			r.restartHub()
		}
	}
}

// edit makes a user's edit on a side: what the side holds is looked at and
// replaced with no apply in between (applies take the root's lock to check
// and replace), so what the test marks as replaced knowingly is what the
// user replaced. Without it the test would race the dispatchers on its own
// bookkeeping, and an edit would land in the instant between an apply's
// check and its rename, which a rename-based replace cannot close (see
// writeFile).
func (ps *propState) edit(id, root string, do func()) {
	tr := ps.r.sides[id].tree.roots[root]
	tr.mu.Lock()
	defer tr.mu.Unlock()
	do()
}

// content returns a fresh content to write.
func (ps *propState) content(id string) string {
	ps.n++
	return fmt.Sprintf("%s:%d", id, ps.n)
}

// hold marks what a side holds at p (and below it) as replaced by the user.
func (ps *propState) hold(p string) {
	filepath.WalkDir(p, func(q string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.Type()&fs.ModeSymlink != 0 {
			target, _ := os.Readlink(q)
			ps.superseded["link:"+target] = true
			ps.logf("  supersede link:%s at %s", target, q)
			return nil
		}
		if d.Type().IsRegular() && filepath.Base(q) != "history.jsonl" {
			data, _ := os.ReadFile(q)
			ps.superseded[string(data)] = true
			ps.logf("  supersede %s at %s", data, q)
		}
		return nil
	})
}

// clearParents makes p's parent folders exist on a side, removing (and
// superseding) any file or symlink in the way.
func (ps *propState) clearParents(id, root, name string) {
	r := ps.r
	for _, a := range ancestors(name) {
		p := r.path(id, root, a)
		if info, err := os.Lstat(p); err == nil && !info.IsDir() {
			ps.hold(p)
			os.Remove(p)
		}
	}
	os.MkdirAll(filepath.Dir(r.path(id, root, name)), 0o755)
}

func (ps *propState) writeFile(id, root, name string) {
	p := ps.r.path(id, root, name)
	ps.clearParents(id, root, name)
	if info, err := os.Lstat(p); err == nil {
		ps.hold(p)
		if info.IsDir() || info.Mode()&fs.ModeSymlink != 0 {
			os.RemoveAll(p)
		}
	}
	c := ps.content(id)
	ps.logf("  write %s %s = %s", id, name, c)
	if err := os.WriteFile(p, []byte(c), 0o644); err == nil && ps.r.sides[id].tree.roots[root].synced(name) {
		ps.written[c] = true
	}
}

func (ps *propState) appendHistory(id string) {
	p := ps.r.path(id, "tool", "history.jsonl")
	if info, err := os.Lstat(p); err == nil && !info.Mode().IsRegular() {
		return
	}
	ps.n++
	line := fmt.Sprintf(`{"%s":%d}`, id, ps.n)
	ps.lines = append(ps.lines, line)
	ps.logf("  append %s %s", id, line)
	f, err := os.OpenFile(p, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o644)
	if err != nil {
		ps.t.Fatal(err)
	}
	f.WriteString(line + "\n")
	f.Close()
}

func (ps *propState) remove(id, root, name string) {
	p := ps.r.path(id, root, name)
	ps.hold(p)
	os.RemoveAll(p)
}

func (ps *propState) symlink(id, root, name string) {
	p := ps.r.path(id, root, name)
	ps.clearParents(id, root, name)
	if _, err := os.Lstat(p); err == nil {
		ps.hold(p)
		os.RemoveAll(p)
	}
	target := "target-" + ps.content(id)
	ps.logf("  symlink %s %s -> %s", id, name, target)
	if err := os.Symlink(target, p); err == nil && ps.r.sides[id].tree.roots[root].synced(name) {
		ps.written["link:"+target] = true
	}
}

func (ps *propState) mkdir(id, root, name string) {
	p := ps.r.path(id, root, name)
	ps.clearParents(id, root, name)
	if info, err := os.Lstat(p); err == nil && !info.IsDir() {
		ps.hold(p)
		os.Remove(p)
	}
	os.MkdirAll(p, 0o755)
}

// checkNoLoss checks every content written survives somewhere unless a
// side replaced it while holding it, and every appended line is in the log.
func (ps *propState) checkNoLoss() {
	t, r := ps.t, ps.r
	have := map[string]bool{}
	for _, root := range r.roots {
		for _, v := range r.tree("host", root) {
			switch {
			case strings.HasPrefix(v, "l:"):
				have["link:"+v[2:]] = true
			case strings.HasPrefix(v, "f:"):
				_, content, _ := strings.Cut(v[2:], ":")
				have[content] = true
			}
		}
	}
	all, err := r.hub.Conflicts()
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range all {
		data, err := os.ReadFile(c.File)
		if err != nil {
			t.Fatal(err)
		}
		if c.Entry.Kind == Symlink {
			have["link:"+string(data)] = true
		} else {
			have[string(data)] = true
		}
	}
	var lost []string
	for c := range ps.written {
		if !have[c] && !ps.superseded[c] {
			lost = append(lost, c)
		}
	}
	slices.Sort(lost)
	if len(lost) > 0 {
		t.Errorf("lost %d contents: %q", len(lost), lost)
	}
	history := r.read("host", "tool", "history.jsonl")
	for _, line := range ps.lines {
		if strings.Count(history, line+"\n") != 1 {
			t.Errorf("history.jsonl holds %q %d times", line, strings.Count(history, line+"\n"))
		}
	}
}

func isFile(p string) bool {
	info, err := os.Lstat(p)
	return err == nil && info.Mode().IsRegular()
}
