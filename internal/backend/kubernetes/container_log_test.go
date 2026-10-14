//go:build !nok8s

package kubernetes

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/BenjaminBenetti/fleet-man/internal/fleetpaths"
	"github.com/BenjaminBenetti/fleet-man/internal/podinit"
)

// capLog keeps the newest bytes under the cap, dropping whole leading lines.
func TestCapLog(t *testing.T) {
	if got := string(capLog([]byte("a\nbb\n"), 10)); got != "a\nbb\n" {
		t.Errorf("under the cap: %q", got)
	}
	if got := string(capLog([]byte("first\nsecond\nthird\n"), 12)); got != "third\n" {
		t.Errorf("over the cap: %q, want the whole last line", got)
	}
}

// AppendContainerLog keeps the file under ContainerLogMax, never cut
// mid-line, as large appends push it past.
func TestAppendContainerLogCaps(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Cleanup(func() { RemoveContainerLog("f", "i") })
	line := strings.Repeat("x", 1023) + "\n"
	chunk := []byte(strings.Repeat(line, 1024)) // 1 MiB of whole lines
	for i := 0; i < ContainerLogMax/len(chunk)+3; i++ {
		if err := AppendContainerLog("f", "i", chunk); err != nil {
			t.Fatal(err)
		}
	}
	data, err := os.ReadFile(fleetpaths.ContainerLogPath("f", "i"))
	if err != nil {
		t.Fatal(err)
	}
	if len(data) > ContainerLogMax || len(data) < ContainerLogMax-len(line) {
		t.Fatalf("container.log is %d bytes, want just under the %d cap", len(data), ContainerLogMax)
	}
	if !bytes.HasPrefix(data, []byte(line)) {
		t.Fatal("the front was cut mid-line")
	}
	if fi, err := os.Stat(fleetpaths.ContainerLogPath("f", "i")); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("container.log mode %v (%v), want 0600", fi.Mode().Perm(), err)
	}
	RemoveContainerLog("f", "i")
	if _, err := os.Stat(filepath.Dir(fleetpaths.ContainerLogPath("f", "i"))); !os.IsNotExist(err) {
		t.Fatalf("the instance's log directory is left behind: %v", err)
	}
}

// SavedRuns reads the runs container.log holds back from its headers.
func TestSavedRunsReadsTheHeaders(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Cleanup(func() { RemoveContainerLog("f", "i") })
	if got := SavedRuns("f", "i"); len(got) != 0 {
		t.Fatalf("no file: %v", got)
	}
	at := time.Unix(1700000000, 0)
	log := RunHeader("restarted", "uid-a", 0, at) + "out\n===== not a header\n" + RunHeader("recovered", "uid-b", 12, at) + "more\n"
	if err := AppendContainerLog("f", "i", []byte(log)); err != nil {
		t.Fatal(err)
	}
	got := SavedRuns("f", "i")
	if len(got) != 2 || !got[RunKey("uid-a", 0)] || !got[RunKey("uid-b", 12)] {
		t.Fatalf("SavedRuns = %v", got)
	}
}

// The ended runs of the rolling copy: all but the current (last) one, and
// only those naming their pod.
func TestEndedRollingRuns(t *testing.T) {
	runs := []podinit.RunOutput{
		{RunMark: podinit.RunMark{PodUID: "a"}, PodSeq: 0},
		{RunMark: podinit.RunMark{}, PodSeq: 0},
		{RunMark: podinit.RunMark{PodUID: "a"}, PodSeq: 1},
		{RunMark: podinit.RunMark{PodUID: "b"}, PodSeq: 0}, // current
	}
	got := EndedRollingRuns(runs)
	if len(got) != 2 || got[0].PodSeq != 0 || got[1].PodSeq != 1 || got[1].PodUID != "a" {
		t.Fatalf("EndedRollingRuns = %+v", got)
	}
	if EndedRollingRuns(nil) != nil || EndedRollingRuns(runs[3:]) != nil {
		t.Fatal("the current run alone is not an ended one")
	}
}
