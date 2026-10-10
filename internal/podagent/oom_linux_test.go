//go:build linux

package podagent

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestAdoptOOMScore(t *testing.T) {
	dir := t.TempDir()
	write := func(name, v string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(v), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	cases := []struct {
		from, to, want string
		fail           bool
	}{
		{"999\n", "-999\n", "999", false},
		{"-500\n", "0\n", "-500", false},
		{"12\n", "12\n", "12\n", false},
		{"lots\n", "0\n", "0\n", true},
		{"1001\n", "0\n", "0\n", true},
	}
	for _, tc := range cases {
		from, to := write("from", tc.from), write("to", tc.to)
		err := adoptOOMScore(from, to)
		got, _ := os.ReadFile(to)
		if (err != nil) != tc.fail || string(got) != tc.want {
			t.Errorf("%q over %q: %q, %v", tc.from, tc.to, got, err)
		}
	}
	if err := adoptOOMScore(filepath.Join(dir, "missing"), write("to", "0")); err == nil {
		t.Error("a missing source is an error")
	}
}

// TestAgentTakesTheContainersOOMScore: an agent (and so every command it
// starts, held ones too) runs at the container's OOM score, not the one the
// runtime gave its exec.
func TestAgentTakesTheContainersOOMScore(t *testing.T) {
	own, err := readOOMScore("/proc/self/oom_score_adj")
	if err != nil {
		t.Skip(err)
	}
	if own >= 1000 {
		t.Skip("oom_score_adj is already at its highest")
	}
	want := min(own+7, 1000) // raising needs no privilege
	d := newTestDirs(t)
	from := filepath.Join(filepath.Dir(d.run), "init_oom_score_adj")
	if err := os.WriteFile(from, []byte(strconv.Itoa(want)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	h := startHelperAgent(t, d, "PODAGENT_TEST_NO_TAKEOVER=1", "PODAGENT_TEST_OOM_FROM="+from)
	for _, hdr := range []bool{false, true} {
		probe := sh(`cat /proc/self/oom_score_adj`)
		probe.Resumable = hdr
		res, err := h.c.Run(context.Background(), probe, nil)
		if err != nil {
			t.Fatal(err)
		}
		if got := strings.TrimSpace(string(res.Stdout)); got != strconv.Itoa(want) {
			t.Errorf("resumable %v: oom_score_adj %s, want %d", hdr, got, want)
		}
	}
}
