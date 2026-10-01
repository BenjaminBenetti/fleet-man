package server

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/BenjaminBenetti/fleet-man/fleetgrpc"
	"github.com/BenjaminBenetti/fleet-man/internal/backendutil"
	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	"github.com/BenjaminBenetti/fleet-man/internal/fleetlaunch"
	"github.com/BenjaminBenetti/fleet-man/internal/flog"
	"github.com/BenjaminBenetti/fleet-man/internal/mic"
	"github.com/BenjaminBenetti/fleet-man/internal/micsink"
	"github.com/BenjaminBenetti/fleet-man/internal/startup"
	"github.com/BenjaminBenetti/fleet-man/internal/state"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// mic.go is the SERVER half of the virtual microphone. Three parties meet here:
//
//   - PROVIDERS: clients (TUIs) holding a Mic stream. Each owns the real
//     microphones of the machine it runs on, and announces that machine's name
//     and capture devices. Exactly one is the SOURCE — the one sent demand and
//     whose audio is routed: the client Settings selects, if it is attached,
//     otherwise the most recently attached one. The rest stand by and take over
//     if the source leaves — superseding rather than rejecting, so two TUIs
//     never fight over the slot.
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
	// maxMicFrameBytes bounds one audio frame. A frame is 40 ms (mic.ChunkBytes);
	// the bound is generous — a client may coalesce — but without one a provider
	// (which may be remote) sending frames at gRPC's 4 MiB message ceiling parks
	// a quarter of a gigabyte in the sink queues before the drop policy engages.
	maxMicFrameBytes = 16 * mic.ChunkBytes
	// micPrepareTimeout bounds one lazy install, and micPrepareParallel how many
	// run at once. Until the hub, startup scripts only ran from provisioning —
	// one instance at a time, with a human watching. Here they run from a
	// background loop: unbounded, turning the feature on across thirty instances
	// would start thirty concurrent apt-gets, and one apt hung on an unreachable
	// mirror would hold its instance's sink slot for the daemon's lifetime.
	micPrepareTimeout  = 10 * time.Minute
	micPrepareParallel = 2
	// micOpenParallel bounds concurrent sink opens (each may probe docker).
	micOpenParallel = 8
)

// micSinkOpenTimeout bounds starting a sink (see serve). A var for tests.
var micSinkOpenTimeout = 30 * time.Second

// bounded runs fn, giving up after timeout. fn keeps running in its goroutine if
// it is truly stuck — there is nothing to cancel it with, the Backend interface
// takes no context — but the CALLER is released, which is what keeps one hung
// docker call from holding an instance's slot forever. If the abandoned fn later
// produces something (an opened sink), it is the caller's job to have made that
// harmless; serve does so by checking the result only on the non-timeout path.
func bounded(timeout time.Duration, what string, fn func() error) error {
	done := make(chan error, 1)
	go func() { done <- fn() }()
	select {
	case err := <-done:
		return err
	case <-time.After(timeout):
		return fmt.Errorf("%s: no answer within %s", what, timeout)
	}
}

// micSinkReadyTimeout is how long a freshly-started sink may take to announce
// itself. Generous: it covers a cold `docker exec` plus the sink bringing the
// sound server up (itself bounded at ~5 s). A var so tests can shorten it.
var micSinkReadyTimeout = 45 * time.Second

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
		if failures := startup.RunWithTimeout(b, inst.WorkspaceDir, []startup.Script{startup.MicScript()}, micPrepareTimeout); len(failures) > 0 {
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

	// micSetting reads the global toggle. The error matters: "the config could
	// not be read" (a config.json caught mid-write) is NOT "the microphone is
	// off", and must neither tear the feature down nor tell the user to go and
	// flip a toggle that is already on.
	micSetting = func() (settings state.MicSettings, err error) {
		config, err := state.LoadConfig()
		if err != nil {
			return state.MicSettings{}, err
		}
		return config.MicSettings, nil
	}
)

// micProvider is one attached Mic stream.
type micProvider struct {
	// client is the machine the provider records on (MicOpen.client, scrubbed).
	// Empty for a client that predates the field: it can be the source by being
	// the most recently attached, but it cannot be selected by name.
	client string
	// devices is the provider's last listing; listed says one has arrived.
	// Guarded by the hub's mu.
	devices []*fleetgrpc.MicDevice
	listed  bool
	// demand carries the latest demand to the stream's send loop. Capacity 1,
	// latest wins: a provider only ever needs the CURRENT answer.
	demand chan *fleetgrpc.MicDemand
	// relist asks the stream's send loop to request a fresh device listing.
	// Capacity 1: requests waiting to be sent are the same request.
	relist chan struct{}
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
	// lastDemand / lastDevice are what the source was last told, and lastSource
	// who that was.
	lastDemand []string
	lastDevice string
	lastSource *micProvider
	// client / device are the selection made in Settings: which client's
	// microphone, and which capture device on it. Pushed to the source with
	// every demand (see MicDemand).
	client string
	device string
	// teardowns counts closeAllSinks calls, so a sync that read its inputs
	// BEFORE a teardown can tell, under the lock, that they are stale.
	teardowns int
	// prepareSlots bounds concurrent lazy installs (micPrepareParallel).
	prepareSlots chan struct{}
	// attaching tracks the attach goroutines, so run can leave nothing behind.
	attaching sync.WaitGroup
	// openSlots bounds concurrent sink opens (micOpenParallel).
	openSlots chan struct{}

	// wake pokes the sync loop (provider attach/detach) so a fresh TUI does not
	// wait out a tick before sinks appear.
	wake chan struct{}

	// sourcesDirty signals that sources() would now answer differently, and
	// onSources is who wants to know (the Watch hub; nil in tests). Decoupled by
	// the channel because every change happens under mu, where calling out —
	// into a hub loop that may itself be waiting for mu — is not an option.
	sourcesDirty chan struct{}
	onSources    func()
}

func newMicHub() *micHub {
	return &micHub{
		sinks:        make(map[string]*micSink),
		retryAt:      make(map[string]time.Time),
		preparedAt:   make(map[string]time.Time),
		wake:         make(chan struct{}, 1),
		sourcesDirty: make(chan struct{}, 1),
		prepareSlots: make(chan struct{}, micPrepareParallel),
		openSlots:    make(chan struct{}, micOpenParallel),
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
	// On the way out: close every sink (which ends each attach goroutine's
	// serve), then wait for those goroutines, so nothing of the hub outlives it.
	defer h.attaching.Wait()
	defer h.closeAllSinks()
	announcing := make(chan struct{})
	go func() { defer close(announcing); h.announceSources(ctx) }()
	defer func() { <-announcing }()
	// Announce once up front, so a client that subscribes before anything has
	// changed is still handed the settings the daemon holds.
	h.sourcesChanged()
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
	teardowns := h.teardowns
	h.mu.Unlock()
	if !haveProvider {
		// Nobody to supply audio, so nothing to inject. Checked BEFORE the config
		// read so the daemon's steady state (no TUI attached) does no per-second
		// work for this feature.
		h.closeAllSinks()
		return
	}
	settings, err := micSetting()
	if err != nil {
		return // unreadable right now: change nothing, look again next tick
	}
	if !settings.Enabled {
		// Normally disable() got here first (SetConfig calls it); this covers a
		// config.json edited by hand. It must keep the SAME promise — "off"
		// means no microphone in the instances, not a silent one — so it is the
		// same teardown. Not per tick: kickProviders empties the provider set,
		// and sync returns before this point while it is empty.
		h.disable()
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
	// Everything above was read WITHOUT the lock, across two file reads. If the
	// feature was torn down meanwhile (disable(), or the last provider leaving)
	// those inputs are stale, and acting on them would inject a sink — and with
	// it a detached sound server — into every instance right after "off"
	// promised there would be none. Decide under the lock, or not at all.
	if h.teardowns != teardowns || len(h.providers) == 0 {
		h.mu.Unlock()
		return
	}
	if h.client != settings.Client || h.device != settings.Device {
		// A selection that did not come through SetConfig (config.json edited
		// by hand): the clients showing it must hear of it all the same.
		h.sourcesChanged()
	}
	h.client, h.device = settings.Client, settings.Device
	var dropped []*micSink
	for key, sink := range h.sinks {
		if inst, ok := want[key]; !ok || inst.ContainerID != sink.containerID {
			dropped = append(dropped, h.removeSinkLocked(key))
		}
	}
	type launch struct {
		key  string
		sink *micSink
	}
	var attach []launch
	now := time.Now()
	for key, inst := range want {
		if _, ok := h.sinks[key]; ok {
			continue
		}
		if at, ok := h.retryAt[key]; ok && now.Before(at) {
			continue
		}
		sink := &micSink{inst: inst, containerID: inst.ContainerID}
		h.sinks[key] = sink
		attach = append(attach, launch{key, sink})
	}
	// Both back-off maps are pruned HERE, against the running set, and nowhere
	// else: an entry lives exactly as long as its instance / container does.
	for key := range h.retryAt {
		if _, ok := want[key]; !ok {
			delete(h.retryAt, key)
		}
	}
	liveContainers := make(map[string]bool, len(want))
	for _, inst := range want {
		liveContainers[inst.ContainerID] = true
	}
	for containerID := range h.preparedAt {
		if !liveContainers[containerID] {
			delete(h.preparedAt, containerID)
		}
	}
	h.publishDemandLocked()
	h.mu.Unlock()

	for _, sink := range dropped {
		sink.close()
	}
	for _, launch := range attach {
		h.attaching.Add(1)
		go func() {
			defer h.attaching.Done()
			h.attach(launch.key, launch.sink)
		}()
	}
}

// attach starts key's sink and serves it until it exits. Runs on its own
// goroutine: preparing an instance can take a minute, and must not hold up the
// other instances' sinks.
//
// It is handed the exact sink sync created for it rather than looking the key
// up: between sync's unlock and this goroutine running, the entry can be dropped
// and re-created (provider leaves and returns), and a lookup would then give two
// attach goroutines the SAME sink — two processes in one container, the second
// overwriting the first's conn so it is never closed.
func (h *micHub) attach(key string, sink *micSink) {
	h.mu.Lock()
	current := h.sinks[key] == sink
	h.mu.Unlock()
	if !current {
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
		// Stamped at the START of the attempt, not on completion, and that is
		// load-bearing: it is what stops a second attach from running apt
		// concurrently against the same container. (So the effective window is
		// micPrepareRetry minus the install's duration, and success and failure
		// are recorded alike — both fine.)
		h.preparedAt[sink.containerID] = time.Now()
	}
	h.mu.Unlock()
	if recentlyPrepared {
		flog.Warn("mic sink failed", "instance", key, "err", err)
		h.retire(key, sink, micRetrySlow)
		return
	}
	// Bounded fan-out (micPrepareParallel). The wait for a slot can be long — so
	// it comes BEFORE the last look below, never between it and the install.
	h.prepareSlots <- struct{}{}
	defer func() { <-h.prepareSlots }()

	// Last look before installing anything: the feature may have been turned
	// off (or this sink dropped) while the sink was failing or this goroutine
	// was queued. "Off" promises that nothing is installed, and an install is
	// not something we can cancel once it is running.
	// An attempt that is ABANDONED — nothing was installed — must hand back its
	// stamp as well as its slot. Otherwise closing the TUI while installs are
	// queued (or one unreadable config read) leaves every one of those instances
	// "recently prepared", and they sit mic-less for the whole retry window
	// although no install ever ran.
	abandon := func() {
		h.mu.Lock()
		delete(h.preparedAt, sink.containerID)
		h.mu.Unlock()
	}
	h.mu.Lock()
	dropped = sink.closed
	if !dropped {
		// Re-stamped now that the install is really about to start: the stamp is
		// the in-flight guard, and a queue wait can outlast micPrepareRetry.
		h.preparedAt[sink.containerID] = time.Now()
	}
	h.mu.Unlock()
	if dropped {
		abandon()
		return
	}
	if settings, err := micSetting(); err != nil || !settings.Enabled {
		// Off — or unreadable just now, which must not cost this instance its
		// microphone: EVERY exit from attach has to hand the registration back,
		// or sync finds the key occupied and skips this instance forever.
		abandon()
		h.retire(key, sink, micRetryQuick)
		return
	}
	flog.Info("mic: preparing instance", "instance", key, "reason", err)
	if prepErr := bounded(micPrepareTimeout+2*time.Minute, "prepare", func() error { return prepareMicInstance(sink.inst) }); prepErr != nil {
		// Surfaced, but NOT the end of the road: the script can fail on its last
		// step (an image whose own /etc/asound.conf bypasses PulseAudio) with a
		// perfectly good sound server installed, so the sink still gets its
		// retry below. If that fails too, the back-off above takes over.
		flog.Warn("mic: prepare instance failed", "instance", key, "err", prepErr)
		if settings, _ := micSetting(); settings.Enabled {
			warnMic(fleetOf(key), sink.inst.Name, fmt.Sprintf("virtual microphone: %v", prepErr))
		}
	}
	h.retire(key, sink, 0)
	h.poke()
}

// serve runs one sink process to completion. ready reports whether it ever
// announced itself; supported=false means the backend has no sink at all.
func (h *micHub) serve(key string, sink *micSink) (ready, supported bool, err error) {
	// OPENING the sink is bounded too, not just waiting for it to speak: it
	// resolves the container's user with docker inspect / devcontainer exec,
	// which hang when dockerd does — and a goroutine parked there holds this
	// instance's slot with no retire and no back-off, for the daemon's lifetime.
	// It is also rate-limited: every TUI open would otherwise fire one such
	// probe per instance at once.
	type opened struct {
		conn      micSinkConn
		supported bool
		err       error
	}
	h.openSlots <- struct{}{}
	result := make(chan opened, 1)
	go func() {
		conn, supported, err := openMicSink(sink.inst)
		result <- opened{conn, supported, err} // buffered: never blocks
	}()
	var got opened
	select {
	case got = <-result:
	case <-time.After(micSinkOpenTimeout):
		// Give up on it — but if it does come back with a running sink, that
		// process must not be leaked: whoever receives the result closes it.
		go func() {
			if late := <-result; late.conn != nil {
				_ = late.conn.Close()
			}
		}()
		<-h.openSlots
		return false, true, fmt.Errorf("open sink: no answer within %s", micSinkOpenTimeout)
	}
	<-h.openSlots
	if got.err == nil && !got.supported {
		return false, false, nil
	}
	if got.err != nil {
		return false, true, got.err
	}
	conn := got.conn

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

	// A sink that never says anything — a paused container, a hung dockerd, an
	// exec that neither prints nor EOFs — would park this goroutine, and with it
	// the instance's slot, for the whole provider session: no retire, so no
	// retry and no back-off. The in-instance half bounds itself; this is the
	// daemon-side equivalent. Closing the conn ends the scan below.
	var becameReady atomic.Bool
	readiness := time.AfterFunc(micSinkReadyTimeout, func() {
		if !becameReady.Load() {
			_ = conn.Close()
		}
	})
	defer readiness.Stop()

	var sinkErr error
	scanner := bufio.NewScanner(conn)
	for scanner.Scan() {
		event, detail, _ := strings.Cut(scanner.Text(), " ")
		switch event {
		case micsink.EventReady:
			ready = true
			becameReady.Store(true)
			flog.Info("mic sink attached", "instance", key)
		case micsink.EventDemand:
			h.setDemand(key, sink, detail == micsink.DemandOn)
		case micsink.EventError:
			sinkErr = fmt.Errorf("sink: %s", detail)
		}
	}
	if sinkErr == nil {
		sinkErr = scanner.Err()
	}
	if sinkErr == nil && !ready && !readiness.Stop() {
		sinkErr = fmt.Errorf("sink said nothing for %s", micSinkReadyTimeout)
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
	h.teardowns++
	for key := range h.sinks {
		dropped = append(dropped, h.removeSinkLocked(key))
	}
	// retryAt is deliberately NOT cleared: the last provider leaving is just a
	// TUI closing, and reopening it must not re-probe every unsupported or
	// broken instance at once. sync prunes entries for instances that are gone.
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
	// Off the caller's goroutine (SetConfig holds a lock), and with retries: the
	// enabled->disabled edge happens ONCE, so a state.json caught mid-write must
	// not mean the instances' sound servers are never stopped.
	go func() {
		for attempt := 0; attempt < 5; attempt++ {
			st, err := state.Load()
			if err != nil {
				time.Sleep(time.Second)
				continue
			}
			for _, f := range st.Fleets {
				for _, inst := range f.Instances {
					if inst.Status == fleet.StatusRunning && inst.ContainerID != "" {
						go stopMicServer(inst)
					}
				}
			}
			return
		}
		flog.Warn("microphone disabled, but the running instances could not be listed to stop their sound servers")
	}()
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

// sourceLocked is the provider whose microphone is recorded: the newest one
// attached from the selected client, or — with no selection, or with the
// selected client not attached — the newest one of all. Falling back rather than
// going silent is the same promise the device selection makes (a device that is
// not there records the system default): closing the laptop that was selected
// must not leave the instances with a dead microphone while another client is
// sitting right there.
func (h *micHub) sourceLocked() *micProvider {
	if h.client != "" {
		for i := len(h.providers) - 1; i >= 0; i-- {
			if h.providers[i].client == h.client {
				return h.providers[i]
			}
		}
	}
	if n := len(h.providers); n > 0 {
		return h.providers[n-1]
	}
	return nil
}

// deviceForLocked is the device to push to provider. The selected device is an
// id from the SELECTED client's enumeration, so a provider standing in for that
// client is not sent it: ids are not unique across machines ("the built-in
// microphone" has the same id on two laptops of one model), and a stand-in must
// record its default, not whatever happens to share the name.
func (h *micHub) deviceForLocked(provider *micProvider) string {
	if provider == nil || (h.client != "" && provider.client != h.client) {
		return ""
	}
	return h.device
}

// publishDemandLocked tells the source who is recording, if that — or who the
// source is — changed. Every transition is logged: this is the audit trail of
// when a human's microphone was live, whose, and for whom.
func (h *micHub) publishDemandLocked() {
	var demanding []string
	for key, sink := range h.sinks {
		if sink.demand {
			demanding = append(demanding, key)
		}
	}
	slices.Sort(demanding)
	source := h.sourceLocked()
	device := h.deviceForLocked(source)
	demandChanged := !slices.Equal(demanding, h.lastDemand)
	sourceChanged := source != h.lastSource
	if !demandChanged && !sourceChanged && device == h.lastDevice {
		return
	}
	previous := h.lastSource
	wasRecording := len(h.lastDemand) > 0
	h.lastDemand, h.lastDevice, h.lastSource = demanding, device, source
	if sourceChanged || wasRecording != (len(demanding) > 0) {
		h.sourcesChanged()
	}
	if sourceChanged && previous != nil && slices.Contains(h.providers, previous) {
		// Superseded, not gone: it must close its microphone.
		previous.post(h.demandMessageLocked(previous, nil))
	}
	if source == nil {
		return
	}
	switch {
	case len(demanding) == 0 && demandChanged:
		flog.Info("mic idle")
	case len(demanding) > 0 && (demandChanged || sourceChanged):
		flog.Info("mic live", "instances", strings.Join(demanding, ","), "client", source.client)
	}
	source.post(h.demandMessageLocked(source, demanding))
}

// demandMessageLocked is the one place a MicDemand is built, so the selection
// rides along with every one of them.
func (h *micHub) demandMessageLocked(provider *micProvider, demanding []string) *fleetgrpc.MicDemand {
	return &fleetgrpc.MicDemand{
		Active:    len(demanding) > 0,
		Instances: demanding,
		Device:    h.deviceForLocked(provider),
		Client:    h.client,
	}
}

// setSelection records a changed selection (SetConfig) and republishes, so the
// microphone moves to the newly selected client — or device — at once rather
// than at the next sync tick.
func (h *micHub) setSelection(client, device string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.client, h.device = client, device
	h.publishDemandLocked()
}

// addProvider attaches a stream. Whether it becomes the source is the
// selection's call (sourceLocked): if it does, the previous source is told to
// stop capturing and it is greeted with current demand; if it does not, it is
// greeted idle — every accepted stream is greeted, the greeting doubles as
// "attached".
func (h *micHub) addProvider(client string, settings state.MicSettings) *micProvider {
	provider := &micProvider{
		client: client,
		demand: make(chan *fleetgrpc.MicDemand, 1),
		relist: make(chan struct{}, 1),
		kicked: make(chan struct{}),
	}
	h.mu.Lock()
	h.client, h.device = settings.Client, settings.Device
	h.providers = append(h.providers, provider)
	if h.sourceLocked() != provider {
		provider.post(h.demandMessageLocked(provider, nil))
	}
	h.publishDemandLocked()
	h.sourcesChanged()
	count := len(h.providers)
	h.mu.Unlock()
	flog.Info("mic provider attached", "client", client, "providers", count)
	h.poke()
	return provider
}

// removeProvider detaches a stream; if it was the source, the next in line
// (sourceLocked) takes over.
func (h *micHub) removeProvider(provider *micProvider) {
	h.mu.Lock()
	index := slices.Index(h.providers, provider)
	if index < 0 {
		h.mu.Unlock()
		return
	}
	h.providers = slices.Delete(h.providers, index, index+1)
	h.publishDemandLocked()
	h.sourcesChanged()
	count := len(h.providers)
	h.mu.Unlock()
	flog.Info("mic provider detached", "client", provider.client, "providers", count)
	h.poke()
}

// kickProviders ends every provider stream (the feature was turned off).
func (h *micHub) kickProviders() {
	h.mu.Lock()
	kicked := h.providers
	h.providers = nil
	h.lastSource = nil
	h.sourcesChanged()
	h.mu.Unlock()
	for _, provider := range kicked {
		close(provider.kicked)
	}
}

// route fans one audio frame from provider out to every sink with demand.
// Frames from a provider that is not the source, or for a sink whose queue is
// full, are dropped — live audio is worthless late.
func (h *micHub) route(provider *micProvider, pcm []byte) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.sourceLocked() != provider {
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

// --- sources: who is attached, and what they can record ---------------------------

// cleanMicClient scrubs a provider's announced name. It is stored, compared
// against the selection and drawn in every other client's terminal.
func cleanMicClient(name string) string {
	return mic.CleanText(name, mic.MaxClientName)
}

// cleanMicDevices bounds and scrubs a provider's announced device listing, for
// the same reasons (a provider may be remote; see mic.CleanDevices).
func cleanMicDevices(list *fleetgrpc.MicDeviceList) []*fleetgrpc.MicDevice {
	var announced []mic.Device
	for _, device := range list.GetDevices() {
		// Bounded BEFORE scrubbing: the scrub is linear in what it is handed.
		if len(announced) == 4*mic.MaxDevices {
			break
		}
		label := device.GetLabel()
		if len(label) > 4*mic.MaxDeviceLabel {
			label = label[:4*mic.MaxDeviceLabel]
		}
		announced = append(announced, mic.Device{ID: device.GetId(), Label: label})
	}
	var devices []*fleetgrpc.MicDevice
	for _, device := range mic.CleanDevices(announced) {
		devices = append(devices, &fleetgrpc.MicDevice{Id: device.ID, Label: device.Label})
	}
	return devices
}

// setDevices records a provider's device listing.
func (h *micHub) setDevices(provider *micProvider, devices []*fleetgrpc.MicDevice) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if !slices.Contains(h.providers, provider) {
		return
	}
	provider.devices, provider.listed = devices, true
	h.sourcesChanged()
}

// relistDevices asks every attached provider for a fresh device listing.
func (h *micHub) relistDevices() {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, provider := range h.providers {
		select {
		case provider.relist <- struct{}{}:
		default:
		}
	}
}

// sources is the attached microphone clients and what each can record — what a
// selector offers. One entry per client NAME: several providers on one machine
// (two TUIs) are one source, described by the newest of them. Anonymous
// providers (no name: a client that predates it) are left out — there is
// nothing to select them by.
func (h *micHub) sources() *fleetgrpc.MicSources {
	h.mu.Lock()
	defer h.mu.Unlock()
	source := h.sourceLocked()
	recording := len(h.lastDemand) > 0
	byClient := make(map[string]*fleetgrpc.MicSource)
	out := &fleetgrpc.MicSources{}
	for i := len(h.providers) - 1; i >= 0; i-- {
		provider := h.providers[i]
		if provider.client == "" {
			continue
		}
		entry, seen := byClient[provider.client]
		if !seen {
			entry = &fleetgrpc.MicSource{Client: provider.client}
			byClient[provider.client] = entry
			out.Sources = append(out.Sources, entry)
		}
		if !entry.DevicesListed && provider.listed {
			entry.Devices, entry.DevicesListed = provider.devices, true
		}
		if provider == source {
			entry.Source, entry.Recording = true, recording
		}
	}
	slices.SortFunc(out.Sources, func(a, b *fleetgrpc.MicSource) int { return strings.Compare(a.GetClient(), b.GetClient()) })
	return out
}

// sourcesChanged flags that what a selector shows — the attached clients, or
// the settings that select among them — is no longer what was last announced.
// Safe with or without mu held: it only signals.
func (h *micHub) sourcesChanged() {
	select {
	case h.sourcesDirty <- struct{}{}:
	default:
	}
}

// announceSources calls onSources after every change, off the hub's lock. It is
// the ONLY caller, and calls it from this one goroutine: that is what keeps two
// changes in quick succession from being announced out of order.
func (h *micHub) announceSources(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-h.sourcesDirty:
			if h.onSources != nil {
				h.onSources()
			}
		}
	}
}

// warnMic adds a microphone warning to the instance's banner WITHOUT replacing
// what provisioning put there, and only once: the lazy install is retried every
// micPrepareRetry, and an unfixable image must not grow a banner of duplicates
// (or a fleet.log entry per retry).
func warnMic(fleetName, instanceName, warning string) {
	if existing, err := os.ReadFile(state.WarnPath(fleetName, instanceName)); err == nil && strings.Contains(string(existing), warning) {
		return
	}
	state.PrependWarn(fleetName, instanceName, warning)
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
	settings, err := micSetting()
	switch {
	case err != nil:
		// Not FailedPrecondition: the client renders that as "disabled — enable
		// it in Settings", pointing the user at a toggle that may well be on.
		return status.Errorf(codes.Unavailable, "cannot read the microphone setting: %v", err)
	case !settings.Enabled:
		return status.Error(codes.FailedPrecondition, "the microphone is disabled in settings")
	}

	provider := s.mic.addProvider(cleanMicClient(open.GetClient()), settings)
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
				// Rejected, not truncated: like the format check above, a client
				// that breaks the contract should fail loudly.
				if len(pcm) > maxMicFrameBytes {
					recvDone <- status.Errorf(codes.InvalidArgument, "audio frame of %d bytes exceeds %d", len(pcm), maxMicFrameBytes)
					return
				}
				s.mic.route(provider, pcm)
			}
			if list := up.GetDevices(); list != nil {
				s.mic.setDevices(provider, cleanMicDevices(list))
			}
		}
	}()

	for {
		select {
		case demand := <-provider.demand:
			if err := stream.Send(&fleetgrpc.MicDown{Msg: &fleetgrpc.MicDown_Demand{Demand: demand}}); err != nil {
				return err
			}
		case <-provider.relist:
			if err := stream.Send(&fleetgrpc.MicDown{Msg: &fleetgrpc.MicDown_ListDevices{ListDevices: &fleetgrpc.MicListDevices{}}}); err != nil {
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

// ListMicSources returns the attached microphone clients and their devices,
// optionally asking each provider to enumerate again first. The refreshed
// listings are not waited for: they arrive over Watch as the providers answer
// (an enumeration can take seconds against a wedged sound server, and a
// selector should not hang on the slowest client).
func (s *service) ListMicSources(_ context.Context, req *fleetgrpc.ListMicSourcesRequest) (*fleetgrpc.ListMicSourcesReply, error) {
	if req.GetRefresh() {
		s.mic.relistDevices()
	}
	return &fleetgrpc.ListMicSourcesReply{Sources: s.micSourcesNow(nil)}, nil
}

// micSourcesNow is what a selector needs, in one snapshot: the attached clients
// and the microphone settings that select among them, read from the config —
// the daemon is its only writer, so that IS what the daemon holds. If the config
// cannot be read just now (caught mid-write), lastKnown stands in: an absent
// group tells clients "not known, keep what you have", which must not replace a
// good value in the cache every new subscriber is greeted from.
func (s *service) micSourcesNow(lastKnown *fleetgrpc.MicSettings) *fleetgrpc.MicSources {
	sources := s.mic.sources()
	sources.Settings = lastKnown
	if settings, err := micSetting(); err == nil {
		client := settings.Client
		sources.Settings = &fleetgrpc.MicSettings{Enabled: settings.Enabled, Device: settings.Device, Client: &client}
	}
	return sources
}
