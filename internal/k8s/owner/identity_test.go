package owner

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"

	"github.com/BenjaminBenetti/fleet-man/internal/state"
)

// withMachine pins the machine fingerprint inputs for one test.
func withMachine(t *testing.T, machine, host string) {
	t.Helper()
	prevID, prevHost := machineIDFunc, hostnameFunc
	machineIDFunc = func() string { return machine }
	hostnameFunc = func() (string, error) { return host, nil }
	t.Cleanup(func() { machineIDFunc, hostnameFunc = prevID, prevHost })
}

func TestLoadIdentityCreatesAndKeeps(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	withMachine(t, "machine-a", "laptop")

	first, err := LoadIdentity()
	if err != nil {
		t.Fatalf("LoadIdentity: %v", err)
	}
	if !daemonIDPattern.MatchString(first.DaemonID) || !noncePattern.MatchString(first.Nonce) || first.Host != "laptop" {
		t.Fatalf("identity = %+v", first)
	}
	if got := first.Owner(); got != first.DaemonID[:12] {
		t.Errorf("Owner = %q", got)
	}
	if got := first.Incarnation(); got != first.Owner()+"-"+first.Nonce {
		t.Errorf("Incarnation = %q", got)
	}
	for path, mode := range map[string]os.FileMode{state.DaemonIDPath(): 0600, state.KubeIncarnationPath(): 0600, state.KubeDir(): 0700} {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatalf("stat %s: %v", path, err)
		}
		if got := info.Mode().Perm(); got != mode {
			t.Errorf("%s mode = %o, want %o", path, got, mode)
		}
	}
	data, _ := os.ReadFile(state.KubeIncarnationPath())
	if strings.Contains(string(data), "machine-a") {
		t.Errorf("the raw machine id is stored: %s", data)
	}

	again, err := LoadIdentity()
	if err != nil {
		t.Fatalf("LoadIdentity again: %v", err)
	}
	if again != first {
		t.Errorf("a second load changed the identity: %+v vs %+v", again, first)
	}
}

// A copy of ~/.fleet on another machine (or a renamed host) keeps the daemon
// id but gets a new incarnation; back on the first machine the fingerprint
// matches what is stored again only if the file came along unchanged.
func TestLoadIdentityNewIncarnationOnAnotherMachine(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	withMachine(t, "machine-a", "laptop")
	a, err := LoadIdentity()
	if err != nil {
		t.Fatal(err)
	}

	withMachine(t, "machine-b", "laptop")
	b, err := LoadIdentity()
	if err != nil {
		t.Fatal(err)
	}
	if b.DaemonID != a.DaemonID || b.Nonce == a.Nonce {
		t.Errorf("another machine: %+v from %+v, want the same daemon id and a new nonce", b, a)
	}

	withMachine(t, "machine-b", "renamed")
	c, err := LoadIdentity()
	if err != nil {
		t.Fatal(err)
	}
	if c.Nonce == b.Nonce {
		t.Error("a hostname change kept the nonce")
	}
}

// A damaged daemon_id is refused, never replaced.
func TestLoadIdentityRefusesMalformedDaemonID(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	withMachine(t, "m", "h")
	if err := os.MkdirAll(state.FleetDir(), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(state.DaemonIDPath(), []byte("not-an-id\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadIdentity(); err == nil || !strings.Contains(err.Error(), "restore it from a backup") {
		t.Fatalf("LoadIdentity = %v, want a refusal", err)
	}
	if data, _ := os.ReadFile(state.DaemonIDPath()); string(data) != "not-an-id\n" {
		t.Errorf("the malformed id was replaced: %q", data)
	}
}

// A damaged incarnation file just means a new incarnation.
func TestLoadIdentityReplacesBadIncarnation(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	withMachine(t, "m", "h")
	if err := os.MkdirAll(state.KubeDir(), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(state.KubeIncarnationPath(), []byte("{garbage"), 0600); err != nil {
		t.Fatal(err)
	}
	id, err := LoadIdentity()
	if err != nil || !noncePattern.MatchString(id.Nonce) {
		t.Fatalf("LoadIdentity = %+v, %v", id, err)
	}
	var f incarnationFile
	data, _ := os.ReadFile(state.KubeIncarnationPath())
	if err := json.Unmarshal(data, &f); err != nil || f.Nonce != id.Nonce || f.V != 1 {
		t.Errorf("incarnation file = %s (%v)", data, err)
	}
}

// Creating the daemon id never overwrites an existing one, even in a race.
func TestCreateExclusive(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "daemon_id")
	var wg sync.WaitGroup
	wins := make(chan string, 8)
	for i := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			content := strings.Repeat(string(rune('a'+i)), 4)
			if err := createExclusive(path, []byte(content)); err == nil {
				wins <- content
			} else if !errors.Is(err, os.ErrExist) {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	close(wins)
	var winners []string
	for w := range wins {
		winners = append(winners, w)
	}
	data, _ := os.ReadFile(path)
	if len(winners) != 1 || string(data) != winners[0] {
		t.Fatalf("winners %v, file %q: want exactly one creator and its content", winners, data)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 1 {
		t.Errorf("temp files left behind: %v", entries)
	}
}

// On a filesystem without hard links the daemon id is still created, and an
// existing one is still never overwritten.
func TestCreateExclusiveWithoutHardLinks(t *testing.T) {
	prev := linkFunc
	linkFunc = func(_, _ string) error { return &os.LinkError{Op: "link", Err: syscall.EPERM} }
	t.Cleanup(func() { linkFunc = prev })

	dir := t.TempDir()
	path := filepath.Join(dir, "daemon_id")
	if err := createExclusive(path, []byte("first")); err != nil {
		t.Fatalf("createExclusive without hard links: %v", err)
	}
	if err := createExclusive(path, []byte("second")); !errors.Is(err, os.ErrExist) {
		t.Errorf("second create = %v, want ErrExist", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if data, _ := os.ReadFile(path); string(data) != "first" || info.Mode().Perm() != 0600 {
		t.Errorf("file = %q mode %o, want the first content at 0600", data, info.Mode().Perm())
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 1 {
		t.Errorf("temp files left behind: %v", entries)
	}

	t.Setenv("HOME", t.TempDir())
	withMachine(t, "m", "h")
	if id, err := LoadIdentity(); err != nil || !daemonIDPattern.MatchString(id.DaemonID) {
		t.Fatalf("LoadIdentity without hard links = %+v, %v", id, err)
	}
}

func TestParseIOPlatformUUID(t *testing.T) {
	out := `+-o J314sAP  <class IOPlatformExpertDevice, id 0x100000213, registered, matched, active, busy 0 (13 ms), retain 39>
    {
      "IOPlatformSerialNumber" = "C02XXXXXXX"
      "IOPlatformUUID" = "564D6E1A-1C2B-4E3F-9A8B-7C6D5E4F3A2B"
    }`
	if got := parseIOPlatformUUID(out); got != "564D6E1A-1C2B-4E3F-9A8B-7C6D5E4F3A2B" {
		t.Errorf("parseIOPlatformUUID = %q", got)
	}
	if got := parseIOPlatformUUID("nothing"); got != "" {
		t.Errorf("parseIOPlatformUUID(nothing) = %q", got)
	}
}

// The real machine id reader works on this OS (it may legitimately be empty
// in a minimal container).
func TestMachineIDDoesNotFail(t *testing.T) {
	_ = machineID()
}
