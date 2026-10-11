//go:build linux

package podagent

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/BenjaminBenetti/fleet-man/internal/execas"
	"github.com/BenjaminBenetti/fleet-man/internal/podinit"
	"github.com/BenjaminBenetti/fleet-man/internal/podwire"
)

// fakeLayout records a cgroup layout whose user cgroup is a plain dir that
// reads as frozen as soon as it is asked (cgroup.events says so): what the
// agent's RPCs do with it, not what the kernel does.
func fakeLayout(t *testing.T, d testDirs) (string, string) {
	t.Helper()
	user := filepath.Join(t.TempDir(), "fleet-user")
	if err := os.MkdirAll(user, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, v := range map[string]string{"cgroup.freeze": "0", "cgroup.events": "populated 1\nfrozen 1\n"} {
		if err := os.WriteFile(filepath.Join(user, name), []byte(v), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	data, _ := json.Marshal(podinit.Layout{V: 1, Container: filepath.Dir(user), Agent: filepath.Join(filepath.Dir(user), "fleet-agent"), User: user})
	file := filepath.Join(d.run, "cgroup.json")
	if err := os.MkdirAll(d.run, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, data, 0o644); err != nil {
		t.Fatal(err)
	}
	return file, user
}

func freezeOptions(t *testing.T) (Options, string, string) {
	d := newTestDirs(t)
	o := d.options()
	layout, user := fakeLayout(t, d)
	o.LayoutFile, o.FreezeMarker, o.SyncPath = layout, filepath.Join(d.run, "freeze.json"), d.run
	return o, user, o.FreezeMarker
}

func TestFreezeAndThawOverTheSession(t *testing.T) {
	o, user, marker := freezeOptions(t)
	s := startAgent(t, o)
	ctx := context.Background()
	var fi podwire.FreezeInfo
	if err := s.c.Call(ctx, podwire.MethodFreeze, podwire.FreezeArgs{WatchdogMS: 20000}, &fi); err != nil {
		t.Fatal(err)
	}
	if d := fi.Deadline.Sub(fi.FrozenAt); d < 19*time.Second || d > 21*time.Second {
		t.Fatalf("the freeze's watchdog is %s away, want 20s", d)
	}
	if got, _ := os.ReadFile(filepath.Join(user, "cgroup.freeze")); string(got) != "1" {
		t.Fatalf("cgroup.freeze = %q", got)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("no record of the freeze for pod-init: %v", err)
	}
	var th podwire.ThawInfo
	if err := s.c.Call(ctx, podwire.MethodThaw, nil, &th); err != nil {
		t.Fatal(err)
	}
	if !th.WasFrozen || th.ByWatchdog {
		t.Fatalf("thaw %+v", th)
	}
	if got, _ := os.ReadFile(filepath.Join(user, "cgroup.freeze")); string(got) != "0" {
		t.Fatalf("cgroup.freeze after the thaw = %q", got)
	}
}

func TestTheAgentsWatchdogThawsAForgottenFreeze(t *testing.T) {
	o, user, _ := freezeOptions(t)
	s := startAgent(t, o)
	ctx := context.Background()
	if err := s.c.Call(ctx, podwire.MethodFreeze, podwire.FreezeArgs{WatchdogMS: 150}, nil); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		if got, _ := os.ReadFile(filepath.Join(user, "cgroup.freeze")); string(got) == "0" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the watchdog never thawed the freeze")
		}
		time.Sleep(20 * time.Millisecond)
	}
	var th podwire.ThawInfo
	if err := s.c.Call(ctx, podwire.MethodThaw, nil, &th); err != nil {
		t.Fatal(err)
	}
	if th.WasFrozen || !th.ByWatchdog {
		t.Fatalf("the thaw after the watchdog's: %+v", th)
	}
}

// A renewal extends the freeze in force (the timer moves with it) and never
// freezes anew: after the watchdog's thaw it answers thawed, the cgroup
// stays thawed, and the thaw after still reports the watchdog.
func TestARenewalAfterTheWatchdogsThawSaysSo(t *testing.T) {
	o, user, _ := freezeOptions(t)
	s := startAgent(t, o)
	ctx := context.Background()
	if err := s.c.Call(ctx, podwire.MethodFreeze, podwire.FreezeArgs{WatchdogMS: 300}, nil); err != nil {
		t.Fatal(err)
	}
	// Renewed in time: the first timer thaws nothing.
	time.Sleep(150 * time.Millisecond)
	var fi podwire.FreezeInfo
	if err := s.c.Call(ctx, podwire.MethodFreeze, podwire.FreezeArgs{WatchdogMS: 300, Renew: true}, &fi); err != nil {
		t.Fatalf("a renewal in time: %v", err)
	}
	time.Sleep(200 * time.Millisecond)
	if got, _ := os.ReadFile(filepath.Join(user, "cgroup.freeze")); string(got) != "1" {
		t.Fatalf("the renewed freeze was thawed at the old deadline: cgroup.freeze = %q", got)
	}
	// Then too late.
	deadline := time.Now().Add(5 * time.Second)
	for {
		if got, _ := os.ReadFile(filepath.Join(user, "cgroup.freeze")); string(got) == "0" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the watchdog never thawed the renewed freeze")
		}
		time.Sleep(20 * time.Millisecond)
	}
	err := s.c.Call(ctx, podwire.MethodFreeze, podwire.FreezeArgs{WatchdogMS: 300, Renew: true}, nil)
	if !podwire.IsCode(err, podwire.CodeThawed) {
		t.Fatalf("a renewal after the watchdog's thaw: %v", err)
	}
	if got, _ := os.ReadFile(filepath.Join(user, "cgroup.freeze")); string(got) != "0" {
		t.Fatalf("a late renewal froze again: cgroup.freeze = %q", got)
	}
	var th podwire.ThawInfo
	if err := s.c.Call(ctx, podwire.MethodThaw, nil, &th); err != nil {
		t.Fatal(err)
	}
	if !th.ByWatchdog {
		t.Fatalf("the thaw after a late renewal: %+v", th)
	}
}

func TestAFreezeWithoutALayoutIsUnsupported(t *testing.T) {
	d := newTestDirs(t)
	o := d.options()
	o.LayoutFile = filepath.Join(d.run, "none.json")
	s := startAgent(t, o)
	err := s.c.Call(context.Background(), podwire.MethodFreeze, nil, nil)
	if !podwire.IsCode(err, podwire.CodeUnsupported) {
		t.Fatalf("freeze of an unprivileged pod: %v", err)
	}
	if err := s.c.Call(context.Background(), podwire.MethodSync, nil, nil); err != nil {
		t.Fatalf("sync: %v", err)
	}
}

func TestCommandsStartInTheUserCgroup(t *testing.T) {
	d := newTestDirs(t)
	o := d.options()
	o.LayoutFile, _ = fakeLayout(t, d)
	got := userCgroup(o)
	if !strings.HasSuffix(got, "fleet-user") {
		t.Fatalf("user cgroup %q", got)
	}
	o.LayoutFile = ""
	if got := userCgroup(o); got != "" {
		t.Fatalf("no layout, yet cgroup %q", got)
	}
	// A plan the agent runs carries it (a plain dir is no cgroup, and the
	// command still runs).
	o.LayoutFile, _ = fakeLayout(t, d)
	s := startAgent(t, o)
	res, err := s.c.Run(context.Background(), podwire.ExecHeader{Request: execas.Request{Class: execas.ClassScript, Argv: []string{"echo", "ok"}}}, nil)
	if err != nil || strings.TrimSpace(string(res.Stdout)) != "ok" {
		t.Fatalf("run: %+v, %v", res, err)
	}
}

func TestRemoteErrorCodesReachTheCaller(t *testing.T) {
	d := newTestDirs(t)
	o := d.options()
	o.Methods = map[string]Method{"coded": func(context.Context, json.RawMessage) (any, error) {
		return nil, &podwire.RemoteError{Code: podwire.CodeDenied, Message: "no"}
	}, "plain": func(context.Context, json.RawMessage) (any, error) {
		return nil, errors.New("plain failure")
	}}
	s := startAgent(t, o)
	if err := s.c.Call(context.Background(), "coded", nil, nil); !podwire.IsCode(err, podwire.CodeDenied) {
		t.Fatalf("coded: %v", err)
	}
	if err := s.c.Call(context.Background(), "plain", nil, nil); !podwire.IsCode(err, podwire.CodeFailed) {
		t.Fatalf("plain: %v", err)
	}
}
