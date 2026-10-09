package server

import (
	"bytes"
	"context"
	"io"
	"testing"
	"time"

	"github.com/BenjaminBenetti/fleet-man/fleetgrpc"
	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	"github.com/BenjaminBenetti/fleet-man/internal/output"
	"github.com/BenjaminBenetti/fleet-man/internal/state"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func takeOutput(t *testing.T, c *outputClient) *fleetgrpc.OutputDown {
	t.Helper()
	select {
	case msg := <-c.frames:
		return msg
	case <-time.After(time.Second):
		t.Fatal("no output frame")
		return nil
	}
}
func TestOutputRoutesOnlyToSelectedClient(t *testing.T) {
	h := newOutputHub()
	h.configure(state.OutputSettings{Enabled: true})
	laptop := h.add("laptop")
	if !takeOutput(t, laptop).GetSelection().GetActive() {
		t.Fatal("Auto did not select first client")
	}
	desk := h.add("desk")
	if takeOutput(t, laptop).GetSelection().GetActive() || !takeOutput(t, desk).GetSelection().GetActive() {
		t.Fatal("Auto did not select newest client")
	}
	source := &outputSource{}
	h.sources["f/i"] = source
	h.route("f/i", source, []byte{1, 2, 3, 4}, false)
	if !bytes.Equal(takeOutput(t, desk).GetAudio().GetPcm(), []byte{1, 2, 3, 4}) {
		t.Fatal("PCM lost")
	}
	if len(laptop.frames) != 0 {
		t.Fatal("audio leaked to standby client")
	}
	h.configure(state.OutputSettings{Enabled: true, Client: "laptop", Device: "pulse:headset"})
	if got := takeOutput(t, laptop).GetSelection(); !got.Active || got.Device != "pulse:headset" {
		t.Fatalf("selection: %v", got)
	}
	if takeOutput(t, desk).GetSelection().GetActive() {
		t.Fatal("old output remained active")
	}
	h.remove(laptop)
	if got := takeOutput(t, desk).GetSelection(); !got.Active || got.Device != "" {
		t.Fatalf("stand-in must use its own default: %v", got)
	}
	laptop = h.add("laptop")
	if takeOutput(t, desk).GetSelection().GetActive() || takeOutput(t, laptop).GetSelection().GetDevice() != "pulse:headset" {
		t.Fatal("pin not restored on reconnect")
	}
}
func TestOutputControlFlushesCongestedAudio(t *testing.T) {
	h := newOutputHub()
	h.configure(state.OutputSettings{Enabled: true})
	c := h.add("desk")
	takeOutput(t, c)
	source := &outputSource{}
	h.sources["f/i"] = source
	for i := 0; i < 1000; i++ {
		h.route("f/i", source, []byte{1, 2, 3, 4}, false)
	}
	if len(c.frames) != cap(c.frames) {
		t.Fatal("queue did not saturate")
	}
	h.configure(state.OutputSettings{Enabled: true, Client: "desk", Device: "pulse:new"})
	if got := takeOutput(t, c); got.GetSelection().GetDevice() != "pulse:new" {
		t.Fatalf("old audio survived switch: %v", got)
	}
	if len(c.frames) != 0 {
		t.Fatal("stale backlog survived switch")
	}
}

func TestOutputStandbyConnectionsDoNotInterruptPinnedPlayback(t *testing.T) {
	h := newOutputHub()
	h.configure(state.OutputSettings{Enabled: true, Client: "desk", Device: "pulse:usb"})
	desk := h.add("desk")
	takeOutput(t, desk)
	source := &outputSource{}
	h.sources["f/i"] = source
	h.route("f/i", source, []byte{1, 2, 3, 4}, false)
	standby := h.add("laptop")
	if takeOutput(t, standby).GetSelection().GetActive() {
		t.Fatal("standby took the pinned selection")
	}
	h.remove(standby)
	if got := takeOutput(t, desk); got.GetAudio() == nil {
		t.Fatalf("standby connection interrupted playback: %v", got)
	}
	if len(desk.frames) != 0 {
		t.Fatal("standby disconnect reset the player")
	}
}
func TestOutputConfigPreservedForOldClients(t *testing.T) {
	isolateFleetDir(t)
	svc := newService()
	ctx := context.Background()
	want := &fleetgrpc.OutputSettings{Enabled: true, Client: "desk", Device: "pulse:usb"}
	first, err := svc.SetConfig(ctx, &fleetgrpc.SetConfigRequest{Config: &fleetgrpc.Config{Output: want}})
	if err != nil {
		t.Fatal(err)
	}
	if first.OutputRevision == 0 || first.OutputRevision != svc.output.list(false).Revision {
		t.Fatal("save acknowledgement and Watch revision disagree")
	}
	if _, err := svc.SetConfig(ctx, &fleetgrpc.SetConfigRequest{Config: &fleetgrpc.Config{}}); err != nil {
		t.Fatal(err)
	}
	got, err := svc.GetConfig(ctx, &fleetgrpc.GetConfigRequest{})
	if err != nil {
		t.Fatal(err)
	}
	o := got.Config.GetOutput()
	if !o.Enabled || o.Client != want.Client || o.Device != want.Device {
		t.Fatalf("older client cleared output: %v", o)
	}
	last, err := svc.SetConfig(ctx, &fleetgrpc.SetConfigRequest{Config: &fleetgrpc.Config{Output: &fleetgrpc.OutputSettings{}}})
	if err != nil {
		t.Fatal(err)
	}
	if last.OutputRevision <= first.OutputRevision || last.OutputRevision != svc.output.list(false).Revision {
		t.Fatal("output revision did not advance with the new selection")
	}
	got, _ = svc.GetConfig(ctx, &fleetgrpc.GetConfigRequest{})
	if got.Config.Output.Enabled {
		t.Fatal("explicit off ignored")
	}
}

func TestOutputDefaultOnAndSavedOffSurviveOlderClients(t *testing.T) {
	isolateFleetDir(t)
	svc := newService()
	ctx := context.Background()
	initial, err := svc.GetConfig(ctx, &fleetgrpc.GetConfigRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if out := initial.Config.GetOutput(); !out.GetEnabled() || out.GetClient() != "" || out.GetDevice() != "" {
		t.Fatalf("fresh daemon must offer output on Auto: %v", out)
	}
	for _, enabled := range []bool{true, false} {
		if !enabled {
			if _, err := svc.SetConfig(ctx, &fleetgrpc.SetConfigRequest{Config: &fleetgrpc.Config{Output: &fleetgrpc.OutputSettings{Enabled: false}}}); err != nil {
				t.Fatal(err)
			}
		}
		// An older client saves an unrelated setting without an output group.
		if _, err := svc.SetConfig(ctx, &fleetgrpc.SetConfigRequest{Config: &fleetgrpc.Config{Agent: &fleetgrpc.AgentSettings{ToolSelection: "codex"}}}); err != nil {
			t.Fatal(err)
		}
		got, err := svc.GetConfig(ctx, &fleetgrpc.GetConfigRequest{})
		if err != nil {
			t.Fatal(err)
		}
		if got.Config.GetOutput().GetEnabled() != enabled {
			t.Fatalf("older client changed output, want enabled=%v", enabled)
		}
	}
}

func TestOutputRPCSelectionDevicesAndDisable(t *testing.T) {
	isolateFleetDir(t)
	if err := state.SaveConfig(&state.Config{OutputSettings: state.OutputSettings{Enabled: true}}); err != nil {
		t.Fatal(err)
	}
	svc, client, cleanup := newTestServer(t)
	defer cleanup()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	stream, err := client.Output(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := stream.Send(&fleetgrpc.OutputUp{Msg: &fleetgrpc.OutputUp_Open{Open: &fleetgrpc.OutputOpen{Client: "desk"}}}); err != nil {
		t.Fatal(err)
	}
	msg, err := stream.Recv()
	if err != nil || !msg.GetSelection().GetActive() {
		t.Fatalf("greeting %v %v", msg, err)
	}
	stream.Send(&fleetgrpc.OutputUp{Msg: &fleetgrpc.OutputUp_Devices{Devices: &fleetgrpc.OutputDeviceList{Devices: []*fleetgrpc.OutputDevice{{Id: "pulse:usb", Label: "USB\x1b[31m"}}}}})
	eventually(t, "remote output devices", func() bool {
		r, _ := client.ListOutputTargets(ctx, &fleetgrpc.ListOutputTargetsRequest{})
		return len(r.GetTargets().GetTargets()) == 1 && len(r.GetTargets().GetTargets()[0].GetDevices()) == 1
	})
	if _, err := client.SetConfig(ctx, &fleetgrpc.SetConfigRequest{Config: &fleetgrpc.Config{Output: &fleetgrpc.OutputSettings{}}}); err != nil {
		t.Fatal(err)
	}
	for {
		_, err = stream.Recv()
		if err != nil {
			break
		}
	}
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("disable: %v", err)
	}
	if len(svc.output.list(false).Targets) != 0 {
		t.Fatal("disabled output retained clients")
	}
}
func TestOutputSourceStreamsPCMAndClosesOnCancel(t *testing.T) {
	original := openOutputSource
	defer func() { openOutputSource = original }()
	fake := newFakeMicSink()
	openOutputSource = func(*fleet.Instance) (io.ReadWriteCloser, bool, error) { return fake, true, nil }
	h := newOutputHub()
	h.configure(state.OutputSettings{Enabled: true})
	c := h.add("desk")
	takeOutput(t, c)
	s := &outputSource{inst: &fleet.Instance{}}
	h.sources["f/i"] = s
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { defer close(done); h.serveSource(ctx, "f/i", s) }()
	go func() { fake.emit("ready"); fake.eventsW.Write(bytes.Repeat([]byte{1, 2, 3, 4}, output.ChunkBytes/4)) }()
	if got := takeOutput(t, c).GetAudio(); got.GetInstance() != "f/i" || len(got.GetPcm()) != output.ChunkBytes {
		t.Fatalf("source frame %v", got)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("source did not stop")
	}
	if !fake.isClosed() {
		t.Fatal("remote exec was not closed")
	}
}

func TestOutputReconcileWatchAndRemotePlayback(t *testing.T) {
	isolateFleetDir(t)
	seedForwardInstance(t)
	if err := state.SaveConfig(&state.Config{OutputSettings: state.OutputSettings{Enabled: true}}); err != nil {
		t.Fatal(err)
	}
	origOpen, origStop := openOutputSource, stopOutputServer
	defer func() { openOutputSource, stopOutputServer = origOpen, origStop }()
	opened := make(chan *fakeMicSink, 8)
	stopped := make(chan string, 8)
	openOutputSource = func(*fleet.Instance) (io.ReadWriteCloser, bool, error) {
		f := newFakeMicSink()
		opened <- f
		go func() {
			f.emit("ready")
			_, _ = f.eventsW.Write(bytes.Repeat([]byte{7, 8, 9, 10}, output.ChunkBytes/4))
		}()
		return f, true, nil
	}
	stopOutputServer = func(inst *fleet.Instance) { stopped <- inst.ContainerID }
	svc, client, cleanup := newTestServer(t)
	defer cleanup()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	runCtx, stopRun := context.WithCancel(ctx)
	ran := make(chan struct{})
	go func() { defer close(ran); svc.output.run(runCtx) }()
	defer func() { stopRun(); <-ran }()
	watch, err := client.Watch(ctx, &fleetgrpc.WatchRequest{IncludeInitialState: true})
	if err != nil {
		t.Fatal(err)
	}
	stream, err := client.Output(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := stream.Send(&fleetgrpc.OutputUp{Msg: &fleetgrpc.OutputUp_Open{Open: &fleetgrpc.OutputOpen{Client: "remote-laptop"}}}); err != nil {
		t.Fatal(err)
	}
	var source *fakeMicSink
	select {
	case source = <-opened:
	case <-ctx.Done():
		t.Fatal("running instance never got an output source")
	}
	for {
		frame, err := stream.Recv()
		if err != nil {
			t.Fatal(err)
		}
		if audio := frame.GetAudio(); audio != nil {
			if audio.Instance != "alpha/i1" || len(audio.Pcm) != output.ChunkBytes || audio.Pcm[0] != 7 {
				t.Fatalf("bad routed audio: %v", audio)
			}
			break
		}
	}
	for {
		ev, err := watch.Recv()
		if err != nil {
			t.Fatal(err)
		}
		if targets := ev.GetOutputTargets(); targets != nil && len(targets.Targets) > 0 {
			if targets.Targets[0].Client != "remote-laptop" || !targets.Targets[0].Selected || !targets.GetSettings().GetEnabled() {
				t.Fatalf("bad watch snapshot: %v", targets)
			}
			break
		}
	}
	if _, err := client.SetConfig(ctx, &fleetgrpc.SetConfigRequest{Config: &fleetgrpc.Config{Output: &fleetgrpc.OutputSettings{}}}); err != nil {
		t.Fatal(err)
	}
	select {
	case id := <-stopped:
		if id != "c1" {
			t.Fatalf("stopped wrong instance %q", id)
		}
	case <-ctx.Done():
		t.Fatal("disabled output was not removed")
	}
	eventually(t, "source closed on disable", source.isClosed)
}
