//go:build unix

package safefs

import (
	"encoding/binary"
	"io/fs"
	"math/rand/v2"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// The race harness runs a program of safefs operations on a tree while an
// attacker goroutine keeps swapping the tree's folders and files for
// symlinks (to a folder and a file outside the tree, absolute and relative,
// to a sentinel folder inside it, dangling) and back. Whatever the
// interleaving, nothing outside the tree and nothing in the sentinel folder
// may change: either would mean an operation followed a link.
//
//	base/
//	  outside/   secret, sub/secret
//	  root/      a/{b/{c/}, x}, b/{x}, x, inside/{secret}

// racePaths are the paths programs name. inside/ is never one of them: only
// links reach it.
var racePaths = []string{"a", "b", "x", "y", "a/b", "a/x", "b/x", "b/a", "a/b/c", "a/b/x", "b/a/x", "a/b/c/x"}

// raceAttackers is how many attacker goroutines run beside each program.
const raceAttackers = 2

// attackVictims are the entries the attacker swaps for links.
var attackVictims = []string{"a", "b", "x", "a/b", "a/x", "b/x", "a/b/c"}

// raceOps are the operations a program byte selects; p and q are paths from
// racePaths, k is a spare byte.
var raceOps = []func(rt *raceTree, r *Root, p, q string, k byte) error{
	func(_ *raceTree, r *Root, p, _ string, _ byte) error {
		_, err := r.WriteFile(p, strings.NewReader("written"), 0o644)
		return err
	},
	func(_ *raceTree, r *Root, p, _ string, _ byte) error {
		return writeTo(r.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644))
	},
	func(_ *raceTree, r *Root, p, _ string, _ byte) error {
		return writeTo(r.OpenFile(p, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o644))
	},
	func(_ *raceTree, r *Root, p, _ string, _ byte) error { _, err := r.ReadFile(p); return err },
	func(_ *raceTree, r *Root, p, _ string, _ byte) error { return r.Mkdir(p, 0o755) },
	func(_ *raceTree, r *Root, p, _ string, _ byte) error { return r.MkdirAll(p+"/n/m", 0o755) },
	func(_ *raceTree, r *Root, p, _ string, _ byte) error { return r.Remove(p) },
	func(_ *raceTree, r *Root, p, _ string, _ byte) error { return r.RemoveAll(p) },
	func(_ *raceTree, r *Root, p, q string, _ byte) error { return r.Rename(p, q) },
	func(rt *raceTree, r *Root, p, _ string, k byte) error {
		targets := []string{"nowhere", "../outside", rt.outside, "inside", filepath.Join(rt.outside, "secret")}
		return r.Symlink(targets[int(k)%len(targets)], p)
	},
	func(_ *raceTree, r *Root, p, _ string, k byte) error {
		return r.Chmod(p, []fs.FileMode{0o700, 0o755}[k%2])
	},
	func(_ *raceTree, r *Root, p, _ string, _ byte) error {
		return r.Chtimes(p, time.Unix(1, 0), time.Unix(1, 0))
	},
	func(_ *raceTree, r *Root, p, _ string, _ byte) error { return r.Lchown(p, os.Getuid(), os.Getgid()) },
	func(_ *raceTree, r *Root, p, _ string, _ byte) error { _, err := r.ReadDir(p); return err },
	func(_ *raceTree, r *Root, p, _ string, _ byte) error {
		return r.WalkDir(p, func(_ string, _ fs.DirEntry, _ error) error { return nil })
	},
	func(_ *raceTree, r *Root, p, _ string, _ byte) error {
		if _, err := r.Lstat(p); err != nil {
			return err
		}
		_, err := r.Readlink(p)
		return err
	},
	func(_ *raceTree, r *Root, p, _ string, _ byte) error {
		sub, err := r.OpenRoot(p)
		if err != nil {
			return err
		}
		defer sub.Close()
		if err := sub.MkdirAll("s", 0o755); err != nil {
			return err
		}
		_, err = sub.WriteFile("s/x", strings.NewReader("sub"), 0o644)
		return err
	},
}

// raceTree is one program's tree.
type raceTree struct {
	root, outside, inside string
}

func newRaceTree(t testing.TB) *raceTree {
	base := t.TempDir()
	rt := &raceTree{
		root:    filepath.Join(base, "root"),
		outside: filepath.Join(base, "outside"),
	}
	rt.inside = filepath.Join(rt.root, "inside")
	for _, d := range []string{rt.outside + "/sub", rt.root + "/a/b/c", rt.root + "/b", rt.inside} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for _, f := range []string{rt.outside + "/secret", rt.outside + "/sub/secret", rt.root + "/a/x", rt.root + "/b/x", rt.root + "/x", rt.inside + "/secret"} {
		if err := os.WriteFile(f, []byte("secret"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return rt
}

// runRaceProgram runs program (three bytes per operation, plus the attacker's
// seed in the first eight) against a fresh tree under attack, and fails t if
// anything outside the tree or in the sentinel folder changed.
func runRaceProgram(t testing.TB, program []byte, portable bool) {
	rt := newRaceTree(t)
	outsideBefore, err := snapshot(rt.outside)
	if err != nil {
		t.Fatal(err)
	}
	insideBefore, err := snapshot(rt.inside)
	if err != nil {
		t.Fatal(err)
	}
	r, err := Open(rt.root, Options{})
	if err != nil {
		t.Fatal(err)
	}
	r.portable = portable
	defer r.Close()

	var seed [8]byte
	copy(seed[:], program)
	stop := make(chan struct{})
	var attackers sync.WaitGroup
	for i := range raceAttackers {
		attackers.Add(1)
		go func() {
			defer attackers.Done()
			rt.attack(stop, binary.LittleEndian.Uint64(seed[:])+uint64(i))
		}()
	}
	for i := 0; i+3 <= len(program); i += 3 {
		op := raceOps[int(program[i])%len(raceOps)]
		p := racePaths[int(program[i+1])%len(racePaths)]
		q := racePaths[int(program[i+2])%len(racePaths)]
		op(rt, r, p, q, program[i+2]) // errors are expected under attack
	}
	close(stop)
	attackers.Wait()

	if after, err := snapshot(rt.outside); err != nil || after != outsideBefore {
		t.Fatalf("program %x changed the folder outside the root (%v):\n%s\nwas\n%s", program, err, after, outsideBefore)
	}
	if after, err := snapshot(rt.inside); err != nil || after != insideBefore {
		t.Fatalf("program %x went through a link into inside/ (%v):\n%s\nwas\n%s", program, err, after, insideBefore)
	}
}

// attack swaps victims for symlinks until stop closes. It works through
// folder handles opened O_NOFOLLOW, so it never follows a link itself (a
// program may have left one at a folder above a victim).
func (rt *raceTree) attack(stop <-chan struct{}, seed uint64) {
	rootFD, err := unix.Open(rt.root, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return
	}
	defer unix.Close(rootFD)
	rng := rand.New(rand.NewPCG(seed, seed^0x5afe))
	for {
		select {
		case <-stop:
			return
		default:
		}
		victim := attackVictims[rng.IntN(len(attackVictims))]
		depth := strings.Count(victim, "/")
		targets := []string{
			rt.outside,
			filepath.Join(rt.outside, "secret"),
			strings.Repeat("../", depth+1) + "outside",
			rt.inside,
			strings.Repeat("../", depth) + "inside",
			strings.Repeat("../", depth) + "inside/secret",
			"nowhere",
		}
		target := targets[rng.IntN(len(targets))]
		dirFD, name := rootFD, victim
		if i := strings.LastIndexByte(victim, '/'); i >= 0 {
			fd, ok := openNoFollow(rootFD, victim[:i])
			if !ok {
				continue
			}
			dirFD, name = fd, victim[i+1:]
		}
		rt.flip(rng, dirFD, name, target)
		if dirFD != rootFD {
			unix.Close(dirFD)
		}
	}
}

// flip swaps the entry name of dirFD with a link to target a few times and
// puts it back. Where the system can exchange two entries atomically the
// path flips between the entry and the link with no gap, which is what a
// time-of-check race needs; elsewhere the entry is renamed aside and the
// link put in its place. Programs change the tree meanwhile, so flip
// tolerates every failure.
func (rt *raceTree) flip(rng *rand.Rand, dirFD int, name, target string) {
	link := name + ".atk"
	unix.Unlinkat(dirFD, link, 0)
	if unix.Symlinkat(target, dirFD, link) != nil {
		return
	}
	defer unix.Unlinkat(dirFD, link, 0)
	if exchangeAt(dirFD, name, link) == nil {
		for range 1 + rng.IntN(64) {
			exchangeAt(dirFD, name, link)
			exchangeAt(dirFD, name, link)
		}
		exchangeAt(dirFD, name, link)
		return
	}
	aside := name + ".aside"
	moved := unix.Renameat(dirFD, name, dirFD, aside) == nil
	if unix.Renameat(dirFD, link, dirFD, name) == nil {
		for range rng.IntN(8) {
			runtime.Gosched()
		}
		unix.Unlinkat(dirFD, name, 0)
	}
	if moved {
		unix.Renameat(dirFD, aside, dirFD, name)
	}
}

// openNoFollow opens the folder rel below dirFD without following a link.
func openNoFollow(dirFD int, rel string) (int, bool) {
	fd := dirFD
	for _, elem := range strings.Split(rel, "/") {
		next, err := unix.Openat(fd, elem, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		if fd != dirFD {
			unix.Close(fd)
		}
		if err != nil {
			return -1, false
		}
		fd = next
	}
	return fd, true
}

func writeTo(f *os.File, err error) error {
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.WriteString("+")
	return err
}

// TestSymlinkSwapRace runs random programs under attack on each walk: the
// fixed-seed share of what FuzzSymlinkSwap explores in CI.
func TestSymlinkSwapRace(t *testing.T) {
	programs := 1500
	if testing.Short() {
		programs = 100
	}
	eachWalk(t, func(t *testing.T, portable bool) {
		rng := rand.New(rand.NewPCG(1, 2))
		for range programs {
			program := make([]byte, 8+3*24)
			for i := range program {
				program[i] = byte(rng.Uint32())
			}
			runRaceProgram(t, program, portable)
		}
	})
}

// FuzzSymlinkSwap explores programs under attack. The macOS and Linux
// workflows run it with -fuzz for a bounded time; the first byte's low bit
// picks the portable walk, so Linux fuzzes both.
func FuzzSymlinkSwap(f *testing.F) {
	for _, seed := range [][]byte{
		{0, 0, 0, 0, 0, 0, 0, 0, 0, 4, 0, 1, 5, 0},
		{1, 7, 7, 7, 7, 7, 7, 7, 8, 4, 5, 0, 9, 3, 7, 1, 0},
		{2, 2, 2, 2, 2, 2, 2, 2, 9, 0, 1, 0, 0, 0, 13, 0, 0, 7, 0, 0, 16, 1, 0},
		{3, 3, 3, 3, 3, 3, 3, 3, 10, 4, 0, 11, 5, 0, 12, 6, 0, 14, 4, 0, 15, 0, 0, 2, 9, 0},
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, program []byte) {
		if len(program) > 8+3*64 {
			program = program[:8+3*64]
		}
		portable := len(program) > 0 && program[0]&1 == 1
		if !portable && !FastPath() {
			portable = true
		}
		runRaceProgram(t, program, portable)
	})
}
