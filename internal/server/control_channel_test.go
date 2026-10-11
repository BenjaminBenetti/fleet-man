package server

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/BenjaminBenetti/fleet-man/internal/agentsock"
	"github.com/BenjaminBenetti/fleet-man/internal/backend"
	"github.com/BenjaminBenetti/fleet-man/internal/control"
	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	"github.com/BenjaminBenetti/fleet-man/internal/mcpbridge"
	"github.com/BenjaminBenetti/fleet-man/internal/state"
)

// control_channel_test.go: instances whose backend carries the control channel
// itself (backend.ControlChannel). No backend has one yet, so these tests make
// such instances through the seams.

// recordingChannel is a control channel that keeps what is written to it.
type recordingChannel struct {
	mu      sync.Mutex
	files   map[string][]byte
	modes   map[string]fs.FileMode
	writes  int
	removes []string
}

func (c *recordingChannel) WriteControlFile(name string, data []byte, mode fs.FileMode) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.files == nil {
		c.files, c.modes = map[string][]byte{}, map[string]fs.FileMode{}
	}
	c.files[name], c.modes[name] = append([]byte(nil), data...), mode
	c.writes++
	return nil
}

func (c *recordingChannel) RemoveControlFile(name string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.files, name)
	c.removes = append(c.removes, name)
	return nil
}

func (c *recordingChannel) snapshot() (files map[string][]byte, writes int, removes []string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	files = make(map[string][]byte, len(c.files))
	for k, v := range c.files {
		files[k] = v
	}
	return files, c.writes, append([]string(nil), c.removes...)
}

// stubChannelInstances makes the instances named (by instance name, in any
// fleet) channel instances, each with its own recordingChannel.
func stubChannelInstances(t *testing.T, names ...string) map[string]*recordingChannel {
	t.Helper()
	channels := make(map[string]*recordingChannel, len(names))
	for _, name := range names {
		channels[name] = &recordingChannel{}
	}
	origHas, origFor := instanceHasControlChannel, controlChannelFor
	instanceHasControlChannel = func(inst *fleet.Instance) bool { return channels[inst.Name] != nil }
	controlChannelFor = func(inst *fleet.Instance) (backend.ControlChannel, bool) {
		if c := channels[inst.Name]; c != nil {
			return c, true
		}
		return nil, false
	}
	t.Cleanup(func() { instanceHasControlChannel, controlChannelFor = origHas, origFor })
	return channels
}

func TestInstanceHasControlChannelOnTodaysBackends(t *testing.T) {
	for _, bt := range []fleet.BackendType{"", fleet.BackendDevcontainer, fleet.BackendCodespaces, fleet.BackendCoder} {
		inst := &fleet.Instance{Name: "i", Backend: bt, ContainerID: "c1"}
		if instanceHasControlChannel(inst) {
			t.Errorf("backend %q: reported a control channel of its own", bt)
		}
		if _, ok := controlChannelFor(inst); ok {
			t.Errorf("backend %q: handed out a control channel", bt)
		}
	}
}

// TestControlRegistryServesNoHostSocketForChannelInstances: a running channel
// instance gets no host control socket (nor a host control directory made for
// one); every other running instance still does.
func TestControlRegistryServesNoHostSocketForChannelInstances(t *testing.T) {
	shortHome(t)
	stubChannelInstances(t, "pod")
	r := newControlRegistry(nil, nil, nil)
	defer r.shutdown()

	st := &state.State{Fleets: map[string]*fleet.Fleet{"alpha": {Name: "alpha", Instances: []*fleet.Instance{
		{Name: "host", Status: fleet.StatusRunning},
		{Name: "pod", Status: fleet.StatusRunning},
	}}}}
	r.syncRunning(st)

	if !waitForSocket(t, state.ControlSocketPath("alpha", "host"), 2*time.Second) {
		t.Fatal("the host instance lost its control socket")
	}
	if _, ok := r.servers["alpha/pod"]; ok {
		t.Fatal("a channel instance got a host control listener")
	}
	if _, err := os.Stat(state.ControlDir("alpha", "pod")); !os.IsNotExist(err) {
		t.Fatalf("a host control directory was made for a channel instance (%v)", err)
	}
}

// TestControlHandlerForServesRelayedConnections: a connection that reaches the
// daemon over a control channel is served by the same handler as a host
// socket's, tagged with the instance it was relayed for.
func TestControlHandlerForServesRelayedConnections(t *testing.T) {
	var opens []openEvent
	var copies []copyEvent
	r := newControlRegistry(func(f, i, url string) {
		opens = append(opens, openEvent{f, i, url})
	}, func(f, i, src, dst string, open bool) {
		copies = append(copies, copyEvent{f, i, src, dst, open})
	}, nil)

	relayed := strings.NewReader(
		`{"type":"browser.open","payload":{"url":"http://localhost:3000"}}` + "\n" +
			`{"type":"browser.open","payload":{"url":""}}` + "\n" +
			`{"type":"file.copy","payload":{"src":":out.txt","dst":"~/Downloads","open":true}}` + "\n")
	if err := control.ServeConn(relayed, r.handlerFor("alpha", "pod")); err != nil {
		t.Fatalf("ServeConn: %v", err)
	}
	if len(opens) != 1 || opens[0] != (openEvent{"alpha", "pod", "http://localhost:3000"}) {
		t.Errorf("browser opens = %+v, want one for alpha/pod (an empty URL is dropped)", opens)
	}
	if len(copies) != 1 || copies[0] != (copyEvent{"alpha", "pod", ":out.txt", "~/Downloads", true}) {
		t.Errorf("copies = %+v, want one for alpha/pod", copies)
	}
}

// TestControlDirTargetSkipsChannelInstances: the SSH-agent relay and the
// in-instance MCP serve no socket from the host control directory of a channel
// instance, even when one exists there.
func TestControlDirTargetSkipsChannelInstances(t *testing.T) {
	t.Setenv("HOME", shortTempDir(t))
	stubChannelInstances(t, "pod")
	provisionControlDir(t, "on", "host", true)
	provisionControlDir(t, "on", "pod", true)

	if _, ok := controlDirTarget("on", &fleet.Instance{Name: "pod"}); ok {
		t.Fatal("controlDirTarget targeted a channel instance")
	}
	if _, ok := controlDirTarget("on", &fleet.Instance{Name: "host"}); !ok {
		t.Fatal("controlDirTarget lost the host instance")
	}

	if !instanceSocketsSupported {
		return
	}
	m := newTestInstanceMCP(t)
	m.sync(&state.State{Fleets: map[string]*fleet.Fleet{"on": {
		Name: "on", Settings: fleet.FleetSettings{FleetMCP: true},
		Instances: []*fleet.Instance{{Name: "host", Status: fleet.StatusRunning}, {Name: "pod", Status: fleet.StatusRunning}},
	}}})
	if _, err := os.Stat(mcpSocket("on", "host")); err != nil {
		t.Fatalf("the host instance lost its MCP socket: %v", err)
	}
	if _, err := os.Stat(filepath.Join(state.ControlDir("on", "pod"), mcpbridge.SocketName)); !os.IsNotExist(err) {
		t.Fatalf("a channel instance got a host MCP socket (%v)", err)
	}
}

// TestHostSocketSyncsSkipKubernetesInstances: a Kubernetes record, in any
// build, carries its control channel in its pod: the control registry, the
// SSH-agent relay and the in-instance MCP open nothing in a host control
// directory for it, whichever way they are asked (their reconciles, and the
// ensure provisioning calls), even when such a directory exists.
func TestHostSocketSyncsSkipKubernetesInstances(t *testing.T) {
	shortHome(t)
	t.Setenv(agentsock.EnvOverride, "")
	pod := &fleet.Instance{Name: "pod", Backend: fleet.BackendKubernetes, KubeCluster: "kc-1", KubeKey: "abcdefgh",
		Status: fleet.StatusRunning, ContainerID: "k8s:kc-1/ns/fi-on-pod-abcdefgh@g1"}
	if !instanceHasControlChannel(pod) {
		t.Fatal("a Kubernetes record has no control channel of its own")
	}
	st := &state.State{Fleets: map[string]*fleet.Fleet{"on": {
		Name: "on", Settings: fleet.FleetSettings{FleetMCP: true}, Instances: []*fleet.Instance{pod},
	}}}
	if err := state.Save(st); err != nil {
		t.Fatal(err)
	}
	dir := provisionControlDir(t, "on", "pod", true)
	if _, ok := controlDirTarget("on", pod); ok {
		t.Fatal("controlDirTarget targeted a Kubernetes instance")
	}
	if _, ok := loadInstanceTarget("on", "pod"); ok {
		t.Fatal("loadInstanceTarget targeted a Kubernetes instance")
	}

	r := newControlRegistry(nil, nil, nil)
	defer r.shutdown()
	r.syncRunning(st)
	if _, ok := r.servers["on/pod"]; ok {
		t.Fatal("the control registry listens for a Kubernetes instance")
	}
	if !instanceSocketsSupported {
		return
	}
	svc := newService()
	t.Cleanup(svc.instanceMCP.close)
	t.Cleanup(svc.agent.close)
	svc.agent.syncInstances(st)
	svc.agent.ensureInstance("on", "pod")
	svc.instanceMCP.sync(st)
	svc.instanceMCP.ensureInstance("on", "pod")
	for _, name := range []string{control.SocketName, agentsock.SocketName, mcpbridge.SocketName} {
		if _, err := os.Lstat(filepath.Join(dir, name)); !os.IsNotExist(err) {
			t.Errorf("a host %s was opened for a Kubernetes instance (%v)", name, err)
		}
	}
}
