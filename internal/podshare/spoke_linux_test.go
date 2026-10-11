//go:build linux

package podshare

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/BenjaminBenetti/fleet-man/internal/mounts/syncpolicy"
	"github.com/BenjaminBenetti/fleet-man/internal/podwire"
	"github.com/BenjaminBenetti/fleet-man/internal/sharesync"
)

func TestLayoutRoots(t *testing.T) {
	table := syncpolicy.Defaults()
	got := LayoutRoots(claudeLayout(-1, -1).Entries, table.Root)
	want := []SpokeRoot{
		{Name: ".claude"},
		{Name: ".mnt", Reserved: []string{"opt/data"}},
		{Name: "files", Reserved: []string{".claude.json", "claude-settings.local.json", "plain.txt"}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("LayoutRoots =\n%+v\nwant\n%+v", got, want)
	}
	nested := LayoutRoots([]Entry{{Path: "a", Dir: true}, {Path: "a/b", Dir: true}}, func(p string) (syncpolicy.Root, string, bool) { return syncpolicy.Root{Path: p}, "", true })
	if len(nested) != 2 || !reflect.DeepEqual(nested[0].Skip, []string{"b"}) {
		t.Fatalf("a root inside another is not skipped by it: %+v", nested)
	}
}

func TestSpokeSpecValidate(t *testing.T) {
	good := SpokeSpec{V: SpokeVersion, Roots: []SpokeRoot{{Name: ".claude"}, {Name: ".mnt", Reserved: []string{"opt/data"}}}, UID: 1000, GID: 1000}
	if err := good.Validate(); err != nil {
		t.Fatalf("Validate(good) = %v", err)
	}
	for name, s := range map[string]SpokeSpec{
		"newer":        {V: SpokeVersion + 1, Roots: good.Roots},
		"no roots":     {V: 1},
		"escaping":     {V: 1, Roots: []SpokeRoot{{Name: "../x"}}},
		"twice":        {V: 1, Roots: []SpokeRoot{{Name: "a"}, {Name: "a"}}},
		"bad reserved": {V: 1, Roots: []SpokeRoot{{Name: "a", Reserved: []string{"/etc"}}}},
		"half owner":   {V: 1, Roots: good.Roots, UID: 1000, GID: -1},
		"bad rescan":   {V: 1, Roots: good.Roots, Rescan: "soon"},
	} {
		if err := s.Validate(); err == nil {
			t.Errorf("%s: Validate took it", name)
		}
	}
}

// TestTakeLeaseCountsAndExcludes: one spoke holds the lease at a time, and
// each one that takes it gets a higher epoch.
func TestTakeLeaseCountsAndExcludes(t *testing.T) {
	name := filepath.Join(t.TempDir(), LeaseFile)
	ctx := context.Background()
	first, e1, err := takeLease(ctx, name, "pod-a", time.Second)
	if err != nil || e1 != 1 {
		t.Fatalf("takeLease = %d, %v; want epoch 1", e1, err)
	}
	if _, _, err := takeLease(ctx, name, "pod-b", 300*time.Millisecond); err == nil || !strings.Contains(err.Error(), "another spoke holds") {
		t.Fatalf("a second spoke took a held lease: %v", err)
	}
	released := make(chan struct{})
	go func() {
		time.Sleep(200 * time.Millisecond)
		first.Close()
		close(released)
	}()
	second, e2, err := takeLease(ctx, name, "pod-b", 5*time.Second)
	if err != nil || e2 != 2 {
		t.Fatalf("takeLease after release = %d, %v; want epoch 2", e2, err)
	}
	<-released
	second.Close()
	data, _ := os.ReadFile(name)
	var rec leaseRecord
	if err := json.Unmarshal(data, &rec); err != nil || rec.Epoch != 2 || rec.Pod != "pod-b" {
		t.Fatalf("lease file = %s (%v)", data, err)
	}
}

// The spoke runs in a process of its own (it changes its working
// directory); spokeChildEnv makes the test binary that process.
const spokeChildEnv = "PODSHARE_TEST_SPOKE"

type spokeChild struct {
	Sync, Dir, Socket string
	Spec              SpokeSpec
}

func TestMain(m *testing.M) {
	if cfg := os.Getenv(spokeChildEnv); cfg != "" {
		os.Exit(runSpokeChild(cfg))
	}
	os.Exit(m.Run())
}

func runSpokeChild(cfg string) int {
	var c spokeChild
	if err := json.Unmarshal([]byte(cfg), &c); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	ln, err := net.Listen("unix", c.Socket)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	err = RunSpoke(context.Background(), SpokeOptions{
		Spec: c.Spec, Dir: c.Dir, Sync: c.Sync, Listener: ln,
		Ready:  func(lease uint64) { fmt.Printf("ready %d\n", lease) },
		Logger: slog.New(slog.NewTextHandler(os.Stderr, nil)),
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "spoke:", err)
		return 1
	}
	return 0
}

// TestRunSpokeServesTheHub runs a spoke over a replayed layout in its own
// process and syncs it with an in-process hub and host tree through its
// socket, as the agents relay fleetd's streams: both ways, a large file on a
// bulk connection. Closing the control connection ends the spoke.
func TestRunSpokeServesTheHub(t *testing.T) {
	base := t.TempDir()
	syncDir := filepath.Join(base, "sync")
	share := filepath.Join(syncDir, "share")
	if err := os.MkdirAll(share, 0o755); err != nil {
		t.Fatal(err)
	}
	layout := claudeLayout(-1, -1)
	if err := Apply(share, layout); err != nil {
		t.Fatal(err)
	}
	table := syncpolicy.Defaults()
	spec := SpokeSpec{V: SpokeVersion, Roots: LayoutRoots(layout.Entries, table.Root), UID: -1, GID: -1, Pod: "pod-uid", NoWatch: true, Rescan: "1h"}
	sock := filepath.Join(base, "spoke.sock")
	cfg, _ := json.Marshal(spokeChild{Sync: syncDir, Dir: share, Socket: sock, Spec: spec})
	cmd := exec.Command(os.Args[0], "-test.run=^$")
	cmd.Env = append(os.Environ(), spokeChildEnv+"="+string(cfg))
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	exited := make(chan error, 1)
	waited := make(chan struct{})
	go func() {
		exited <- cmd.Wait()
		close(waited)
	}()
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		<-waited
		if t.Failed() {
			t.Logf("spoke's stderr:\n%s", stderr.String())
		}
	})
	line, err := bufio.NewReader(out).ReadString('\n')
	if err != nil || line != "ready 1\n" {
		t.Fatalf("the spoke said %q (%v), want ready with lease 1\n%s", line, err, stderr.String())
	}

	// The daemon's side: its hub and the host's tree.
	host := filepath.Join(base, "host")
	var roots []sharesync.TreeRoot
	var names []string
	for _, r := range spec.Roots {
		d := filepath.Join(host, r.Name)
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
		roots = append(roots, sharesync.TreeRoot{Name: r.Name, Dir: d})
		names = append(names, r.Name)
	}
	hub, err := sharesync.OpenHub(filepath.Join(base, "hub"), table, names, sharesync.HubOptions{NormalDelay: time.Millisecond, BulkDelay: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	defer hub.Close()
	tree, err := sharesync.OpenTree(roots, sharesync.TreeOptions{StateDir: filepath.Join(base, "host-state"), Settle: -1})
	if err != nil {
		t.Fatal(err)
	}
	defer tree.Close()
	hostSide := sharesync.NewSide(sharesync.SideInfo{ID: "host", Location: "host"}, tree, hub, sharesync.SideOptions{NoWatch: true, Poll: time.Hour, Rescan: time.Hour})
	if err := hostSide.Attach(context.Background(), hub); err != nil {
		t.Fatal(err)
	}

	dial := func(tag podwire.Tag, header any) net.Conn {
		c, err := net.Dial("unix", sock)
		if err != nil {
			t.Fatalf("dial the spoke: %v", err)
		}
		if err := podwire.WritePreamble(c, tag, header); err != nil {
			t.Fatal(err)
		}
		return c
	}
	ctl := dial(podwire.TagSyncCtl, spec)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var lease atomic.Uint64
	served := make(chan error, 1)
	go func() {
		served <- sharesync.ServeRemote(ctx, ctl, hub, sharesync.SideInfo{ID: "k8s:c/share", Location: "c"}, sharesync.RemoteOptions{
			OpenBulk: func(context.Context) (net.Conn, error) { return dial(podwire.TagSyncBulk, struct{}{}), nil },
			Attached: func(l uint64) { lease.Store(l) },
		})
	}()

	big := strings.Repeat("x", 3*sharesync.InlineMax)
	writeFile(t, filepath.Join(host, ".claude", "settings.json"), `{"theme":"dark"}`)
	writeFile(t, filepath.Join(share, "roots/.mnt/opt/data/big.bin"), big)
	flush := func() {
		t.Helper()
		fctx, fcancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer fcancel()
		if err := hub.Flush(fctx); err != nil {
			t.Fatalf("Flush: %v\n%s", err, stderr.String())
		}
	}
	waitFor(t, func() bool { return lease.Load() == 1 })
	flush()
	if got := readFile(filepath.Join(share, "roots/.claude/settings.json")); got != `{"theme":"dark"}` {
		t.Errorf("the spoke's tree has %q, want the host's settings", got)
	}
	if got := readFile(filepath.Join(host, ".mnt/opt/data/big.bin")); got != big {
		t.Errorf("the host's copy of the pod's large file is %d bytes, want %d", len(got), len(big))
	}
	if entries, err := os.ReadDir(filepath.Join(share, StateDir)); err != nil || len(entries) == 0 {
		t.Errorf("the spoke keeps no state in %s (%v)", StateDir, err)
	}

	_ = ctl.Close()
	select {
	case err := <-exited:
		if err != nil {
			t.Fatalf("the spoke ended with %v once its control connection closed", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the spoke outlived its control connection")
	}
	select {
	case <-served:
	case <-time.After(10 * time.Second):
		t.Fatal("ServeRemote outlived the spoke's connection")
	}
}

func writeFile(t *testing.T, p, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func readFile(p string) string {
	data, err := os.ReadFile(p)
	if err != nil {
		return "<" + err.Error() + ">"
	}
	return string(data)
}

func waitFor(t *testing.T, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatal("timed out")
		}
		time.Sleep(10 * time.Millisecond)
	}
}
