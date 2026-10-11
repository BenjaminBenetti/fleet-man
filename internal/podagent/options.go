package podagent

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"time"

	"github.com/BenjaminBenetti/fleet-man/internal/execas"
	"github.com/BenjaminBenetti/fleet-man/internal/podwire"
	"github.com/BenjaminBenetti/fleet-man/internal/version"
)

// Where a pod keeps the agent's state.
const (
	// DefaultControlDir is the pod's control emptyDir, where the in-pod
	// sockets live and the main agent records itself (.pod-agent.pid).
	DefaultControlDir = "/fleet-mounts/control"
	// DefaultRunDir is fleet's private run directory in the pod (root only):
	// the agents' takeover sockets and, under held/, the shepherds'.
	DefaultRunDir = "/opt/fleet/run"

	pidFileName = ".pod-agent.pid"
	heldDirName = "held"

	// DefaultTmuxDir is where fleet stages its static tmux in a pod whose
	// image has none (fleetd's install dir and bundle name): bin/ holds
	// tmux, share/terminfo the terminal descriptions it reads.
	DefaultTmuxDir = "/opt/fleet/tmux"
)

// Environment the agent reads in a pod.
const (
	// EnvPodUID is the pod's metadata.uid, from the downward API. Hello
	// reports it, and held commands are bound to it.
	EnvPodUID = "FLEET_POD_UID"
	// EnvHoldGrace overrides podwire.HoldGrace (a Go duration).
	EnvHoldGrace = "FLEET_EXEC_HOLD_GRACE"
)

// Method is an RPC the agent serves: it gets the call's raw JSON args and
// returns a result to encode as JSON.
type Method func(ctx context.Context, args json.RawMessage) (any, error)

// Options configure Serve, ExecStdio and the shepherds they start.
// DefaultOptions fills them for a pod; tests point them elsewhere.
type Options struct {
	// Role is the session's role (Serve only).
	Role podwire.Role
	// DaemonProto is fleetd's highest protocol version (`serve --proto`): the
	// agent offers nothing above it. 0 offers its whole range.
	DaemonProto int

	// SpecPath is the pod's exec spec, CacheDir the probe cache.
	SpecPath string
	CacheDir string
	// Host overrides the Host built from SpecPath (tests).
	Host *execas.Host

	ControlDir string
	RunDir     string
	PodUID     string
	Version    string

	// Exe and ShepherdArgs start a shepherd: the fleet binary (its own
	// /proc/self/exe by default) and the argv after it ("pod-agent
	// shepherd").
	Exe          string
	ShepherdArgs []string
	// ShepherdEnv is the shepherds' environment (this process's by default).
	ShepherdEnv []string

	// Methods are RPCs beyond the built-in ones.
	Methods map[string]Method

	// Bundles are the program bundles fleet may stage into the pod (the
	// static tmux): each one that is installed, when a command is planned,
	// is last on the command's PATH whatever its class (see
	// execas.Host.Bundles), so an image without tmux gets the bundled one
	// (and one with its own keeps it) and never an install from a package
	// mirror.
	Bundles []execas.Bundle

	// HoldGrace and MaxHeld bound held commands.
	HoldGrace time.Duration
	MaxHeld   int
	// PingInterval and Silence override podwire's PingInterval and
	// AgentSilence; StreamPing and StreamSilence the direct-mode stream's
	// heartbeat.
	PingInterval, Silence     time.Duration
	StreamPing, StreamSilence time.Duration
	// TakeoverAsk is how long a new agent waits for the recorded one to
	// release; TakeoverKill how long it then waits after SIGTERM before
	// SIGKILL, and after SIGKILL for the lock.
	TakeoverAsk, TakeoverKill time.Duration
	// NoTakeover skips the pid file: for agents a test runs side by side.
	NoTakeover bool
	// ControlCheck is how often the main agent checks that its in-pod
	// sockets' files are still its own (2s by default).
	ControlCheck time.Duration

	// InitOOMScore is the oom_score_adj file whose value the agent takes for
	// itself, and so for everything it starts: PID 1's in a pod. "" keeps
	// the agent's own.
	InitOOMScore string

	// Stderr is where the agent writes what it cannot send fleetd.
	Stderr io.Writer
}

// DefaultOptions are a pod's: its paths, the environment's pod UID and hold
// grace, and this build's version.
func DefaultOptions() Options {
	o := Options{
		Role:       podwire.RoleMain,
		SpecPath:   execas.DefaultSpecPath,
		CacheDir:   execas.DefaultCacheDir,
		ControlDir: DefaultControlDir,
		RunDir:     DefaultRunDir,
		PodUID:     os.Getenv(EnvPodUID),
		Version:    version.Version,
		// CRI-O with crun starts exec'd processes at -999, not the
		// container's score.
		InitOOMScore: "/proc/1/oom_score_adj",
		Bundles:      []execas.Bundle{{Bin: DefaultTmuxDir + "/bin", Terminfo: DefaultTmuxDir + "/share/terminfo"}},
	}
	if d, err := time.ParseDuration(os.Getenv(EnvHoldGrace)); err == nil && d > 0 {
		o.HoldGrace = d
	}
	return o
}

// withDefaults fills what o leaves zero.
func (o Options) withDefaults() Options {
	if o.Role == "" {
		o.Role = podwire.RoleMain
	}
	if o.RunDir == "" {
		o.RunDir = DefaultRunDir
	}
	if o.ControlDir == "" {
		o.ControlDir = DefaultControlDir
	}
	if o.Exe == "" {
		o.Exe = "/proc/self/exe"
		if exe, err := os.Executable(); err == nil {
			o.Exe = exe
		}
	}
	if o.ShepherdArgs == nil {
		o.ShepherdArgs = []string{"pod-agent", "shepherd"}
	}
	if o.ShepherdEnv == nil {
		o.ShepherdEnv = os.Environ()
	}
	if o.HoldGrace <= 0 {
		o.HoldGrace = podwire.HoldGrace
	}
	if o.MaxHeld <= 0 {
		o.MaxHeld = podwire.MaxHeld
	}
	if o.PingInterval <= 0 {
		o.PingInterval = podwire.PingInterval
	}
	if o.Silence <= 0 {
		o.Silence = podwire.AgentSilence
	}
	if o.StreamPing <= 0 {
		o.StreamPing = podwire.StreamPingInterval
	}
	if o.StreamSilence <= 0 {
		o.StreamSilence = podwire.StreamSilence
	}
	if o.TakeoverAsk <= 0 {
		o.TakeoverAsk = 2 * time.Second
	}
	if o.TakeoverKill <= 0 {
		o.TakeoverKill = 2 * time.Second
	}
	if o.ControlCheck <= 0 {
		o.ControlCheck = 2 * time.Second
	}
	if o.Stderr == nil {
		o.Stderr = os.Stderr
	}
	return o
}

// protoRange is the protocol range the agent offers.
func (o Options) protoRange() (int, int) {
	hi := podwire.ProtoMax
	if o.DaemonProto > 0 && o.DaemonProto < hi {
		hi = max(o.DaemonProto, podwire.ProtoMin)
	}
	return podwire.ProtoMin, hi
}
