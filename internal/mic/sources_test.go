package mic

import (
	"context"
	"errors"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/BenjaminBenetti/fleet-man/fleetgrpc"
)

// TestMain keeps the provider's device announcement out of every test that is
// not about it: with the real lister, each Run would shell out to whatever
// sound tools the machine running the tests has, on a goroutine of its own.
func TestMain(m *testing.M) {
	listDevices = func() ([]Device, error) { return nil, errors.New("no device listing in tests") }
	os.Exit(m.Run())
}

// stubDevices makes the provider announce what list returns.
func stubDevices(t *testing.T, list func() ([]Device, error)) {
	t.Helper()
	orig := listDevices
	listDevices = list
	t.Cleanup(func() { listDevices = orig })
}

func stubHostname(t *testing.T, name string, err error) {
	t.Helper()
	orig := hostname
	hostname = func() (string, error) { return name, err }
	t.Cleanup(func() { hostname = orig })
}

// The name is what the microphone selection is stored against, so it has to be
// the same every time this machine attaches — and safe to draw in a terminal.
func TestClientName(t *testing.T) {
	cases := []struct {
		name, env, host string
		hostErr         error
		want            string
	}{
		{name: "the short hostname", host: "desk.lan", want: "desk"},
		{name: "a bare hostname", host: "laptop", want: "laptop"},
		{name: "the override wins", env: "Ben's Desk", host: "desk.lan", want: "Ben's Desk"},
		{name: "a blank override does not", env: "   ", host: "desk", want: "desk"},
		{name: "no hostname at all", hostErr: errors.New("no hostname"), want: "unknown"},
		{name: "an unprintable hostname", host: "\x1b\x07", want: "unknown"},
		{name: "control characters are dropped", env: "desk\x1b[2J\n", want: "desk[2J"},
		{name: "a long name is cut", env: strings.Repeat("x", 3*MaxClientName), want: strings.Repeat("x", MaxClientName)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(EnvClient, tc.env)
			stubHostname(t, tc.host, tc.hostErr)
			if got := ClientName(); got != tc.want {
				t.Fatalf("ClientName() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestCleanText(t *testing.T) {
	cases := []struct {
		in    string
		limit int
		want  string
	}{
		{"plain", 16, "plain"},
		{"  padded \t and\nwrapped  ", 32, "padded and wrapped"},
		{"esc\x1b]0;title\x07ape", 32, "esc]0;titleape"},
		{"bidi‮override", 32, "bidioverride"},
		{"Großes Mikrofon", 32, "Großes Mikrofon"},
		{"abcdef", 3, "abc"},
		{"ab cd", 3, "ab"},   // no room for the space AND a rune after it
		{"ab cd", 4, "ab c"}, // exactly room for both
		{"\x00\x01", 8, ""},
	}
	for _, tc := range cases {
		if got := CleanText(tc.in, tc.limit); got != tc.want {
			t.Errorf("CleanText(%q, %d) = %q, want %q", tc.in, tc.limit, got, tc.want)
		}
	}
}

// An id is an identifier: it is dropped if it is not clean, never rewritten —
// a rewritten id would be a DIFFERENT device as far as the recorder is
// concerned. Labels are only for reading, and are scrubbed.
func TestCleanDevices(t *testing.T) {
	devices := []Device{
		{ID: "avfoundation:MacBook Pro Microphone", Label: "MacBook Pro Microphone"}, // spaces in an id are fine
		{ID: "pulse:evil\x1bid", Label: "dropped"},
		{ID: "", Label: "no id"},
		{ID: "pulse:" + strings.Repeat("x", MaxDeviceID), Label: "too long"},
		{ID: "pulse:usb", Label: "USB\x1b[31m Mic"},
		{ID: "pulse:usb", Label: "a duplicate id"},
		{ID: "pulse:nolabel", Label: "\x07"},
	}
	got := CleanDevices(devices)
	want := []Device{
		{ID: "avfoundation:MacBook Pro Microphone", Label: "MacBook Pro Microphone"},
		{ID: "pulse:usb", Label: "USB[31m Mic"},
		{ID: "pulse:nolabel", Label: "pulse:nolabel"},
	}
	if len(got) != len(want) {
		t.Fatalf("CleanDevices = %+v, want %+v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("device %d = %+v, want %+v", i, got[i], want[i])
		}
	}

	var many []Device
	for i := range 3 * MaxDevices {
		many = append(many, Device{ID: "pulse:" + strings.Repeat("d", i+1), Label: "d"})
	}
	if got := CleanDevices(many); len(got) != MaxDevices {
		t.Fatalf("%d devices survived, want the bound of %d", len(got), MaxDevices)
	}
}

// announcements records what a provider tells the fake daemon about itself.
type announcements struct {
	mu      sync.Mutex
	opens   []*fleetgrpc.MicOpen
	devices []*fleetgrpc.MicDeviceList
}

func (a *announcements) lists() []*fleetgrpc.MicDeviceList {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]*fleetgrpc.MicDeviceList(nil), a.devices...)
}

// announcingDaemon greets the provider idle, records its open header and every
// device listing, and sends a list_devices request each time ask is signalled.
func announcingDaemon(seen *announcements, ask <-chan struct{}) *fakeDaemon {
	return &fakeDaemon{handler: func(stream fleetgrpc.FleetService_MicServer) error {
		first, err := stream.Recv()
		if err != nil {
			return err
		}
		seen.mu.Lock()
		seen.opens = append(seen.opens, first.GetOpen())
		seen.mu.Unlock()
		_ = stream.Send(demandFrame(false))
		go func() {
			for {
				select {
				case <-ask:
					_ = stream.Send(&fleetgrpc.MicDown{Msg: &fleetgrpc.MicDown_ListDevices{ListDevices: &fleetgrpc.MicListDevices{}}})
				case <-stream.Context().Done():
					return
				}
			}
		}()
		for {
			up, err := stream.Recv()
			if err != nil {
				return err
			}
			if list := up.GetDevices(); list != nil {
				seen.mu.Lock()
				seen.devices = append(seen.devices, list)
				seen.mu.Unlock()
			}
		}
	}}
}

func runProvider(t *testing.T, daemon *fakeDaemon, report func(Status)) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		Run(ctx, dialFakeDaemon(t, daemon), "", report)
	}()
	// Joined before the seams this test installed are restored (cleanups run
	// last-registered first, and this one is registered after them).
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("Run did not return after cancel")
		}
	})
}

// What makes this machine's microphones selectable from another client: the
// provider says who it is, lists its devices once it is attached, and lists
// them again whenever the daemon asks.
func TestRunAnnouncesItsClientAndDevices(t *testing.T) {
	var recorder stubCapture
	recorder.install(t)
	t.Setenv(EnvClient, "laptop")

	var mu sync.Mutex
	listing := []Device{{ID: "pulse:builtin", Label: "Built-in Microphone"}}
	stubDevices(t, func() ([]Device, error) {
		mu.Lock()
		defer mu.Unlock()
		return listing, nil
	})

	var seen announcements
	ask := make(chan struct{})
	runProvider(t, announcingDaemon(&seen, ask), func(Status) {})

	waitFor(t, "the first device listing", func() bool { return len(seen.lists()) == 1 })
	seen.mu.Lock()
	open := seen.opens[0]
	seen.mu.Unlock()
	if open.GetClient() != "laptop" {
		t.Fatalf("open.client = %q, want this machine's name", open.GetClient())
	}
	if first := seen.lists()[0].GetDevices(); len(first) != 1 || first[0].GetId() != "pulse:builtin" || first[0].GetLabel() != "Built-in Microphone" {
		t.Fatalf("first listing = %v", first)
	}

	// A headset is plugged in; someone opens a selector.
	mu.Lock()
	listing = append(listing, Device{ID: "pulse:headset", Label: "Headset"})
	mu.Unlock()
	ask <- struct{}{}
	waitFor(t, "the refreshed listing", func() bool { return len(seen.lists()) == 2 })
	if second := seen.lists()[1].GetDevices(); len(second) != 2 || second[1].GetId() != "pulse:headset" {
		t.Fatalf("refreshed listing = %v", second)
	}
}

// A listing that FAILS says nothing about the hardware, so nothing is sent: the
// daemon keeps what it was told before. An empty listing, on the other hand, is
// an answer (a default-only recorder) and is sent.
func TestRunDoesNotAnnounceAFailedListing(t *testing.T) {
	var recorder stubCapture
	recorder.install(t)

	var mu sync.Mutex
	calls := 0
	fail := true
	stubDevices(t, func() ([]Device, error) {
		mu.Lock()
		defer mu.Unlock()
		calls++
		if fail {
			return nil, errors.New("pactl: timeout")
		}
		return nil, nil
	})
	listed := func() int { mu.Lock(); defer mu.Unlock(); return calls }

	var seen announcements
	ask := make(chan struct{})
	runProvider(t, announcingDaemon(&seen, ask), func(Status) {})

	waitFor(t, "the first listing attempt", func() bool { return listed() == 1 })
	mu.Lock()
	fail = false
	mu.Unlock()
	ask <- struct{}{}
	waitFor(t, "the empty listing", func() bool { return len(seen.lists()) == 1 })
	if got := seen.lists()[0].GetDevices(); len(got) != 0 {
		t.Fatalf("listing = %v, want empty", got)
	}
	if listed() != 2 {
		t.Fatalf("listed %d times, want 2 (the failed attempt sent nothing)", listed())
	}
}

// A slow enumeration (a wedged sound server) must not hold anything else up:
// the provider still attaches, answers demand and records while it is stuck.
func TestRunDoesNotWaitForTheDeviceListing(t *testing.T) {
	var recorder stubCapture
	recorder.install(t)
	stuck := make(chan struct{})
	stubDevices(t, func() ([]Device, error) {
		<-stuck
		return nil, errors.New("gave up")
	})
	// Freed only AFTER Run has been joined (cleanups run last-registered first):
	// Run must return on cancel without waiting for a lister that is stuck.
	t.Cleanup(func() { close(stuck) })

	var log statusLog
	runProvider(t, holdDemand(), log.report)
	waitFor(t, "the provider to go live with the listing still stuck", func() bool { return log.has(StateLive) })
}

// The daemon names the selected client with every demand. Demand reaching a
// provider that is NOT that client means the selected one is not attached and
// this machine is recording in its place — which the read-out must say, since
// it is not the microphone the user chose.
func TestRunReportsStandingInForTheSelectedClient(t *testing.T) {
	var recorder stubCapture
	recorder.install(t)
	t.Setenv(EnvClient, "laptop")

	selected := make(chan string)
	daemon := &fakeDaemon{handler: func(stream fleetgrpc.FleetService_MicServer) error {
		if _, err := stream.Recv(); err != nil {
			return err
		}
		for {
			select {
			case client := <-selected:
				frame := demandFrame(true, "alpha/i1")
				frame.GetDemand().Client = client
				_ = stream.Send(frame)
			case <-stream.Context().Done():
				return nil
			}
		}
	}}
	var log statusLog
	runProvider(t, daemon, log.report)
	last := func() Status {
		log.mu.Lock()
		defer log.mu.Unlock()
		if len(log.seen) == 0 {
			return Status{}
		}
		return log.seen[len(log.seen)-1]
	}

	selected <- "desk"
	waitFor(t, "live, standing in for desk", func() bool {
		status := last()
		return status.State == StateLive && status.StandInFor == "desk"
	})
	selected <- "laptop" // this machine is the selected one after all
	waitFor(t, "live as the selected client", func() bool {
		status := last()
		return status.State == StateLive && status.StandInFor == ""
	})
	selected <- "" // no selection: the most recent client records, nobody is stood in for
	waitFor(t, "live with no selection", func() bool {
		status := last()
		return status.State == StateLive && status.StandInFor == ""
	})
}
