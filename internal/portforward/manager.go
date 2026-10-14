package portforward

import (
	"fmt"
	"net"
	"sync"

	"github.com/BenjaminBenetti/fleet-man/internal/flog"
)

// Manager tracks active in-process port forward proxies per instance. Each
// forward is a local TCP listener whose accepted connections are tunnelled
// to the target via a TargetDialer (one dial per connection).
type Manager struct {
	mu       sync.Mutex
	forwards map[string][]*Forward // key: "fleet/instance"
}

// NewManager creates a new port forward manager.
func NewManager() *Manager {
	return &Manager{
		forwards: make(map[string][]*Forward),
	}
}

// Add starts a new port forward for the given instance: a local listener on
// spec.LocalPort whose connections are tunnelled to the target via dial
// (spec.HalfClose is recorded for display; dial carries it).
func (m *Manager) Add(key string, spec Forward, dial TargetDialer) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	for _, fwd := range m.forwards[key] {
		if fwd.LocalPort == spec.LocalPort {
			return fmt.Errorf("local port %d is already forwarded on %s", spec.LocalPort, key)
		}
	}

	spec.BrowserProxy = false
	if err := m.addForwardLocked(key, spec, dial); err != nil {
		return err
	}
	flog.Info("port-forward started", "key", key, "localPort", spec.LocalPort, "remotePort", spec.RemotePort, "halfClose", spec.HalfClose)
	return nil
}

// addForwardLocked starts a forward on spec.LocalPort and records it, with
// spec's marks (BrowserProxy, HalfClose), under key.
//
// The caller must hold m.mu; addForwardLocked does not lock.
func (m *Manager) addForwardLocked(key string, spec Forward, dial TargetDialer) error {
	fwd, err := startProxy(spec.LocalPort, spec.RemotePort, dial)
	if err != nil {
		return fmt.Errorf("start proxy %d->%d: %w", spec.LocalPort, spec.RemotePort, err)
	}
	fwd.BrowserProxy, fwd.HalfClose = spec.BrowserProxy, spec.HalfClose
	m.forwards[key] = append(m.forwards[key], fwd)
	return nil
}

// FindBrowserProxy returns the local port of an existing browser proxy
// forward for the given instance key. Returns (0, false) if none exists.
func (m *Manager) FindBrowserProxy(key string) (int, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()

	for _, fwd := range m.forwards[key] {
		if fwd.BrowserProxy {
			return fwd.LocalPort, true
		}
	}
	return 0, false
}

// AddBrowserProxy creates a port forward marked as a browser proxy,
// automatically selecting an available local port. It returns the
// chosen local port on success.
func (m *Manager) AddBrowserProxy(key string, remotePort int, dial TargetDialer) (int, error) {
	// Grab an ephemeral port from the OS.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, fmt.Errorf("find available port: %w", err)
	}
	localPort := ln.Addr().(*net.TCPAddr).Port
	ln.Close()

	m.mu.Lock()
	defer m.mu.Unlock()

	if err := m.addForwardLocked(key, Forward{LocalPort: localPort, RemotePort: remotePort, BrowserProxy: true}, dial); err != nil {
		return 0, err
	}
	flog.Info("browser proxy started", "key", key, "localPort", localPort, "remotePort", remotePort)
	return localPort, nil
}

// Remove stops and removes the port forward on the given local port.
func (m *Manager) Remove(key string, localPort int) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	fwds := m.forwards[key]
	for i, fwd := range fwds {
		if fwd.LocalPort == localPort {
			stopForward(fwd)
			m.forwards[key] = append(fwds[:i], fwds[i+1:]...)
			flog.Info("port-forward stopped", "key", key, "localPort", localPort)
			return nil
		}
	}
	return fmt.Errorf("no forward on local port %d for %s", localPort, key)
}

// RemoveAll stops all port forwards for an instance.
func (m *Manager) RemoveAll(key string) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if n := len(m.forwards[key]); n > 0 {
		flog.Info("port-forwards stopped", "key", key, "count", n)
	}

	for _, fwd := range m.forwards[key] {
		stopForward(fwd)
	}
	delete(m.forwards, key)
}

// List returns a copy of active forwards for the given instance.
func (m *Manager) List(key string) []Forward {
	if m == nil {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()

	fwds := m.forwards[key]
	result := make([]Forward, len(fwds))
	for i, fwd := range fwds {
		result[i] = Forward{LocalPort: fwd.LocalPort, RemotePort: fwd.RemotePort, BrowserProxy: fwd.BrowserProxy, HalfClose: fwd.HalfClose}
	}
	return result
}

// ListAll returns all active forwards grouped by instance key.
func (m *Manager) ListAll() map[string][]Forward {
	m.mu.Lock()
	defer m.mu.Unlock()

	result := make(map[string][]Forward, len(m.forwards))
	for key, fwds := range m.forwards {
		entries := make([]Forward, len(fwds))
		for i, fwd := range fwds {
			entries[i] = Forward{LocalPort: fwd.LocalPort, RemotePort: fwd.RemotePort, BrowserProxy: fwd.BrowserProxy, HalfClose: fwd.HalfClose}
		}
		result[key] = entries
	}
	return result
}

// FormatLabels returns a comma-separated string of forward labels for an instance.
// Returns empty string if no forwards are active. Safe to call on a nil manager.
func (m *Manager) FormatLabels(key string) string {
	if m == nil {
		return ""
	}
	fwds := m.List(key)
	if len(fwds) == 0 {
		return ""
	}
	labels := ""
	for i, fwd := range fwds {
		if i > 0 {
			labels += ", "
		}
		labels += fwd.Label()
	}
	return labels
}

// Shutdown stops all active port forwards. Safe to call on a nil manager.
func (m *Manager) Shutdown() {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()

	for key, fwds := range m.forwards {
		for _, fwd := range fwds {
			stopForward(fwd)
		}
		delete(m.forwards, key)
	}
}
