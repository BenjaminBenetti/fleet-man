package imageserve

import (
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"sync"
	"time"

	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	"github.com/BenjaminBenetti/fleet-man/internal/state"
)

// Cluster is what the image server knows of one connection.
type Cluster struct {
	ID string
	// Credential is the connection's gateway pull credential
	// (Image.GatewayCredential); "" until delivery first needs one.
	Credential string
	// Previous is the credential a rotation replaced, still accepted until
	// PreviousUntil: a kubelet may hold the pull Secret's old content for a
	// moment.
	Previous      string
	PreviousUntil time.Time
	// PacePercent is the share of the measured uplink the connection's
	// pulls may use (Image.UploadPacePercent; 0 is unpaced).
	PacePercent int
}

// Clusters finds the daemon's connections and keeps their gateway pull
// credentials. A node authenticates a pull with Basic auth: the connection's
// id and its credential.
type Clusters interface {
	// Lookup returns a connection; false when there is none (it was
	// removed: its credential stops working at once).
	Lookup(id string) (Cluster, bool)
	// EnsureCredential returns the connection's credential, generating and
	// saving one when it has none.
	EnsureCredential(id string) (string, error)
	// RotateCredential replaces the connection's credential with a new one
	// and returns it.
	RotateCredential(id string) (string, error)
}

// rotationGrace is how long a replaced credential keeps working.
const rotationGrace = 10 * time.Minute

// StateClusters is Clusters over the daemon's connection registry
// (~/.fleet/kube_clusters.json), where each record's
// Image.GatewayCredential lives (0600, never sent over any RPC). Reads are
// reused for a couple of seconds, so a pull's requests do not each read the
// file. The zero value is ready.
type StateClusters struct {
	// Now overrides the clock (tests).
	Now func() time.Time

	mu       sync.Mutex
	loadedAt time.Time
	byID     map[string]fleet.KubeCluster
	previous map[string]Cluster
}

// stateClustersTTL is how long a read of the registry is reused.
const stateClustersTTL = 2 * time.Second

func (c *StateClusters) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

// load refreshes the cached registry when it is older than the TTL; callers
// hold c.mu.
func (c *StateClusters) load() error {
	if c.byID != nil && c.now().Sub(c.loadedAt) < stateClustersTTL {
		return nil
	}
	k, err := state.LoadKubeClusters()
	if err != nil {
		return err
	}
	c.byID = make(map[string]fleet.KubeCluster, len(k.Clusters))
	for _, r := range k.Clusters {
		c.byID[r.ID] = r
	}
	c.loadedAt = c.now()
	return nil
}

// Lookup implements Clusters.
func (c *StateClusters) Lookup(id string) (Cluster, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.load(); err != nil {
		return Cluster{}, false
	}
	r, ok := c.byID[id]
	if !ok {
		return Cluster{}, false
	}
	out := Cluster{ID: id, Credential: r.Image.GatewayCredential, PacePercent: PacePercent(r.Image)}
	if p, ok := c.previous[id]; ok && c.now().Before(p.PreviousUntil) {
		out.Previous, out.PreviousUntil = p.Previous, p.PreviousUntil
	}
	return out, true
}

// PacePercent is a connection's upload pace: its setting, else
// fleet.DefaultKubeUploadPacePercent.
func PacePercent(s fleet.KubeImageSettings) int {
	if s.UploadPacePercent == nil {
		return fleet.DefaultKubeUploadPacePercent
	}
	return *s.UploadPacePercent
}

// EnsureCredential implements Clusters.
func (c *StateClusters) EnsureCredential(id string) (string, error) {
	if cl, ok := c.Lookup(id); ok && cl.Credential != "" {
		return cl.Credential, nil
	}
	return c.update(id, false)
}

// RotateCredential implements Clusters.
func (c *StateClusters) RotateCredential(id string) (string, error) {
	return c.update(id, true)
}

func (c *StateClusters) update(id string, rotate bool) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	var cred, old string
	err := state.UpdateKubeClusters(func(k *state.KubeClusters) error {
		r := k.Find(id)
		if r == nil {
			return fmt.Errorf("no Kubernetes connection %q", id)
		}
		old = r.Image.GatewayCredential
		if rotate || old == "" {
			r.Image.GatewayCredential = newCredential()
		}
		cred = r.Image.GatewayCredential
		return nil
	})
	if err != nil {
		return "", err
	}
	if rotate && old != "" {
		if c.previous == nil {
			c.previous = map[string]Cluster{}
		}
		c.previous[id] = Cluster{Previous: old, PreviousUntil: c.now().Add(rotationGrace)}
	}
	c.byID = nil
	return cred, nil
}

// newCredential is a random 256-bit credential.
func newCredential() string {
	var b [32]byte
	_, _ = rand.Read(b[:])
	return base64.RawURLEncoding.EncodeToString(b[:])
}
