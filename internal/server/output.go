package server

import (
	"context"
	"fmt"
	"io"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/BenjaminBenetti/fleet-man/fleetgrpc"
	"github.com/BenjaminBenetti/fleet-man/internal/backend"
	"github.com/BenjaminBenetti/fleet-man/internal/backendutil"
	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	"github.com/BenjaminBenetti/fleet-man/internal/fleetlaunch"
	"github.com/BenjaminBenetti/fleet-man/internal/flog"
	"github.com/BenjaminBenetti/fleet-man/internal/mic"
	"github.com/BenjaminBenetti/fleet-man/internal/output"
	"github.com/BenjaminBenetti/fleet-man/internal/startup"
	"github.com/BenjaminBenetti/fleet-man/internal/state"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

var outputSetting = func() (state.OutputSettings, error) {
	c, err := state.LoadConfig()
	if err != nil {
		return state.OutputSettings{}, err
	}
	return c.OutputSettings, nil
}
var openOutputSource = func(inst *fleet.Instance) (io.ReadWriteCloser, bool, error) {
	b, ok := backendutil.NewForInstance(inst, false).(backend.OutputBackend)
	if !ok {
		return nil, false, nil
	}
	cmd, ok := b.OutputSourceCommand(inst.ContainerID)
	if !ok {
		return nil, false, nil
	}
	c, err := startStdioBridge(cmd)
	return c, true, err
}
var prepareOutputInstance = func(inst *fleet.Instance) error {
	b := backendutil.NewForInstance(inst, false)
	if _, err := fleetlaunch.EnsureFresh(b, inst.WorkspaceDir, nil); err != nil {
		return err
	}
	if failures := startup.RunWithTimeout(b, inst.WorkspaceDir, []startup.Script{startup.AudioScript(false, true)}, micPrepareTimeout); len(failures) > 0 {
		return failures[0]
	}
	return nil
}
var stopOutputServer = func(inst *fleet.Instance) {
	b := backendutil.NewForInstance(inst, false)
	if _, ok := b.(backend.OutputBackend); !ok {
		return
	}
	_, _ = b.RunScript(inst.ContainerID, fleetlaunch.RemotePath+" output stop >/dev/null 2>&1")
}

type outputClient struct {
	name      string
	devices   []*fleetgrpc.OutputDevice
	listed    bool
	selection *fleetgrpc.OutputSelection
	// All selection and audio messages share one ordered, bounded queue. A
	// selection change flushes audio before enqueueing the new selection.
	frames chan *fleetgrpc.OutputDown
	relist chan struct{}
	kicked chan struct{}
}
type outputSource struct {
	inst   *fleet.Instance
	cancel context.CancelFunc
}
type outputHub struct {
	mu                      sync.Mutex
	configMu                sync.Locker
	workers                 sync.WaitGroup
	pendingStop             bool
	settings                state.OutputSettings
	revision                uint64
	clients                 []*outputClient
	sources                 map[string]*outputSource
	prepared                map[string]time.Time
	openSlots, prepareSlots chan struct{}
	wake, dirty             chan struct{}
	onChange                func(*fleetgrpc.OutputTargets)
}

func newOutputHub() *outputHub {
	return &outputHub{sources: map[string]*outputSource{}, prepared: map[string]time.Time{}, openSlots: make(chan struct{}, 8), prepareSlots: make(chan struct{}, 2), wake: make(chan struct{}, 1), dirty: make(chan struct{}, 1)}
}
func (h *outputHub) changed() {
	select {
	case h.dirty <- struct{}{}:
	default:
	}
	select {
	case h.wake <- struct{}{}:
	default:
	}
}
func (h *outputHub) selectedLocked() *outputClient {
	if !h.settings.Enabled {
		return nil
	}
	for i := len(h.clients) - 1; i >= 0; i-- {
		if h.clients[i].name == h.settings.Client {
			return h.clients[i]
		}
	}
	if len(h.clients) > 0 {
		return h.clients[len(h.clients)-1]
	}
	return nil
}
func (h *outputHub) deviceLocked(c *outputClient) string {
	if h.settings.Client != "" && h.settings.Client != c.name {
		return ""
	}
	return h.settings.Device
}
func (h *outputHub) selectLocked() {
	selected := h.selectedLocked()
	for _, c := range h.clients {
		device := ""
		if c == selected {
			device = h.deviceLocked(c)
		}
		if c.selection != nil && c.selection.Active == (c == selected) && c.selection.Device == device {
			continue
		}
		c.selection = &fleetgrpc.OutputSelection{Active: c == selected, Device: device}
	drain:
		for {
			select {
			case <-c.frames:
			default:
				break drain
			}
		}
		c.frames <- &fleetgrpc.OutputDown{Msg: &fleetgrpc.OutputDown_Selection{Selection: c.selection}}
	}
	h.changed()
}
func (h *outputHub) configure(settings state.OutputSettings) uint64 {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.settings == settings {
		return h.revision
	}
	wasEnabled := h.settings.Enabled
	h.settings = settings
	h.revision++
	h.selectLocked()
	if !settings.Enabled {
		for _, c := range h.clients {
			close(c.kicked)
		}
		h.clients = nil
		for key, source := range h.sources {
			source.cancel()
			delete(h.sources, key)
		}
	}
	if wasEnabled && !settings.Enabled {
		h.pendingStop = true
		// Capture the instance set now. A delayed teardown must not resolve a
		// different state directory after the originating daemon has stopped.
		if st, err := state.Load(); err == nil {
			h.stopDisabledInstances(st)
			h.pendingStop = false
		}
	}
	return h.revision
}
func (h *outputHub) stopDisabledInstances(st *state.State) {
	stop := stopOutputServer
	for _, f := range st.Fleets {
		for _, inst := range f.Instances {
			if inst.Status == fleet.StatusRunning && inst.ContainerID != "" {
				go func() {
					h.mu.Lock()
					disabled := !h.settings.Enabled
					h.mu.Unlock()
					if disabled {
						stop(inst)
					}
				}()
			}
		}
	}
}
func (h *outputHub) add(name string) *outputClient {
	c := &outputClient{name: name, frames: make(chan *fleetgrpc.OutputDown, 16), relist: make(chan struct{}, 1), kicked: make(chan struct{})}
	h.mu.Lock()
	defer h.mu.Unlock()
	if !h.settings.Enabled {
		close(c.kicked)
		return c
	}
	h.clients = append(h.clients, c)
	h.selectLocked()
	return c
}
func (h *outputHub) remove(c *outputClient) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if i := slices.Index(h.clients, c); i >= 0 {
		h.clients = slices.Delete(h.clients, i, i+1)
		h.selectLocked()
	}
	if len(h.clients) == 0 {
		for key, source := range h.sources {
			source.cancel()
			delete(h.sources, key)
		}
	}
}
func (h *outputHub) list(refresh bool) *fleetgrpc.OutputTargets {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := &fleetgrpc.OutputTargets{Settings: &fleetgrpc.OutputSettings{Enabled: h.settings.Enabled, Client: h.settings.Client, Device: h.settings.Device}, Revision: h.revision}
	seen := map[string]bool{}
	selected := h.selectedLocked()
	for i := len(h.clients) - 1; i >= 0; i-- {
		c := h.clients[i]
		if refresh {
			select {
			case c.relist <- struct{}{}:
			default:
			}
		}
		if seen[c.name] {
			continue
		}
		seen[c.name] = true
		out.Targets = append(out.Targets, &fleetgrpc.OutputTarget{Client: c.name, Devices: c.devices, DevicesListed: c.listed, Selected: c == selected})
	}
	slices.SortFunc(out.Targets, func(a, b *fleetgrpc.OutputTarget) int { return strings.Compare(a.Client, b.Client) })
	return out
}
func (h *outputHub) setDevices(c *outputClient, list *fleetgrpc.OutputDeviceList) {
	var devices []mic.Device
	for _, d := range list.GetDevices() {
		if len(devices) >= 4*mic.MaxDevices {
			break
		}
		label := d.GetLabel()
		if len(label) > 4*mic.MaxDeviceLabel {
			label = label[:4*mic.MaxDeviceLabel]
		}
		devices = append(devices, mic.Device{ID: d.GetId(), Label: label})
	}
	var clean []*fleetgrpc.OutputDevice
	for _, d := range mic.CleanDevices(devices) {
		clean = append(clean, &fleetgrpc.OutputDevice{Id: d.ID, Label: d.Label})
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if !slices.Contains(h.clients, c) {
		return
	}
	c.devices, c.listed = clean, true
	h.changed()
}
func (h *outputHub) route(key string, source *outputSource, pcm []byte, end bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.sources[key] != source {
		return
	}
	c := h.selectedLocked()
	if c == nil {
		return
	}
	msg := &fleetgrpc.OutputDown{Msg: &fleetgrpc.OutputDown_Audio{Audio: &fleetgrpc.OutputAudio{Instance: key, Pcm: pcm, End: end}}}
	// Do not evict a pending selection control frame. Drop fresh audio if the
	// network is behind; the queue bounds the accumulated delay to 320 ms.
	select {
	case c.frames <- msg:
	default:
	}
}
func (h *outputHub) run(ctx context.Context) {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	defer func() {
		h.mu.Lock()
		for _, s := range h.sources {
			s.cancel()
		}
		clear(h.sources)
		h.mu.Unlock()
		h.workers.Wait()
	}()
	for {
		h.reconcile(ctx)
		if h.onChange != nil {
			h.onChange(h.list(false))
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		case <-h.wake:
		case <-h.dirty:
		}
	}
}
func (h *outputHub) reconcile(ctx context.Context) {
	if h.configMu != nil {
		h.configMu.Lock()
	}
	settings, err := outputSetting()
	if err == nil {
		h.configure(settings)
	}
	if h.configMu != nil {
		h.configMu.Unlock()
	}
	if err != nil {
		return
	}
	st, err := state.Load()
	if err != nil {
		return
	}
	want := map[string]*fleet.Instance{}
	for fn, f := range st.Fleets {
		for _, inst := range f.Instances {
			if inst.Status == fleet.StatusRunning && inst.ContainerID != "" {
				want[fn+"/"+inst.Name] = inst
			}
		}
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	// configure may have raced a SetConfig. The in-memory setting wins and no
	// stale file read may start a source after the output has been disabled.
	if !h.settings.Enabled || len(h.clients) == 0 {
		clear(want)
	}
	if h.pendingStop && !h.settings.Enabled {
		h.stopDisabledInstances(st)
		h.pendingStop = false
	}
	for key, s := range h.sources {
		if inst := want[key]; inst == nil || inst.ContainerID != s.inst.ContainerID {
			s.cancel()
			delete(h.sources, key)
		}
	}
	for key, inst := range want {
		if h.sources[key] != nil {
			continue
		}
		sourceCtx, cancel := context.WithCancel(ctx)
		source := &outputSource{inst: inst, cancel: cancel}
		h.sources[key] = source
		h.workers.Add(1)
		go func() { defer h.workers.Done(); h.runSource(sourceCtx, key, source) }()
	}
	// Preparation backoff survives TUI reconnects, but not deleted containers.
	live := map[string]bool{}
	for _, f := range st.Fleets {
		for _, i := range f.Instances {
			live[i.ContainerID] = true
		}
	}
	for id := range h.prepared {
		if !live[id] {
			delete(h.prepared, id)
		}
	}
}
func (h *outputHub) runSource(ctx context.Context, key string, s *outputSource) {
	for ctx.Err() == nil {
		ready, supported, err := h.serveSource(ctx, key, s)
		if ctx.Err() != nil {
			return
		}
		h.route(key, s, nil, true)
		delay := 2 * time.Second
		if !supported {
			delay = 5 * time.Minute
		} else if !ready {
			delay = 5 * time.Minute
			h.mu.Lock()
			at := h.prepared[s.inst.ContainerID]
			h.mu.Unlock()
			if time.Since(at) >= micPrepareRetry {
				select {
				case h.prepareSlots <- struct{}{}:
				case <-ctx.Done():
					return
				}
				if ctx.Err() != nil {
					<-h.prepareSlots
					return
				}
				h.mu.Lock()
				h.prepared[s.inst.ContainerID] = time.Now()
				h.mu.Unlock()
				prepErr := bounded(micPrepareTimeout+time.Minute, "prepare audio output", func() error { return prepareOutputInstance(s.inst) })
				<-h.prepareSlots
				if cfg, e := outputSetting(); e == nil && !cfg.Enabled {
					stopOutputServer(s.inst)
				}
				if prepErr != nil {
					flog.Warn("audio output setup failed", "instance", key, "err", prepErr)
					warnMic(fleetOf(key), s.inst.Name, fmt.Sprintf("audio output: %v", prepErr))
				}
				delay = 2 * time.Second
			}
		}
		if err != nil {
			flog.Info("audio output source lost", "instance", key, "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(delay):
		}
	}
}
func (h *outputHub) serveSource(ctx context.Context, key string, s *outputSource) (ready, supported bool, err error) {
	select {
	case h.openSlots <- struct{}{}:
	case <-ctx.Done():
		return false, true, ctx.Err()
	}
	type result struct {
		c   io.ReadWriteCloser
		ok  bool
		err error
	}
	opened := make(chan result)
	open := openOutputSource
	openCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	go func() {
		defer func() { <-h.openSlots }()
		c, ok, err := open(s.inst)
		select {
		case opened <- result{c, ok, err}:
		case <-openCtx.Done():
			if c != nil {
				c.Close()
			}
		}
	}()
	var r result
	select {
	case r = <-opened:
	case <-openCtx.Done():
		return false, true, openCtx.Err()
	}
	if !r.ok || r.err != nil {
		return false, r.ok, r.err
	}
	defer r.c.Close()
	finished := make(chan struct{})
	defer close(finished)
	go func() {
		select {
		case <-ctx.Done():
			r.c.Close()
		case <-finished:
		}
	}()
	timer := time.AfterFunc(45*time.Second, func() { r.c.Close() })
	defer timer.Stop()
	header := make([]byte, len("ready\n"))
	_, err = io.ReadFull(r.c, header)
	if err != nil || string(header) != "ready\n" {
		return false, true, fmt.Errorf("audio source did not become ready: %q (%v)", header, err)
	}
	timer.Stop()
	quiet := 0
	for {
		pcm := make([]byte, output.ChunkBytes)
		if _, err := io.ReadFull(r.c, pcm); err != nil {
			return true, true, err
		}
		silent := true
		for _, b := range pcm {
			if b != 0 {
				silent = false
				break
			}
		}
		if silent {
			quiet++
		} else {
			quiet = 0
		}
		if quiet <= 5 {
			h.route(key, s, pcm, false)
		}
	}
}

func (s *service) Output(stream fleetgrpc.FleetService_OutputServer) error {
	first, err := stream.Recv()
	if err != nil {
		return err
	}
	if first.GetOpen() == nil {
		return status.Error(codes.InvalidArgument, "first Output frame must carry open")
	}
	name := cleanMicClient(first.GetOpen().GetClient())
	if name == "" {
		return status.Error(codes.InvalidArgument, "output client name is required")
	}
	// Serialize registration with settings writes so an old config snapshot
	// cannot undo a disable or selection change made concurrently.
	s.muWrite.Lock()
	cfg, err := outputSetting()
	if err != nil {
		s.muWrite.Unlock()
		return status.Errorf(codes.Unavailable, "read audio output settings: %v", err)
	}
	s.output.configure(cfg)
	if !cfg.Enabled {
		s.muWrite.Unlock()
		return status.Error(codes.FailedPrecondition, "audio output is disabled in settings")
	}
	c := s.output.add(name)
	s.muWrite.Unlock()
	defer s.output.remove(c)
	errors := make(chan error, 1)
	go func() {
		for {
			up, err := stream.Recv()
			if err != nil {
				errors <- err
				return
			}
			if list := up.GetDevices(); list != nil {
				s.output.setDevices(c, list)
			}
		}
	}()
	for {
		select {
		case msg := <-c.frames:
			if err := stream.Send(msg); err != nil {
				return err
			}
		case <-c.relist:
			if err := stream.Send(&fleetgrpc.OutputDown{Msg: &fleetgrpc.OutputDown_ListDevices{ListDevices: &fleetgrpc.OutputListDevices{}}}); err != nil {
				return err
			}
		case err := <-errors:
			if err == io.EOF {
				return nil
			}
			return err
		case <-c.kicked:
			return status.Error(codes.FailedPrecondition, "audio output is disabled in settings")
		case <-stream.Context().Done():
			return stream.Context().Err()
		}
	}
}
func (s *service) ListOutputTargets(_ context.Context, req *fleetgrpc.ListOutputTargetsRequest) (*fleetgrpc.ListOutputTargetsReply, error) {
	s.muWrite.Lock()
	if cfg, err := outputSetting(); err == nil {
		s.output.configure(cfg)
	}
	s.muWrite.Unlock()
	return &fleetgrpc.ListOutputTargetsReply{Targets: s.output.list(req.GetRefresh())}, nil
}
