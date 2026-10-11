//go:build unix

package sharesync

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand/v2"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// The policies' property test: three sides (the daemon host, and two copies
// of one cluster location, one behind an unreliable peer) edit settings
// files (json3: add, change and remove keys, write the '{}' seed, write in
// place so a scan can catch the file half written), a Codex-style
// config.toml (toml3, comments kept around its keys) and a login (logins,
// refreshes, logouts, Claude's cleared login, writes in place), interleaved
// with partial scans, flushes, faults, crashes between a write and its
// record, and side and hub restarts. Healed, it checks:
//
//   - the settings files and the config converge everywhere, and the login
//     within each location, never across;
//   - every file is whole (parses) wherever it is;
//   - every key added and never removed by anyone is there: no merge, seed
//     or torn write loses one, and '{}' never wins;
//   - each location's login is a login made there.

func TestPropertyMergesConverge(t *testing.T) {
	seeds, steps := 12, 70
	if testing.Short() {
		seeds, steps = 4, 40
	}
	if n, err := strconv.Atoi(os.Getenv("SHARESYNC_SEEDS")); err == nil && n > 0 {
		seeds, steps = n, 100
	}
	for seed := range seeds {
		t.Run(fmt.Sprint(seed), func(t *testing.T) {
			runMergesProperty(t, uint64(seed), steps)
		})
	}
}

// mergesProp is what the test knows of what was written.
type mergesProp struct {
	t      *testing.T
	r      *rig
	rng    *rand.Rand
	n      int
	faulty *faultyPeer
	// added are the keys added to each file; removed those someone removed.
	added   map[string]map[string]bool
	removed map[string]map[string]bool

	crashMu  sync.Mutex
	crashRng *rand.Rand
	crashOn  bool
	trace    bool
}

// propSettle is the property test's MergeSettle.
const propSettle = 20 * time.Millisecond

// mergeFiles are the merged files the test edits, root and path.
var mergeFiles = [][2]string{{"tool", "settings.json"}, {"files", ".claude.json"}, {"tool", "config.toml"}}

func runMergesProperty(t *testing.T, seed uint64, steps int) {
	mp := &mergesProp{
		t: t, rng: rand.New(rand.NewPCG(seed, seed^0x5bd1e995)),
		added: map[string]map[string]bool{}, removed: map[string]map[string]bool{},
		crashRng: rand.New(rand.NewPCG(seed+3, seed)), trace: os.Getenv("SHARESYNC_TRACE") != "",
	}
	for _, f := range mergeFiles {
		mp.added[f[1]], mp.removed[f[1]] = map[string]bool{}, map[string]bool{}
	}
	r := mergeRig(t, 0)
	// Writes in place are caught half done by scans unless the tree waits
	// for its merging files to settle.
	r.topts = func(string) TreeOptions { return TreeOptions{Settle: -1, MergeSettle: propSettle} }
	r.addSide(host)
	r.addSide(a1)
	r.onOpen = func(s *rigSide) { s.tree.afterWrite = mp.crashHook(s.info.ID) }
	for _, id := range r.order {
		r.sides[id].tree.afterWrite = mp.crashHook(id)
	}
	mp.r = r
	mp.faulty = r.faulty(a2)
	mp.crashy(true)
	for range steps {
		mp.step()
	}
	mp.faulty.set(func(f *faultyPeer) { f.down, f.deaf, f.lose, f.fetchFail = false, false, false, false })
	mp.crashy(false)
	// Writes caught half done are finished by now (each torn write is
	// completed in its step); once they settle, every side scans and
	// everything is delivered.
	time.Sleep(2 * propSettle)
	r.hub.Kick("a2")
	r.sync()
	time.Sleep(2 * propSettle)
	r.sync()
	mp.check()
}

func (mp *mergesProp) crashy(on bool) {
	mp.crashMu.Lock()
	mp.crashOn = on
	mp.crashMu.Unlock()
}

func (mp *mergesProp) crashHook(id string) func(root, p string) error {
	return func(root, p string) error {
		mp.crashMu.Lock()
		defer mp.crashMu.Unlock()
		if mp.crashOn && mp.crashRng.IntN(30) == 0 {
			mp.logf("crash %s %s/%s", id, root, p)
			return errors.New("crash between the write and its record")
		}
		return nil
	}
}

func (mp *mergesProp) logf(format string, args ...any) {
	if mp.trace {
		mp.t.Logf(format, args...)
	}
}

func (mp *mergesProp) step() {
	r := mp.r
	id := r.order[mp.rng.IntN(len(r.order))]
	switch k := mp.rng.IntN(20); {
	case k < 7:
		f := mergeFiles[mp.rng.IntN(2)]
		mp.editJSON(id, f[0], f[1])
	case k < 9:
		mp.editTOML(id)
	case k < 12:
		mp.login(id)
	case k < 14:
		f := append(mergeFiles, [2]string{"tool", ".credentials.json"})[mp.rng.IntN(4)]
		err := r.sides[id].side.scan(context.Background(), f[0], []string{f[1]})
		mp.logf("scan %s %s/%s: %v", id, f[0], f[1], err)
	case k < 16:
		r.flushErr(50 * time.Millisecond)
	case k < 17:
		mp.faulty.set(func(f *faultyPeer) {
			f.down = mp.rng.IntN(4) == 0
			f.deaf = mp.rng.IntN(3) == 0
			f.lose = mp.rng.IntN(4) == 0
			f.fetchFail = mp.rng.IntN(4) == 0
		})
	case k < 19:
		r.restartSide(id)
	default:
		if mp.rng.IntN(3) == 0 {
			r.restartHub()
		}
	}
}

// rewrite rewrites a side's file as a user (or an agent) does, reading it
// and writing what next makes of it under the root's lock, so no op lands
// between the two (an edit made on what the side no longer holds is the
// user's own doing, as on a shared docker folder). next returns false to
// write nothing. A write in place is made in two: half the content, a scan
// that may catch it so, then the rest.
func (mp *mergesProp) rewrite(id, root, rel string, inPlace bool, next func(current string) (string, bool)) {
	tr := mp.r.sides[id].tree.roots[root]
	tr.mu.Lock()
	content, ok := next(mp.r.read(id, root, rel))
	if !ok {
		tr.mu.Unlock()
		return
	}
	if inPlace {
		mp.r.write(id, root, rel, content[:len(content)/2])
		tr.mu.Unlock()
		mp.r.sides[id].side.scan(context.Background(), root, []string{rel})
		tr.mu.Lock()
	}
	mp.r.write(id, root, rel, content)
	tr.mu.Unlock()
}

// remove deletes a side's file under the root's lock; gone is called with
// what it held.
func (mp *mergesProp) remove(id, root, rel string, gone func(held string)) {
	tr := mp.r.sides[id].tree.roots[root]
	tr.mu.Lock()
	defer tr.mu.Unlock()
	if held := mp.r.read(id, root, rel); held != "<missing>" {
		gone(held)
		os.Remove(mp.r.path(id, root, rel))
	}
}

func (mp *mergesProp) editJSON(id, root, rel string) {
	mp.n++
	n := mp.n
	switch k := mp.rng.IntN(20); {
	case k < 2:
		// The seed: what an instance (or the share's layout) writes where
		// the file is missing, as its symlink target must parse.
		mp.rewrite(id, root, rel, false, func(cur string) (string, bool) {
			mp.logf("%s %s/%s: seed (over %q)", id, root, rel, cur)
			return "{}", cur == "<missing>"
		})
		return
	case k < 3:
		mp.remove(id, root, rel, func(held string) {
			obj := map[string]any{}
			json.Unmarshal([]byte(held), &obj)
			for key := range obj {
				mp.removed[rel][key] = true
			}
			mp.logf("%s %s/%s: delete", id, root, rel)
		})
		return
	}
	mp.rewrite(id, root, rel, mp.rng.IntN(8) == 0, func(cur string) (string, bool) {
		obj := map[string]any{}
		json.Unmarshal([]byte(cur), &obj)
		keys := make([]string, 0, len(obj))
		for key := range obj {
			keys = append(keys, key)
		}
		slices.Sort(keys)
		switch k := mp.rng.IntN(10); {
		case k < 2 && len(keys) > 0:
			key := keys[mp.rng.IntN(len(keys))]
			mp.removed[rel][key] = true
			delete(obj, key)
			mp.logf("%s %s/%s: remove %s", id, root, rel, key)
		case k < 4 && len(keys) > 0:
			key := keys[mp.rng.IntN(len(keys))]
			obj[key] = n
			mp.logf("%s %s/%s: change %s", id, root, rel, key)
		default:
			key := fmt.Sprintf("k%d", n)
			obj[key] = n
			mp.added[rel][key] = true
			mp.logf("%s %s/%s: add %s", id, root, rel, key)
		}
		data, _ := json.MarshalIndent(obj, "", "  ")
		return string(data), true
	})
}

var tomlLine = regexp.MustCompile(`(?m)^(t\d+) = .*\n`)

func (mp *mergesProp) editTOML(id string) {
	mp.n++
	n := mp.n
	mp.rewrite(id, "tool", "config.toml", mp.rng.IntN(8) == 0, func(text string) (string, bool) {
		if _, err := parseTOML([]byte(text)); err != nil {
			text = "# Codex config\n"
		}
		keys := tomlLine.FindAllStringSubmatch(text, -1)
		switch k := mp.rng.IntN(10); {
		case k < 3 && len(keys) > 0:
			key := keys[mp.rng.IntN(len(keys))][1]
			mp.removed["config.toml"][key] = true
			text = regexp.MustCompile(`(?m)^`+key+` = .*\n`).ReplaceAllString(text, "")
			mp.logf("%s config.toml: remove %s", id, key)
		case k < 5 && len(keys) > 0:
			key := keys[mp.rng.IntN(len(keys))][1]
			text = regexp.MustCompile(`(?m)^`+key+` = .*\n`).ReplaceAllString(text, fmt.Sprintf("%s = %d # changed\n", key, n))
			mp.logf("%s config.toml: change %s", id, key)
		default:
			key := fmt.Sprintf("t%d", n)
			mp.added["config.toml"][key] = true
			text += fmt.Sprintf("# about %s\n%s = %d\n", key, key, n)
			mp.logf("%s config.toml: add %s", id, key)
		}
		return text, true
	})
}

func (mp *mergesProp) login(id string) {
	loc := mp.r.sides[id].info.Location
	mp.n++
	switch k := mp.rng.IntN(10); {
	case k < 2:
		mp.remove(id, "tool", ".credentials.json", func(string) { mp.logf("%s login: logout", id) })
	case k < 4:
		mp.rewrite(id, "tool", ".credentials.json", false, func(string) (string, bool) {
			mp.logf("%s login: cleared", id)
			return clearedClaudeLogin, true
		})
	default:
		tok := fmt.Sprintf("%s-%d", loc, mp.n)
		mp.rewrite(id, "tool", ".credentials.json", mp.rng.IntN(6) == 0, func(string) (string, bool) {
			mp.logf("%s login: %s", id, tok)
			return claudeLogin(tok), true
		})
	}
}

// lost returns the keys added to a file and never removed that keys lacks.
func (mp *mergesProp) lost(file string, keys map[string]bool) []string {
	var lost []string
	for k := range mp.added[file] {
		if !mp.removed[file][k] && !keys[k] {
			lost = append(lost, k)
		}
	}
	slices.Sort(lost)
	return lost
}

func (mp *mergesProp) check() {
	t, r := mp.t, mp.r
	// Convergence: the merged files everywhere, the login per location.
	for _, f := range mergeFiles {
		want := r.read(r.order[0], f[0], f[1])
		for _, id := range r.order[1:] {
			if got := r.read(id, f[0], f[1]); got != want {
				t.Errorf("%s/%s: %s holds %q, %s holds %q", f[0], f[1], id, got, r.order[0], want)
			}
		}
	}
	if c1, c2 := r.read("a1", "tool", ".credentials.json"), r.read("a2", "tool", ".credentials.json"); c1 != c2 {
		t.Errorf("k1's copies differ: %q vs %q", c1, c2)
	}
	for _, id := range r.order {
		login := r.read(id, "tool", ".credentials.json")
		if login == "<missing>" || login == clearedClaudeLogin {
			continue
		}
		if !strings.Contains(login, `"at-`+r.sides[id].info.Location+`-`) {
			t.Errorf("%s (%s) holds another location's login: %s", id, r.sides[id].info.Location, login)
		}
		if wholeLogin(".credentials.json", []byte(login)) != nil {
			t.Errorf("%s holds a torn login: %q", id, login)
		}
	}
	// Whole files, and no key lost.
	for _, f := range mergeFiles {
		text := r.read("host", f[0], f[1])
		var keys map[string]bool
		if text == "<missing>" {
			if lost := mp.lost(f[1], keys); len(lost) > 0 {
				t.Errorf("%s is gone, with keys never removed: %v", f[1], lost)
			}
			continue
		}
		if f[1] == "config.toml" {
			if _, err := parseTOML([]byte(text)); err != nil {
				t.Errorf("config.toml does not parse (%v): %q", err, text)
				continue
			}
			keys = map[string]bool{}
			for _, m := range tomlLine.FindAllStringSubmatch(text, -1) {
				keys[m[1]] = true
			}
			v, _ := parseTOML([]byte(text))
			if len(v.keys) != len(keys) {
				t.Errorf("config.toml's keys are not each on their own line:\n%s", text)
			}
		} else {
			obj := map[string]any{}
			if err := json.Unmarshal([]byte(text), &obj); err != nil {
				t.Errorf("%s does not parse (%v): %q", f[1], err, text)
				continue
			}
			keys = map[string]bool{}
			for k := range obj {
				keys[k] = true
			}
		}
		if lost := mp.lost(f[1], keys); len(lost) > 0 {
			t.Errorf("%s lost %d keys: %v\n%s", f[1], len(lost), lost, text)
		}
	}
	if t.Failed() {
		t.Logf("conflicts: %q", r.conflicts())
	}
}
