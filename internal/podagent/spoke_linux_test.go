//go:build linux

package podagent

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/BenjaminBenetti/fleet-man/internal/mounts/syncpolicy"
	"github.com/BenjaminBenetti/fleet-man/internal/podshare"
	"github.com/BenjaminBenetti/fleet-man/internal/podwire"
	"github.com/BenjaminBenetti/fleet-man/internal/sharesync"
)

// spokeEnv is the environment of the spokes a test's agents start.
func spokeEnv(root string) []string {
	env := []string{helperEnv + "=spoke", "PODAGENT_TEST_ROOT=" + root}
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, helperEnv+"=") {
			env = append(env, kv)
		}
	}
	return env
}

// shareTree lays out a fleet share for a test pod: d's sync folder with the
// tree mounted (here: made) inside it.
func shareTree(t *testing.T, d testDirs) (syncDir, share string, layout podshare.Layout) {
	t.Helper()
	syncDir = filepath.Join(filepath.Dir(d.run), "s")
	share = filepath.Join(syncDir, "share")
	if err := os.MkdirAll(share, 0o755); err != nil {
		t.Fatal(err)
	}
	layout = podshare.Layout{V: podshare.LayoutVersion, UID: -1, GID: -1, Entries: []podshare.Entry{
		{Path: ".claude", Dir: true, Mode: 0o777},
		{Path: "files", Dir: true, Mode: 0o777},
		{Path: "files/.claude.json", Mode: 0o666, Seed: "{}"},
	}}
	if err := podshare.Apply(share, layout); err != nil {
		t.Fatal(err)
	}
	return syncDir, share, layout
}

// TestSyncStreamsReachTheSpoke: fleetd's TagSyncCtl stream on the agent
// session starts the pod's spoke and becomes its control connection; its
// TagSyncBulk streams on the bulk session reach the same spoke; closing the
// control stream ends the spoke. A host file lands in the pod's share and a
// large pod file on the host.
func TestSyncStreamsReachTheSpoke(t *testing.T) {
	d := newTestDirs(t)
	syncDir, share, layout := shareTree(t, d)
	o := d.options()
	o.SpokeArgs, o.SpokeEnv = []string{"pod-agent", "spoke"}, spokeEnv(filepath.Dir(d.run))
	o.ShareDir, o.ShareSync = share, syncDir
	main := startAgent(t, o)
	bo := o
	bo.Role = podwire.RoleBulk
	bulk := startAgent(t, bo)

	table := syncpolicy.Defaults()
	spec := podshare.SpokeSpec{V: podshare.SpokeVersion, Roots: podshare.LayoutRoots(layout.Entries, table.Root), UID: -1, GID: -1, Pod: "pod-uid-1", NoWatch: true, Rescan: "1h"}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	ctl, err := main.c.OpenRelay(ctx, podwire.TagSyncCtl, spec)
	if err != nil {
		t.Fatalf("OpenRelay(sync-ctl): %v", err)
	}

	base := t.TempDir()
	host := filepath.Join(base, "host")
	var roots []sharesync.TreeRoot
	var names []string
	for _, r := range spec.Roots {
		dir := filepath.Join(host, r.Name)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		roots = append(roots, sharesync.TreeRoot{Name: r.Name, Dir: dir})
		names = append(names, r.Name)
	}
	hub, err := sharesync.OpenHub(filepath.Join(base, "hub"), table, names, sharesync.HubOptions{NormalDelay: time.Millisecond, BulkDelay: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	defer hub.Close()
	tree, err := sharesync.OpenTree(roots, sharesync.TreeOptions{StateDir: filepath.Join(base, "state"), Settle: -1})
	if err != nil {
		t.Fatal(err)
	}
	defer tree.Close()
	side := sharesync.NewSide(sharesync.SideInfo{ID: "host", Location: "host"}, tree, hub, sharesync.SideOptions{NoWatch: true, Poll: time.Hour, Rescan: time.Hour})
	if err := side.Attach(ctx, hub); err != nil {
		t.Fatal(err)
	}
	served := make(chan error, 1)
	go func() {
		served <- sharesync.ServeRemote(ctx, ctl, hub, sharesync.SideInfo{ID: "k8s:c/share", Location: "c"}, sharesync.RemoteOptions{
			OpenBulk: func(ctx context.Context) (net.Conn, error) {
				return bulk.c.OpenRelay(ctx, podwire.TagSyncBulk, struct{}{})
			},
		})
	}()

	if err := os.WriteFile(filepath.Join(host, ".claude", "CLAUDE.md"), []byte("from the host"), 0o644); err != nil {
		t.Fatal(err)
	}
	big := strings.Repeat("y", 2*sharesync.InlineMax)
	if err := os.WriteFile(filepath.Join(share, "roots/.claude/big.bin"), []byte(big), 0o644); err != nil {
		t.Fatal(err)
	}
	// A Flush before ServeRemote attached the pod's side has only the host
	// side to wait for, so it can succeed with nothing crossed yet: flush
	// until both files are across.
	crossed := func() (string, []byte) {
		toPod, _ := os.ReadFile(filepath.Join(share, "roots/.claude/CLAUDE.md"))
		toHost, _ := os.ReadFile(filepath.Join(host, ".claude/big.bin"))
		return string(toPod), toHost
	}
	deadline := time.Now().Add(20 * time.Second)
	for {
		fctx, fcancel := context.WithTimeout(ctx, 5*time.Second)
		err := hub.Flush(fctx)
		fcancel()
		if toPod, toHost := crossed(); err == nil && toPod == "from the host" && string(toHost) == big {
			break
		}
		if time.Now().After(deadline) {
			toPod, toHost := crossed()
			t.Fatalf("after 20s (last Flush: %v): the pod's share has %q, want the host's file; the host's copy of the pod's large file is %d bytes, want %d", err, toPod, len(toHost), len(big))
		}
		time.Sleep(50 * time.Millisecond)
	}

	_ = ctl.Close()
	select {
	case <-served:
	case <-time.After(10 * time.Second):
		t.Fatal("ServeRemote outlived its control stream")
	}
	// The spoke is gone with its control connection: a bulk stream finds
	// nothing to reach.
	waitUntil(t, 10*time.Second, func() bool {
		c, err := bulk.c.OpenRelay(ctx, podwire.TagSyncBulk, struct{}{})
		if err == nil {
			_ = c.Close()
			return false
		}
		return podwire.IsSockRefused(err, podwire.RefusedUnreachable)
	})
}

// TestSyncCtlRefusesABadSpec: a spec the agent cannot run is refused with
// its reason, starting nothing; the bulk session has no control streams.
func TestSyncCtlRefusesABadSpec(t *testing.T) {
	d := newTestDirs(t)
	main := startAgent(t, d.options())
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, err := main.c.OpenRelay(ctx, podwire.TagSyncCtl, podshare.SpokeSpec{V: 1, Roots: []podshare.SpokeRoot{{Name: "../etc"}}})
	var re *podwire.SockRefusedError
	if !errors.As(err, &re) || !re.Agent || !strings.Contains(re.Msg, "not a clean path") {
		t.Fatalf("OpenRelay = %v, want the agent's refusal naming the folder", err)
	}
	if _, err := os.Stat(filepath.Join(d.run, podshare.SpokeDirName, podshare.SpokeSocketName)); err == nil {
		t.Fatal("a spoke was started for a refused spec")
	}
	bo := d.options()
	bo.Role = podwire.RoleBulk
	bulk := startAgent(t, bo)
	if _, err := bulk.c.OpenRelay(ctx, podwire.TagSyncCtl, podshare.SpokeSpec{V: 1, Roots: []podshare.SpokeRoot{{Name: ".claude"}}}); !podwire.IsSockRefused(err, podwire.RefusedUnsupported) {
		t.Fatalf("the bulk agent took a control stream: %v", err)
	}
}

func waitUntil(t *testing.T, d time.Duration, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatal("timed out")
		}
		time.Sleep(50 * time.Millisecond)
	}
}
