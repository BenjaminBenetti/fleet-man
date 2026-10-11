//go:build !nok8s

package kubernetes

import (
	"bytes"
	"testing"

	"github.com/BenjaminBenetti/fleet-man/internal/k8s/topology"
	"github.com/BenjaminBenetti/fleet-man/internal/podshare"
	"github.com/BenjaminBenetti/fleet-man/internal/podwire"
	"github.com/BenjaminBenetti/fleet-man/internal/sharesync"
)

func multiNodeShare() *ShareRecord {
	return &ShareRecord{Mode: string(topology.MultiNode), Claim: "fs-web-abcd2345-share", Roots: []podshare.SpokeRoot{{Name: ".claude"}, {Name: ".mnt", Skip: []string{"opt/data"}}, {Name: ".mnt/opt/data"}}, UID: 1000, GID: 1000}
}

// TestShareHintOf: only a pod of a share mounted from several nodes is asked
// for hints, of its share's folders.
func TestShareHintOf(t *testing.T) {
	h := shareHintOf(multiNodeShare())
	if h == nil || len(h.Roots) != 3 || h.Roots[1].Name != ".mnt" || len(h.Roots[1].Skip) != 1 {
		t.Fatalf("a multi-node share's hint = %+v", h)
	}
	single := multiNodeShare()
	single.Mode = string(topology.SingleNode)
	copyTier := &ShareRecord{Mode: string(topology.Copy), Roots: single.Roots}
	for name, s := range map[string]*ShareRecord{"single-node": single, "copy": copyTier, "none": nil} {
		if got := shareHintOf(s); got != nil {
			t.Errorf("%s: asked for hints %+v", name, got)
		}
	}
}

// TestRelayShareHints: a pod's hints reach the spoke being served for its
// share (the one fleetd's record of the pod names, never what the pod says),
// only for the share's folders, and are dropped while no spoke is served.
func TestRelayShareHints(t *testing.T) {
	p := NewPool(PoolOptions{})
	defer p.Close()
	tgt := PodTarget{Cluster: "c-abc234", Fleet: "web", Instance: "a", StatefulSet: "web-a", Share: multiNodeShare()}
	roots := map[string]bool{}
	for _, r := range shareHintOf(tgt.Share).Roots {
		roots[r.Name] = true
	}
	var in bytes.Buffer
	for _, m := range []podwire.ShareHintMsg{
		{Root: ".claude", Paths: []string{"projects/a/s.jsonl"}},
		{Root: ".codex", Paths: []string{"auth.json"}}, // not one of its share's
		{Root: ".mnt/opt/data", Paths: []string{"."}},
	} {
		if err := podwire.WriteShareHint(&in, m); err != nil {
			t.Fatal(err)
		}
	}
	// No spoke served: the hints go nowhere, and nothing blocks.
	p.relayShareHints(tgt, roots, bytes.NewReader(in.Bytes()))

	hints, release := takeHints(ShareTree(tgt.Cluster, "", *tgt.Share))
	defer release()
	p.relayShareHints(tgt, roots, bytes.NewReader(in.Bytes()))
	var got []sharesync.Hint
	for len(hints) > 0 {
		got = append(got, <-hints)
	}
	if len(got) != 2 || got[0].Root != ".claude" || got[0].Paths[0] != "projects/a/s.jsonl" || got[1].Root != ".mnt/opt/data" {
		t.Fatalf("the spoke got %+v", got)
	}

	// Another share's spoke hears nothing of this pod's.
	other, release2 := takeHints(ShareTree(tgt.Cluster, "", ShareRecord{Mode: string(topology.MultiNode), Claim: "fs-other-share"}))
	defer release2()
	p.relayShareHints(tgt, roots, bytes.NewReader(in.Bytes()))
	if len(other) != 0 {
		t.Fatalf("another share's spoke got %d hints", len(other))
	}
}

// TestRelayShareHintsIsPaced: a pod sending a flood of hints gets at most
// shareHintRate a second through.
func TestRelayShareHintsIsPaced(t *testing.T) {
	p := NewPool(PoolOptions{})
	defer p.Close()
	tgt := PodTarget{Cluster: "c-abc234", Share: multiNodeShare()}
	var in bytes.Buffer
	for i := 0; i < 5*shareHintRate; i++ {
		_ = podwire.WriteShareHint(&in, podwire.ShareHintMsg{Root: ".claude", Paths: []string{"x"}})
	}
	hints, release := takeHints(ShareTree(tgt.Cluster, "", *tgt.Share))
	defer release()
	p.relayShareHints(tgt, map[string]bool{".claude": true}, bytes.NewReader(in.Bytes()))
	if n := len(hints); n > shareHintRate*2 || n == 0 {
		t.Fatalf("%d hints passed of %d sent at once", n, 5*shareHintRate)
	}
}

// TestDesiredStateAsksForShareHints: a pod of a multi-node share is asked to
// hint its share's folders; others are not.
func TestDesiredStateAsksForShareHints(t *testing.T) {
	p := NewPool(PoolOptions{})
	defer p.Close()
	ds, _ := p.desiredState(PodTarget{Cluster: "c-abc234", StatefulSet: "web-a", Share: multiNodeShare()})
	if ds.ShareHint == nil || len(ds.ShareHint.Roots) != 3 || ds.Validate() != nil {
		t.Fatalf("a multi-node share's pod: %+v", ds.ShareHint)
	}
	if ds, _ := p.desiredState(PodTarget{Cluster: "c-abc234", StatefulSet: "web-b"}); ds.ShareHint != nil {
		t.Fatalf("a pod without a share: %+v", ds.ShareHint)
	}
}

// TestRecordTargetCarriesTheShare: the pod an instance record names carries
// its share (a copy), so its sessions know to ask for hints.
func TestRecordTargetCarriesTheShare(t *testing.T) {
	rec := InstanceRecord{Cluster: "c-abc234", StatefulSet: "web-a", Share: multiNodeShare()}
	tgt := rec.target()
	if tgt.Share == nil || tgt.Share.Claim != rec.Share.Claim {
		t.Fatalf("target share %+v", tgt.Share)
	}
	tgt.Share.Roots[0].Name = "changed"
	if rec.Share.Roots[0].Name == "changed" {
		t.Fatal("the target shares the record's share")
	}
}
