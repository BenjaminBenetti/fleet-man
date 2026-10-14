//go:build unix

package sharesync

import (
	"context"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/BenjaminBenetti/fleet-man/internal/mounts/syncpolicy"
)

// locationMerger is a policy of location scope on the priority lane, as part
// B's credentials are, deciding like mirror.
type locationMerger struct{ Mirror }

func (locationMerger) Scope() Scope { return ScopeLocation }
func (locationMerger) Lane() Lane   { return LanePriority }

// scopeRig is a rig whose tool root keeps login.json per location.
func scopeRig(t *testing.T, policy Merger, sides ...string) *rig {
	t.Helper()
	r := &rig{t: t, base: t.TempDir(), roots: []string{"tool"}, sides: map[string]*rigSide{}, logs: &logBuffer{}}
	r.table = syncpolicy.NewTable(syncpolicy.Root{
		Path: "tool", Tool: "Tool", Default: syncpolicy.Mirror,
		Rules: []syncpolicy.Rule{{Pattern: "login.json", Policy: syncpolicy.Credential}, {Pattern: "settings.json", Policy: syncpolicy.JSON3}},
	})
	pol := DefaultPolicies()
	pol[syncpolicy.Credential] = policy
	pol[syncpolicy.JSON3] = policy
	r.hopts = HubOptions{NormalDelay: time.Millisecond, BulkDelay: time.Millisecond, Logger: r.logs.logger(), Policies: pol}
	r.topts = func(string) TreeOptions { return TreeOptions{Settle: -1, MergeSettle: -1} }
	r.openHub()
	t.Cleanup(r.close)
	for i := 0; i+1 < len(sides); i += 2 {
		r.addSide(SideInfo{ID: sides[i], Location: sides[i+1]})
	}
	return r
}

// TestLocationScopeStaysInItsLocation checks a path of location scope syncs
// live among the copies of one location and never crosses to another, in
// either direction, deletes included, while the rest of the root syncs
// everywhere.
func TestLocationScopeStaysInItsLocation(t *testing.T) {
	r := scopeRig(t, locationMerger{}, "host", "host", "a1", "k1", "a2", "k1", "b1", "k2")
	r.write("a1", "tool", "login.json", "k1 login")
	r.write("host", "tool", "login.json", "host login")
	r.write("b1", "tool", "shared", "everywhere")
	r.sync()
	want := map[string]string{"host": "host login", "a1": "k1 login", "a2": "k1 login", "b1": "<missing>"}
	for id, w := range want {
		if got := r.read(id, "tool", "login.json"); got != w {
			t.Errorf("%s login.json = %q, want %q", id, got, w)
		}
		if got := r.read(id, "tool", "shared"); got != "everywhere" {
			t.Errorf("%s shared = %q", id, got)
		}
	}
	// A logout in k1 reaches k1's other copy only.
	os.Remove(r.path("a2", "tool", "login.json"))
	r.sync()
	if got := r.read("a1", "tool", "login.json"); got != "<missing>" {
		t.Errorf("a1 login.json after a2 logged out = %q", got)
	}
	if got := r.read("host", "tool", "login.json"); got != "host login" {
		t.Errorf("host login.json after k1 logged out = %q", got)
	}
	// Each location's state is its own: two locations, two records.
	if st := r.hub.Status(); st.Roots[0].Files != 2 {
		t.Errorf("tool holds %d files, want shared and the host's login", st.Roots[0].Files)
	}
}

// TestPriorityLaneSkipsTheDelay checks a change on the priority lane goes at
// once while the normal lane waits out its delay.
func TestPriorityLaneSkipsTheDelay(t *testing.T) {
	r := scopeRig(t, locationMerger{})
	r.hub.Close()
	r.hopts.NormalDelay = time.Hour
	r.openHub()
	r.addSide(SideInfo{ID: "a1", Location: "k1"})
	r.addSide(SideInfo{ID: "a2", Location: "k1"})
	r.write("a1", "tool", "login.json", "now")
	r.write("a1", "tool", "other", "later")
	if err := r.sides["a1"].side.Rescan(context.Background()); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for r.read("a2", "tool", "login.json") != "now" && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if got := r.read("a2", "tool", "login.json"); got != "now" {
		t.Fatalf("the priority change did not arrive: %q", got)
	}
	if got := r.read("a2", "tool", "other"); got != "<missing>" {
		t.Errorf("a normal change skipped its delay: %q", got)
	}
}

// baseMerger is a three-way policy (as json3 is): it keeps two earlier
// contents and merges concurrent changes with the base they were made on.
type baseMerger struct {
	Mirror
	mu    sync.Mutex
	bases []string
}

func (*baseMerger) History() int { return 2 }

func (b *baseMerger) Merge(ctx context.Context, in *MergeInput) (MergeResult, error) {
	if in.Fast || in.Current == nil || in.Current.Entry.Kind != File || in.Incoming.Entry.Kind != File {
		return MergeResult{Decision: Accept}, nil
	}
	base, ok, err := in.Base(ctx)
	if err != nil {
		return MergeResult{}, err
	}
	cur, err := in.Current.Content(ctx)
	if err != nil {
		return MergeResult{}, err
	}
	inc, err := in.Incoming.Content(ctx)
	if err != nil {
		return MergeResult{}, err
	}
	b.mu.Lock()
	b.bases = append(b.bases, string(base))
	b.mu.Unlock()
	if !ok {
		return MergeResult{Decision: Keep, Conflict: true}, nil
	}
	return MergeResult{Decision: Merged, Content: []byte(string(base) + "|" + string(cur) + "|" + string(inc))}, nil
}

// TestMergersGetTheBase checks a policy keeping history is handed the
// content a concurrent change was made on, and its merged content reaches
// every copy, the reporting one included, and is the base of the next
// concurrent change.
func TestMergersGetTheBase(t *testing.T) {
	m := &baseMerger{}
	r := scopeRig(t, m, "host", "host", "c1", "k1")
	r.write("host", "tool", "settings.json", "v1")
	r.sync()
	r.write("host", "tool", "settings.json", "host-v2")
	r.write("c1", "tool", "settings.json", "c1-v2")
	r.sync()
	r.assertConverged()
	m.mu.Lock()
	bases := append([]string(nil), m.bases...)
	m.mu.Unlock()
	if len(bases) != 1 || bases[0] != "v1" {
		t.Fatalf("bases handed to the merge = %q, want v1", bases)
	}
	got := r.read("host", "tool", "settings.json")
	if got != "v1|host-v2|c1-v2" && got != "v1|c1-v2|host-v2" {
		t.Errorf("merged settings.json = %q", got)
	}

	// Both copies hold the merge now; their next edits meet on it.
	r.write("host", "tool", "settings.json", "host-v3")
	r.write("c1", "tool", "settings.json", "c1-v3")
	r.sync()
	r.assertConverged()
	m.mu.Lock()
	bases = append([]string(nil), m.bases...)
	m.mu.Unlock()
	if len(bases) != 2 || bases[1] != got {
		t.Errorf("bases handed to the merges = %q, want the merged %q second", bases, got)
	}
}
