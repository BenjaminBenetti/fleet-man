//go:build unix

package sharesync

import (
	"bytes"
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// remoteSpoke is a spoke of a rig's hub over a connection, as a pod's would
// be: its tree in its own folders, the hub's end served by ServeRemote.
type remoteSpoke struct {
	dir   string
	tree  *Tree
	spoke *Spoke
	// bulks counts the bulk connections the hub opened.
	bulks int
	mu    sync.Mutex
	done  chan error
	stop  func()
}

// addRemote attaches a spoke to r's hub as info, over a pipe.
func addRemote(t *testing.T, r *rig, info SideInfo, opts RemoteOptions) *remoteSpoke {
	t.Helper()
	rs := &remoteSpoke{dir: filepath.Join(r.base, "remote", info.ID), done: make(chan error, 2)}
	var roots []TreeRoot
	for _, root := range r.roots {
		d := filepath.Join(rs.dir, root)
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
		roots = append(roots, TreeRoot{Name: root, Dir: d})
	}
	tree, err := OpenTree(roots, TreeOptions{StateDir: filepath.Join(r.base, "remote-state", info.ID), Table: r.table, Settle: -1, Logger: r.logs.logger().With("tree", "remote")})
	if err != nil {
		t.Fatalf("OpenTree: %v", err)
	}
	rs.tree = tree
	hubEnd, spokeEnd := net.Pipe()
	rs.spoke = NewSpoke(spokeEnd, tree, SpokeOptions{
		Info:   SideInfo{ID: "pod"},
		Side:   SideOptions{NoWatch: true, Poll: time.Hour, Rescan: time.Hour, Debounce: time.Millisecond, Again: 5 * time.Millisecond},
		Lease:  7,
		Logger: r.logs.logger().With("spoke", info.ID),
	})
	ctx, cancel := context.WithCancel(context.Background())
	if opts.OpenBulk == nil {
		opts.OpenBulk = func(context.Context) (net.Conn, error) {
			a, b := net.Pipe()
			rs.mu.Lock()
			rs.bulks++
			rs.mu.Unlock()
			go func() { _ = rs.spoke.ServeBulk(ctx, b) }()
			return a, nil
		}
	}
	opts.Logger = r.logs.logger().With("remote", info.ID)
	go func() { rs.done <- ServeRemote(ctx, hubEnd, r.hub, info, opts) }()
	go func() { rs.done <- rs.spoke.Run(ctx) }()
	rs.stop = func() {
		cancel()
		<-rs.done
		<-rs.done
		tree.Close()
	}
	t.Cleanup(func() {
		if rs.stop != nil {
			rs.stop()
		}
	})
	return rs
}

func (rs *remoteSpoke) write(t *testing.T, root, rel, content string) {
	t.Helper()
	p := filepath.Join(rs.dir, root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func (rs *remoteSpoke) read(root, rel string) string {
	data, err := os.ReadFile(filepath.Join(rs.dir, root, filepath.FromSlash(rel)))
	if err != nil {
		return "<" + err.Error() + ">"
	}
	return string(data)
}

// waitAttached waits until the hub has the side.
func waitAttached(t *testing.T, r *rig, id string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for r.hub.side(id) == nil {
		if time.Now().After(deadline) {
			t.Fatalf("%s never attached\n%s", id, r.logs.String())
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// TestRemoteSpokeConverges syncs a host side and a spoke over the wire both
// ways, small content inline and large content on bulk connections.
func TestRemoteSpokeConverges(t *testing.T) {
	r := newRig(t, "host")
	rs := addRemote(t, r, SideInfo{ID: "cluster:share", Location: "cluster"}, RemoteOptions{})
	waitAttached(t, r, "cluster:share")

	big := strings.Repeat("0123456789abcdef", (InlineMax/16)*3)
	r.write("host", "tool", "settings/a.txt", "from the host")
	r.write("host", "user", "big-host.bin", big)
	rs.write(t, "tool", "notes/b.txt", "from the pod")
	rs.write(t, "user", "big-pod.bin", big+"pod")
	r.sync()

	if got := rs.read("tool", "settings/a.txt"); got != "from the host" {
		t.Errorf("the spoke has %q, want the host's file", got)
	}
	if got := rs.read("user", "big-host.bin"); got != big {
		t.Errorf("the spoke's copy of the large host file is %d bytes, want %d", len(got), len(big))
	}
	if got := r.read("host", "tool", "notes/b.txt"); got != "from the pod" {
		t.Errorf("the host has %q, want the pod's file", got)
	}
	if got := r.read("host", "user", "big-pod.bin"); got != big+"pod" {
		t.Errorf("the host's copy of the large pod file is %d bytes, want %d", len(got), len(big)+3)
	}
	rs.mu.Lock()
	bulks := rs.bulks
	rs.mu.Unlock()
	if bulks < 2 {
		t.Errorf("%d bulk connections, want one each way at least for the large files", bulks)
	}

	// A later change on either side follows, and a delete propagates.
	if err := os.Remove(r.path("host", "tool", "settings/a.txt")); err != nil {
		t.Fatal(err)
	}
	rs.write(t, "tool", "notes/b.txt", "edited in the pod")
	r.sync()
	if got := rs.read("tool", "settings/a.txt"); !strings.Contains(got, "no such file") {
		t.Errorf("a host delete left the spoke with %q", got)
	}
	if got := r.read("host", "tool", "notes/b.txt"); got != "edited in the pod" {
		t.Errorf("the host has %q after the pod's edit", got)
	}
}

// TestRemoteSpokeAcceptRefusesALease: the caller's Accept refuses a spoke
// (an older lease than one already seen), which is never attached.
func TestRemoteSpokeAcceptRefusesALease(t *testing.T) {
	r := newRig(t, "host")
	var saw uint64
	rs := addRemote(t, r, SideInfo{ID: "cluster:share", Location: "cluster"}, RemoteOptions{Accept: func(lease uint64) error {
		saw = lease
		return errors.New("lease 7 is older than lease 9")
	}})
	for i := 0; i < 2; i++ {
		select {
		case err := <-rs.done:
			if err == nil {
				t.Fatal("a refused spoke ended without an error")
			}
		case <-time.After(5 * time.Second):
			t.Fatal("a refused spoke never ended")
		}
	}
	rs.done <- nil
	rs.done <- nil
	if saw != 7 {
		t.Errorf("Accept saw lease %d, want the spoke's 7", saw)
	}
	if r.hub.side("cluster:share") != nil {
		t.Error("a refused spoke was attached")
	}
}

// fakeSpoke is a hand-driven spoke end, for what a real one never sends.
func fakeSpoke(t *testing.T, r *rig, info SideInfo) (*wire, chan error) {
	t.Helper()
	hubEnd, spokeEnd := net.Pipe()
	done := make(chan error, 1)
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		done <- ServeRemote(ctx, hubEnd, r.hub, info, RemoteOptions{HelloTimeout: 2 * time.Second, Logger: r.logs.logger()})
	}()
	w := newWire(spokeEnd)
	go func() {
		for {
			m, err := w.recv()
			if err != nil {
				w.fail(err)
				return
			}
			if answers[m.T] {
				w.deliver(m)
			}
		}
	}()
	t.Cleanup(func() {
		cancel()
		w.fail(nil)
	})
	return w, done
}

// TestRemoteHubBoundsTheSpokesHello: a hello claiming a rev past any real
// one (which Attach would adopt as the root's) is refused, and nothing is
// attached.
func TestRemoteHubBoundsTheSpokesHello(t *testing.T) {
	for _, tc := range []struct {
		name  string
		hello Hello
	}{
		{"an absurd rev", Hello{Roots: []RootHello{{Root: "tool", Epoch: "e", Held: []Held{{Path: "a", Rev: 1 << 62, Entry: Entry{Kind: File, Hash: sum("x"), Size: 1}}}}}}},
		{"a root outside the tree", Hello{Roots: []RootHello{{Root: "../etc"}}}},
		{"an oversized epoch", Hello{Roots: []RootHello{{Root: "tool", Epoch: strings.Repeat("e", 100)}}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newRig(t, "host")
			w, done := fakeSpoke(t, r, SideInfo{ID: "cluster:share", Location: "cluster"})
			a, err := w.call(context.Background(), 0, wireMsg{T: msgHello, Hello: &tc.hello})
			if err != nil {
				t.Fatalf("hello: %v", err)
			}
			if a.T != msgRefused {
				t.Fatalf("the hub answered %s, want refused", a.T)
			}
			if err := <-done; err == nil {
				t.Fatal("ServeRemote took the hello")
			}
			if r.hub.side("cluster:share") != nil {
				t.Fatal("the spoke was attached")
			}
		})
	}
}

// TestRemoteHubBoundsReportsAndNeeds: once attached, a report claiming an
// absurd base, and an ask for content of a batch the hub never sent, are
// refused rather than handed to the hub.
func TestRemoteHubBoundsReportsAndNeeds(t *testing.T) {
	r := newRig(t, "host")
	r.write("host", "tool", "secret.txt", "the host's")
	r.sync()
	w, _ := fakeSpoke(t, r, SideInfo{ID: "cluster:share", Location: "cluster"})
	a, err := w.call(context.Background(), 0, wireMsg{T: msgHello, Hello: &Hello{}})
	if err != nil || a.T != msgWelcome {
		t.Fatalf("hello: %v %s", err, a.T)
	}
	epoch := a.Welcome.Epochs["tool"]
	_, err = w.call(context.Background(), 0, wireMsg{T: msgReport, Report: &Report{Root: "tool", Epoch: epoch, Changes: []Change{{Path: "x", Base: 1 << 60, Entry: Entry{Kind: File, Hash: sum("x"), Size: 1}}}}})
	if err == nil || !strings.Contains(err.Error(), "base rev") {
		t.Fatalf("a report with an absurd base: %v, want refused", err)
	}
	op := Op{Path: "secret.txt", Rev: 1, Entry: Entry{Kind: File, Size: int64(len("the host's")), Hash: sum("the host's")}}
	_, err = w.call(context.Background(), 0, wireMsg{T: msgNeed, Need: &wireNeed{Batch: 999, Op: op}})
	if err == nil {
		t.Fatal("the hub served content for a batch it never sent")
	}
}

// TestChunkHello splits a large hello into bounded messages that add back up
// to it.
func TestChunkHello(t *testing.T) {
	var held []Held
	for i := 0; i < helloChunk*2+5; i++ {
		held = append(held, Held{Path: "p", Rev: uint64(i)})
	}
	h := Hello{Roots: []RootHello{{Root: "a", Epoch: "ea", Held: held}, {Root: "b", Epoch: "eb"}, {Root: "c", Held: held[:3]}}}
	chunks := chunkHello(h)
	if len(chunks) != 3 {
		t.Fatalf("%d chunks, want 3", len(chunks))
	}
	got := map[string]int{}
	epochs := map[string]string{}
	for _, c := range chunks {
		n := 0
		for _, rh := range c.Roots {
			n += len(rh.Held)
			got[rh.Root] += len(rh.Held)
			epochs[rh.Root] = rh.Epoch
		}
		if n > helloChunk {
			t.Fatalf("a chunk holds %d paths, over %d", n, helloChunk)
		}
	}
	if got["a"] != len(held) || got["c"] != 3 || epochs["a"] != "ea" || epochs["b"] != "eb" {
		t.Fatalf("chunks add up to %v (epochs %v)", got, epochs)
	}
	if empty := chunkHello(Hello{}); len(empty) != 1 {
		t.Fatalf("an empty hello is %d chunks, want 1", len(empty))
	}
}

// TestWireMessagesAreBounded: a message over the wire's bound is refused on
// both ends.
func TestWireMessagesAreBounded(t *testing.T) {
	var buf bytes.Buffer
	if err := writeMsg(&buf, wireMsg{T: msgData, Data: make([]byte, maxWireMessage)}); err == nil {
		t.Fatal("writeMsg sent an oversized message")
	}
	buf.Reset()
	buf.Write([]byte{0xff, 0xff, 0xff, 0xff})
	var m wireMsg
	if err := readMsg(&buf, &m); err == nil {
		t.Fatal("readMsg took an oversized length")
	}
}
