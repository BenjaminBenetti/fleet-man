package server

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/BenjaminBenetti/fleet-man/fleetgrpc"
	"github.com/BenjaminBenetti/fleet-man/internal/backendutil"
	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	"github.com/BenjaminBenetti/fleet-man/internal/fleetlaunch"
	"github.com/BenjaminBenetti/fleet-man/internal/flog"
	"github.com/BenjaminBenetti/fleet-man/internal/mic"
	"github.com/BenjaminBenetti/fleet-man/internal/startup"
	"github.com/BenjaminBenetti/fleet-man/internal/state"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// mic.go is the SERVER half of the virtual microphone. Three parties meet here:
//
//   - PROVIDERS: clients (TUIs) holding a Mic stream. They own the real
//     microphone, on whatever machine the human sits at. The most recently
//     attached one is ACTIVE; the rest stand by and are promoted if it leaves —
//     superseding rather than rejecting, so two TUIs never fight over the slot.
//   - SINKS: one `fleet mic sink` process inside every running instance
//     (internal/micsink), reached through the backend's MicSinkCommand. Audio
//     written to a sink's stdin is that instance's microphone; its stdout
//     reports DEMAND — whether anything in the instance is recording.
//   - the HUB below, which relays demand to the active provider (so the real
//     microphone is only open while someone is listening) and fans the
//     provider's audio out to exactly the sinks that asked for it.
//
// Sinks exist only while the feature is enabled AND a provider is attached:
// with nobody to supply audio there is nothing to inject.

const (
	// micSyncInterval is how often the hub reconciles its sinks against the
	// running instances (the control registry's cadence).
	micSyncInterval = time.Second
	// micSinkQueue bounds a sink's pending audio, in frames. A sink that stops
	// draining (a wedged docker exec) drops audio rather than stalling the
	// provider's stream for every other instance.
	micSinkQueue = 64
	// micRetryQuick / micRetrySlow space re-attach attempts: quick after a sink
	// that had been working (container restart), slow after one that never came
	// up (no packages and no way to install them).
	micRetryQuick = 2 * time.Second
	micRetrySlow  = 5 * time.Minute
	// micPrepareRetry is how long the hub waits before running the lazy install
	// on the same container AGAIN. "Once" would be wrong: one bad minute (a
	// network hiccup mid-apt) must not cost the instance its microphone until
	// the daemon restarts. "Every attach" would be wrong too: an image that can
	// never be prepared (no package manager, no sudo) would run apt forever.
	micPrepareRetry = 30 * time.Minute
)

// micSinkConn is a running sink: Write feeds PCM to its stdin, Read yields its
// stdout event lines, Close kills it.
type micSinkConn = io.ReadWriteCloser

// Seams so tests can drive the hub with no containers.
var (
	// openMicSink starts the in-instance sink. ok=false means the instance's
	// backend cannot host one.
	openMicSink = func(inst *fleet.Instance) (conn micSinkConn, ok bool, err error) {
		cmd, ok := backendutil.NewForInstance(inst, false).MicSinkCommand(inst.ContainerID)
		if !ok {
			return nil, false, nil
		}
		conn, err = startStdioBridge(cmd)
		return conn, true, err
	}

	// prepareMicInstance makes an instance able to run a sink: a current staged
	// fleet binary (an instance provisioned by an older fleet has no `mic sink`)
	// and the audio packages (an instance created before the feature was turned
	// on has none). It is the lazy twin of the provisioning-time install.
	prepareMicInstance = func(inst *fleet.Instance) error {
		b := backendutil.NewForInstance(inst, false)
		if _, err := fleetlaunch.EnsureFresh(b, inst.WorkspaceDir, nil); err != nil {
			return fmt.Errorf("stage fleet binary: %w", err)
		}
		if failures := startup.Run(b, inst.WorkspaceDir, []startup.Script{startup.MicScript()}); len(failures) > 0 {
			return failures[0]
		}
		return nil
	}

	// stopMicServer shuts the instance's private sound server down, so that with
	// the feature off the instance really has no microphone. Best-effort.
	stopMicServer = func(inst *fleet.Instance) {
		b := backendutil.NewForInstance(inst, false)
		if !b.SupportsMicSink() {
			return
		}
		_, _ = b.RunScript(inst.ContainerID, fleetlaunch.RemotePath+" mic stop >/dev/null 2>&1")
	}

	// micEnabled reads the global toggle.
	micEnabled = func() bool {
		config, err := state.LoadConfig()
		return err == nil && config.MicSettings.Enabled
	}
)

// micProvider is one attached Mic stream.
type micProvider struct {
	// demand carries the latest demand to the stream's send loop. Capacity 1,
	// latest wins: a provider only ever needs the CURRENT answer.
	demand chan *fleetgrpc.MicDemand
	// kicked is closed when the hub ends the stream (feature turned off).
	kicked chan struct{}
}

func (p *micProvider) post(demand *fleetgrpc.MicDemand) {
	for {
		select {
		case p.demand <- demand:
			return
		default:
		}
		select {
		case <-p.demand:
		default:
		}
	}
}

// micSink is the hub's record of one instance's sink.
type micSink struct {
	inst        *fleet.Instance
	containerID string

	// conn is nil while the sink is still attaching (or being prepared).
	conn  micSinkConn
	audio chan []byte
	// demand is the sink's last reported state.
	demand bool
	// closed marks a sink the hub has dropped, so its goroutines stop touching
	// hub state.
	closed bool
}

// micHub owns providers, sinks and the routing between them.
type micHub struct {
	mu        sync.Mutex
	providers []*micProvider // attach order; the last is active
	sinks     map[string]*micSink
	// retryAt backs off re-attach per instance key; preparedAt is when the lazy
	// install last ran for a container (see micPrepareRetry).
	retryAt    map[string]time.Time
	preparedAt map[string]time.Time
	// lastDemand is what the active provider was last told.
	lastDemand []string

	// wake pokes the sync loop (provider attach/detach) so a fresh TUI does not
	// wait out a tick before sinks appear.
	wake chan struct{}
}

func newMicHub() *micHub {
	return &micHub{
		sinks:      make(map[string]*micSink),
		retryAt:    make(map[string]time.Time),
		preparedAt: make(map[string]time.Time),
		wake:       make(chan struct{}, 1),
	}
}

func (h *micHub) poke() {
	select {
	case h.wake <- struct{}{}:
	default:
	}
}

// run reconciles sinks until ctx is cancelled, then tears everything down.
func (h *micHub) run(ctx context.Context) {
	defer h.closeAllSinks()
	ticker := time.NewTicker(micSyncInterval)
	defer ticker.Stop()
	for {
		h.sync()
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		case <-h.wake:
		}
	}
}

// sync brings the sink set in line with: a provider attached, the feature
// enabled, and the currently running instances.
func (h *micHub) sync() {
	h.mu.Lock()
	haveProvider := len(h.providers) > 0
	h.mu.Unlock()
	if !haveProvider {
		// Nobody to supply audio, so nothing to inject. Checked BEFORE the config
		// read so the daemon's steady state (no TUI attached) does no per-second
		// work for this feature.
		h.closeAllSinks()
		return
	}
	if !micEnabled() {
		// Normally disable() got here first (SetConfig calls it); this covers a
		// config.json edited by hand.
		h.kickProviders()
		h.closeAllSinks()
		return
	}

	st, err := state.Load()
	if err != nil {
		return
	}
	want := make(map[string]*fleet.Instance)
	for fleetName, f := range st.Fleets {
		for _, inst := range f.Instances {
			if inst.Status == fleet.StatusRunning && inst.ContainerID != "" {
				want[fleetName+"/"+inst.Name] = inst
			}
		}
	}

	h.mu.Lock()
	var dropped []*micSink
	for key, sink := range h.sinks {
		if inst, ok := want[key]; !ok || inst.ContainerID != sink.containerID {
			dropped = append(dropped, h.removeSinkLocked(key))
		}
	}
	var attach []string
	now := time.Now()
	for key, inst := range want {
		if _, ok := h.sinks[key]; ok {
			continue
		}
		if at, ok := h.retryAt[key]; ok && now.Before(at) {
			continue
		}
		h.sinks[key] = &micSink{inst: inst, containerID: inst.ContainerID}
		attach = append(attach, key)
	}
	for key := range h.retryAt {
		if _, ok := want[key]; !ok {
			delete(h.retryAt, key)
		}
	}
	h.publishDemandLocked()
	h.mu.Unlock()

	for _, sink := range dropped {
		sink.close()
	}
	for _, key := range attach {
		go h.attach(key)
	}
}

// attach starts key's sink and serves it until it exits. Runs on its own
// goroutine: preparing an instance can take a minute, and must not hold up the
// other instances' sinks.
func (h *micHub) attach(key string) {
	h.mu.Lock()
	sink, ok := h.sinks[key]
	h.mu.Unlock()
	if !ok {
		return
	}

	ready, supported, err := h.serve(key, sink)
	if !supported {
		// Not an error: this backend has no sink. Re-check rarely, in case the
		// instance is recreated on another backend under the same name.
		h.retire(key, sink, micRetrySlow)
		return
	}
	h.mu.Lock()
	dropped := sink.closed
	h.mu.Unlock()
	if dropped {
		// The hub closed this sink itself (provider left, instance stopped,
		// feature turned off); the "error" is just our own Close.
		flog.Info("mic sink detached", "instance", key)
		return
	}
	if ready {
		// It had been working and died on its own — typically the container
		// restarting. Come back quickly.
		flog.Info("mic sink lost", "instance", key, "err", err)
		h.retire(key, sink, micRetryQuick)
		return
	}

	// The sink never came up. The usual reason is an instance that predates the
	// feature; install what it needs and try again — but not more often than
	// micPrepareRetry per container.
	h.mu.Lock()
	last, tried := h.preparedAt[sink.containerID]
	recentlyPrepared := tried && time.Since(last) < micPrepareRetry
	if !recentlyPrepared {
		h.preparedAt[sink.containerID] = time.Now()
	}
	h.mu.Unlock()
	if recentlyPrepared {
		flog.Warn("mic sink failed", "instance", key, "err", err)
		h.retire(key, sink, micRetrySlow)
		return
	}
	flog.Info("mic: preparing instance", "instance", key, "reason", err)
	if prepErr := prepareMicInstance(sink.inst); prepErr != nil {
		// Surfaced, but NOT the end of the road: the script can fail on its last
		// step (an image whose own /etc/asound.conf bypasses PulseAudio) with a
		// perfectly good sound server installed, so the sink still gets its
		// retry below. If that fails too, the back-off above takes over.
		flog.Warn("mic: prepare instance failed", "instance", key, "err", prepErr)
		state.WriteWarn(fleetOf(key), sink.inst.Name, fmt.Sprintf("virtual microphone: %v", prepErr))
	}
	h.retire(key, sink, 0)
	h.poke()
}

// serve runs one sink process to completion. ready reports whether it ever
// announced itself; supported=false means the backend has no sink at all.
func (h *micHub) serve(key string, sink *micSink) (ready, supported bool, err error) {
	conn, supported, err := openMicSink(sink.inst)
	if !supported {
		return false, false, nil
	}
	if err != nil {
		return false, true, err
	}

	audio := make(chan []byte, micSinkQueue)
	h.mu.Lock()
	if sink.closed {
		h.mu.Unlock()
		_ = conn.Close()
		return false, true, fmt.Errorf("dropped while attaching")
	}
	sink.conn, sink.audio = conn, audio
	h.mu.Unlock()

	// Writer: the only goroutine writing to the sink's stdin. Ends when the hub
	// closes the conn (Write fails) or drops the sink (channel closed).
	go func() {
		for pcm := range audio {
			if _, err := conn.Write(pcm); err != nil {
				return
			}
		}
	}()

	var sinkErr error
	scanner := bufio.NewScanner(conn)
	for scanner.Scan() {
		event, detail, _ := strings.Cut(scanner.Text(), " ")
		switch event {
		case "ready":
			ready = true
			flog.Info("mic sink attached", "instance", key)
		case "demand":
			h.setDemand(key, sink, detail == "1")
		case "error":
			sinkErr = fmt.Errorf("sink: %s", detail)
		}
	}
	if sinkErr == nil {
		sinkErr = scanner.Err()
	}
	if sinkErr == nil && !ready {
		sinkErr = fmt.Errorf("sink exited before becoming ready (is %s current?)", fleetlaunch.RemotePath)
	}
	return ready, true, sinkErr
}

// retire removes a finished sink and schedules when it may be re-attached.
func (h *micHub) retire(key string, sink *micSink, backoff time.Duration) {
	h.mu.Lock()
	if h.sinks[key] == sink {
		h.removeSinkLocked(key)
		if backoff > 0 {
			h.retryAt[key] = time.Now().Add(backoff)
		}
		h.publishDemandLocked()
	}
	h.mu.Unlock()
	sink.close()
}

// removeSinkLocked drops key from the sink set. The caller closes the returned
// sink outside the lock (Close kills a process).
func (h *micHub) removeSinkLocked(key string) *micSink {
	sink := h.sinks[key]
	delete(h.sinks, key)
	sink.closed = true
	sink.demand = false
	if sink.audio != nil {
		close(sink.audio)
		sink.audio = nil
	}
	return sink
}

func (s *micSink) close() {
	if s.conn != nil {
		_ = s.conn.Close()
	}
}

// closeAllSinks detaches every sink.
func (h *micHub) closeAllSinks() {
	h.mu.Lock()
	var dropped []*micSink
	for key := range h.sinks {
		dropped = append(dropped, h.removeSinkLocked(key))
	}
	clear(h.retryAt)
	h.publishDemandLocked()
	h.mu.Unlock()
	for _, sink := range dropped {
		sink.close()
	}
}

// disable is the feature being turned OFF (SetConfig): end every provider
// stream, detach every sink, and shut the instances' private sound servers down
// so that "off" really means the instances have no microphone — not a silent
// one that recorders still happily open.
func (h *micHub) disable() {
	flog.Info("microphone disabled")
	h.kickProviders()
	h.closeAllSinks()
	st, err := state.Load()
	if err != nil {
		return
	}
	for _, f := range st.Fleets {
		for _, inst := range f.Instances {
			if inst.Status == fleet.StatusRunning && inst.ContainerID != "" {
				go stopMicServer(inst)
			}
		}
	}
}

// setDemand records a sink's demand report and republishes the aggregate.
func (h *micHub) setDemand(key string, sink *micSink, active bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if sink.closed || sink.demand == active {
		return
	}
	sink.demand = active
	h.publishDemandLocked()
}

// publishDemandLocked tells the active provider who is recording, if that
// changed. Every transition is logged: this is the audit trail of when the
// human's microphone was live, and for whom.
func (h *micHub) publishDemandLocked() {
	var demanding []string
	for key, sink := range h.sinks {
		if sink.demand {
			demanding = append(demanding, key)
		}
	}
	slices.Sort(demanding)
	if slices.Equal(demanding, h.lastDemand) {
		return
	}
	h.lastDemand = demanding
	if len(h.providers) == 0 {
		return
	}
	if len(demanding) > 0 {
		flog.Info("mic live", "instances", strings.Join(demanding, ","))
	} else {
		flog.Info("mic idle")
	}
	h.providers[len(h.providers)-1].post(demandMessage(demanding))
}

func demandMessage(demanding []string) *fleetgrpc.MicDemand {
	return &fleetgrpc.MicDemand{Active: len(demanding) > 0, Instances: demanding}
}

// addProvider attaches a stream as the new active provider. The previous active
// one is told to stop capturing; the new one is greeted with current demand.
func (h *micHub) addProvider() *micProvider {
	provider := &micProvider{demand: make(chan *fleetgrpc.MicDemand, 1), kicked: make(chan struct{})}
	h.mu.Lock()
	if n := len(h.providers); n > 0 {
		h.providers[n-1].post(demandMessage(nil))
	}
	h.providers = append(h.providers, provider)
	provider.post(demandMessage(h.lastDemand))
	count := len(h.providers)
	h.mu.Unlock()
	flog.Info("mic provider attached", "providers", count)
	h.poke()
	return provider
}

// removeProvider detaches a stream, promoting the newest stand-by if it was the
// active one.
func (h *micHub) removeProvider(provider *micProvider) {
	h.mu.Lock()
	index := slices.Index(h.providers, provider)
	if index < 0 {
		h.mu.Unlock()
		return
	}
	wasActive := index == len(h.providers)-1
	h.providers = slices.Delete(h.providers, index, index+1)
	if n := len(h.providers); wasActive && n > 0 {
		h.providers[n-1].post(demandMessage(h.lastDemand))
	}
	count := len(h.providers)
	h.mu.Unlock()
	flog.Info("mic provider detached", "providers", count)
	h.poke()
}

// kickProviders ends every provider stream (the feature was turned off).
func (h *micHub) kickProviders() {
	h.mu.Lock()
	kicked := h.providers
	h.providers = nil
	h.mu.Unlock()
	for _, provider := range kicked {
		close(provider.kicked)
	}
}

// route fans one audio frame from provider out to every sink with demand.
// Frames from a stand-by provider, or for a sink whose queue is full, are
// dropped — live audio is worthless late.
func (h *micHub) route(provider *micProvider, pcm []byte) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if n := len(h.providers); n == 0 || h.providers[n-1] != provider {
		return
	}
	for _, sink := range h.sinks {
		if !sink.demand || sink.audio == nil {
			continue
		}
		select {
		case sink.audio <- pcm:
		default:
		}
	}
}

func fleetOf(key string) string {
	fleetName, _, _ := splitInstanceKey(key)
	return fleetName
}

// Mic implements the virtual-microphone data plane. The first client frame must
// carry the MicOpen header; afterwards the client streams PCM up while the
// server streams demand down.
func (s *service) Mic(stream fleetgrpc.FleetService_MicServer) error {
	first, err := stream.Recv()
	if err != nil {
		return err
	}
	open := first.GetOpen()
	if open == nil {
		return status.Error(codes.InvalidArgument, "first Mic frame must carry open")
	}
	if open.GetSampleRate() != mic.SampleRate || open.GetChannels() != mic.Channels {
		return status.Errorf(codes.InvalidArgument, "unsupported format %d Hz x%d (want %d Hz x%d s16le)",
			open.GetSampleRate(), open.GetChannels(), mic.SampleRate, mic.Channels)
	}
	if !micEnabled() {
		return status.Error(codes.FailedPrecondition, "the microphone is disabled in settings")
	}

	provider := s.mic.addProvider()
	defer s.mic.removeProvider(provider)

	recvDone := make(chan error, 1)
	go func() {
		for {
			up, err := stream.Recv()
			if err != nil {
				recvDone <- err
				return
			}
			if pcm := up.GetAudio(); len(pcm) > 0 {
				s.mic.route(provider, pcm)
			}
		}
	}()

	for {
		select {
		case demand := <-provider.demand:
			if err := stream.Send(&fleetgrpc.MicDown{Msg: &fleetgrpc.MicDown_Demand{Demand: demand}}); err != nil {
				return err
			}
		case err := <-recvDone:
			if err == io.EOF {
				return nil
			}
			return err
		case <-provider.kicked:
			return status.Error(codes.FailedPrecondition, "the microphone was disabled in settings")
		case <-stream.Context().Done():
			return stream.Context().Err()
		}
	}
}
