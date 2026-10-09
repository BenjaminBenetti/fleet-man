package tui

import (
	"context"
	"fmt"
	"slices"
	"sync"
	"time"

	"github.com/BenjaminBenetti/fleet-man/fleetgrpc"
	"github.com/BenjaminBenetti/fleet-man/internal/configutil"
	"github.com/BenjaminBenetti/fleet-man/internal/fleetclient"
	"github.com/BenjaminBenetti/fleet-man/internal/mic"
	"github.com/BenjaminBenetti/fleet-man/internal/output"
	"github.com/BenjaminBenetti/fleet-man/internal/protoconv"
	tea "github.com/charmbracelet/bubbletea"
)

var outputCtl struct {
	mu      sync.Mutex
	parent  context.Context
	program *tea.Program
	cancel  context.CancelFunc
	gen     int
}

func startOutputControl(ctx context.Context, program *tea.Program) {
	outputCtl.mu.Lock()
	defer outputCtl.mu.Unlock()
	outputCtl.parent, outputCtl.program = ctx, program
}
func syncOutputFromConfig(config *configutil.Config) {
	outputCtl.mu.Lock()
	defer outputCtl.mu.Unlock()
	if outputCtl.parent == nil {
		return
	}
	enabled := config != nil && config.OutputSettings.Enabled
	if !enabled && outputCtl.cancel != nil {
		outputCtl.cancel()
		outputCtl.cancel = nil
		outputCtl.gen++
	}
	if enabled && outputCtl.cancel == nil {
		ctx, cancel := context.WithCancel(outputCtl.parent)
		outputCtl.cancel = cancel
		outputCtl.gen++
		go runOutputClient(ctx, outputCtl.program, outputCtl.gen)
	}
}

type outputStatusMsg struct {
	status output.Status
	gen    int
}
type outputTargetsMsg struct {
	targets *fleetgrpc.OutputTargets
	gen     int
}

func runOutputClient(ctx context.Context, program *tea.Program, gen int) {
	report, flush := newLatestForwarder(func(s output.Status) { program.Send(outputStatusMsg{s, gen}) })
	defer flush()
	defer func() {
		outputCtl.mu.Lock()
		defer outputCtl.mu.Unlock()
		if outputCtl.gen == gen {
			outputCtl.cancel()
			outputCtl.cancel = nil
		}
	}()
	for ctx.Err() == nil {
		conn, err := fleetclient.Dial(ctx)
		if err == nil {
			output.Run(ctx, conn.Service(), report)
			conn.Close()
			return
		}
		report(output.Status{Detail: "connecting to daemon"})
		select {
		case <-ctx.Done():
			return
		case <-time.After(time.Second):
		}
	}
}

func (m *model) adoptOutputSettings(p *fleetgrpc.OutputSettings) {
	if p == nil || m.config == nil {
		return
	}
	m.config.OutputSettings = configutil.OutputSettings{Enabled: p.GetEnabled(), Client: p.GetClient(), Device: p.GetDevice()}
	syncOutputFromConfig(m.config)
}

var relistOutputTargetsRemote = func() error {
	return mutate(func(ctx context.Context, svc fleetgrpc.FleetServiceClient) error {
		_, err := svc.ListOutputTargets(ctx, &fleetgrpc.ListOutputTargetsRequest{Refresh: true})
		return err
	})
}

func relistOutputTargetsCmd() tea.Cmd {
	return func() tea.Msg { _ = relistOutputTargetsRemote(); return nil }
}

type outputChoice struct{ client, device string }

func outputChoices(m *model) []outputChoice {
	choices := []outputChoice{{}}
	for _, t := range m.outputTargets.GetTargets() {
		choices = append(choices, outputChoice{client: t.GetClient()})
		for _, d := range t.GetDevices() {
			choices = append(choices, outputChoice{client: t.GetClient(), device: d.GetId()})
		}
	}
	return choices
}
func outputDeviceLabel(m *model) string {
	s := m.config.OutputSettings
	if s.Client == "" {
		return "Auto"
	}
	client := mic.CleanText(s.Client, mic.MaxClientName)
	for _, t := range m.outputTargets.GetTargets() {
		if t.GetClient() != s.Client {
			continue
		}
		if s.Device == "" {
			return client + " — System default"
		}
		for _, d := range t.GetDevices() {
			if d.GetId() == s.Device {
				return client + " — " + mic.CleanText(d.GetLabel(), mic.MaxDeviceLabel)
			}
		}
		if !t.GetDevicesListed() {
			return client + " — " + mic.CleanText(s.Device, mic.MaxDeviceID)
		}
		return client + " — device unavailable (System default)"
	}
	return client + " — disconnected"
}
func outputRouteNote(m *model) string {
	for _, t := range m.outputTargets.GetTargets() {
		if t.GetSelected() {
			name := mic.CleanText(t.GetClient(), mic.MaxClientName)
			if wanted := m.config.OutputSettings.Client; wanted != "" && wanted != t.GetClient() {
				return "Output on " + name + " while " + mic.CleanText(wanted, mic.MaxClientName) + " is disconnected"
			}
			return "Output on " + name
		}
	}
	return "Waiting for a client with audio output"
}

// The acknowledgement's revision prevents a queued Watch snapshot from
// undoing a newer local save when the user cycles devices quickly.
var setOutputConfigRemote = func(config *configutil.Config) (uint64, error) {
	var revision uint64
	err := mutate(func(ctx context.Context, svc fleetgrpc.FleetServiceClient) error {
		reply, err := svc.SetConfig(ctx, &fleetgrpc.SetConfigRequest{Config: protoconv.ConfigToProto(config)})
		if err == nil {
			revision = reply.GetOutputRevision()
		}
		return err
	})
	return revision, err
}

func (p *settingsPage) toggleOutputEnabled(m *model) tea.Cmd {
	if m.config == nil {
		m.config = placeholderConfig()
	}
	old := m.config.OutputSettings
	m.config.OutputSettings.Enabled = !old.Enabled
	revision, err := setOutputConfigRemote(m.config)
	if err != nil {
		m.config.OutputSettings = old
		m.message = fmt.Sprintf("Failed to save settings: %v", err)
		return nil
	}
	m.outputRevision = revision
	syncOutputFromConfig(m.config)
	if old.Enabled {
		m.outputStatus = output.Status{}
		m.message = "Audio output off"
		return nil
	}
	m.message = "Audio output on — select Auto or a connected client's device"
	return relistOutputTargetsCmd()
}
func (p *settingsPage) cycleOutputDevice(m *model, direction int) tea.Cmd {
	if m.config == nil {
		return nil
	}
	choices := outputChoices(m)
	if len(choices) == 1 {
		m.message = "Waiting for clients to announce their output devices"
		return relistOutputTargetsCmd()
	}
	old := m.config.OutputSettings
	index := max(0, slices.Index(choices, outputChoice{old.Client, old.Device}))
	next := choices[(index+direction+len(choices))%len(choices)]
	m.config.OutputSettings.Client, m.config.OutputSettings.Device = next.client, next.device
	revision, err := setOutputConfigRemote(m.config)
	if err != nil {
		m.config.OutputSettings = old
		m.message = fmt.Sprintf("Failed to save settings: %v", err)
		return nil
	}
	m.outputRevision = revision
	m.message = "Audio output set to " + outputDeviceLabel(m)
	return nil
}
