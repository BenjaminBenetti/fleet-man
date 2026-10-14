package state

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sync"

	"github.com/BenjaminBenetti/fleet-man/internal/atomicfile"
	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	"github.com/BenjaminBenetti/fleet-man/internal/k8s/names"
)

// KubeClusters is the daemon's registry of Kubernetes connections,
// ~/.fleet/kube_clusters.json. It lives in its own file, not config.json,
// because the kube RPCs edit single records (SetConfig replaces the whole
// config) and because a record carries Image.GatewayCredential, so the file is
// written 0600. Each record is a fleet.KubeCluster; its JSON tags are the file
// format.
type KubeClusters struct {
	// V is the file format version (KubeClustersVersion). A file from a newer
	// fleet is read but never overwritten.
	V        int                 `json:"v"`
	Clusters []fleet.KubeCluster `json:"clusters"`
}

// KubeClustersVersion is the kube_clusters.json format this build writes.
const KubeClustersVersion = 1

// ErrKubeClustersNewer refuses to overwrite a kube_clusters.json written by a
// newer fleet, which may hold settings this build would drop.
var ErrKubeClustersNewer = errors.New("kube_clusters.json was written by a newer fleet")

// kubeClustersMu serializes UpdateKubeClusters within the daemon, the file's
// only writer.
var kubeClustersMu sync.Mutex

// LoadKubeClusters reads the registry. A missing file is an empty registry.
func LoadKubeClusters() (*KubeClusters, error) {
	data, err := os.ReadFile(KubeClustersPath())
	if os.IsNotExist(err) {
		return &KubeClusters{V: KubeClustersVersion}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading kube clusters file: %w", err)
	}
	k := &KubeClusters{}
	if err := json.Unmarshal(data, k); err != nil {
		return nil, fmt.Errorf("parsing kube clusters file: %w", err)
	}
	if k.V == 0 {
		k.V = KubeClustersVersion
	}
	return k, nil
}

// UpdateKubeClusters is an atomic read-modify-write of the registry: fn edits
// the loaded registry in place, and the result is saved only when fn returns
// nil (its error is returned verbatim) and every record still has a valid,
// unique id. A registry from a newer fleet is refused with
// ErrKubeClustersNewer before fn runs.
func UpdateKubeClusters(fn func(*KubeClusters) error) error {
	kubeClustersMu.Lock()
	defer kubeClustersMu.Unlock()

	k, err := LoadKubeClusters()
	if err != nil {
		return err
	}
	if k.V > KubeClustersVersion {
		return fmt.Errorf("%w (format v%d, this build writes v%d); upgrade fleet to change Kubernetes connections", ErrKubeClustersNewer, k.V, KubeClustersVersion)
	}
	if err := fn(k); err != nil {
		return err
	}
	k.V = KubeClustersVersion
	if err := k.validate(); err != nil {
		return err
	}
	return saveKubeClusters(k)
}

// Find returns the record with the given id, or nil. The pointer aliases the
// registry, so an UpdateKubeClusters callback may edit through it.
func (k *KubeClusters) Find(id string) *fleet.KubeCluster {
	for i := range k.Clusters {
		if k.Clusters[i].ID == id {
			return &k.Clusters[i]
		}
	}
	return nil
}

// Remove drops the record with the given id and reports whether it was there.
func (k *KubeClusters) Remove(id string) bool {
	for i := range k.Clusters {
		if k.Clusters[i].ID == id {
			k.Clusters = append(k.Clusters[:i], k.Clusters[i+1:]...)
			return true
		}
	}
	return false
}

// validate holds the invariants everything keyed by a record id relies on:
// labels, ContainerIDs, deploy targets and ~/.fleet/kube/<id>.
func (k *KubeClusters) validate() error {
	seen := make(map[string]bool, len(k.Clusters))
	for _, c := range k.Clusters {
		if err := names.ValidateClusterID(c.ID); err != nil {
			return err
		}
		if seen[c.ID] {
			return fmt.Errorf("two Kubernetes connections have the id %q", c.ID)
		}
		seen[c.ID] = true
	}
	return nil
}

func saveKubeClusters(k *KubeClusters) error {
	if err := os.MkdirAll(FleetDir(), 0700); err != nil {
		return fmt.Errorf("creating fleet dir: %w", err)
	}
	data, err := json.MarshalIndent(k, "", "  ")
	if err != nil {
		return fmt.Errorf("marshaling kube clusters: %w", err)
	}
	// 0600: records carry the gateway pull credential. atomicfile keeps the
	// backup loop and unlocked readers from seeing a torn file.
	if err := atomicfile.Write(KubeClustersPath(), data, 0600); err != nil {
		return fmt.Errorf("writing kube clusters file: %w", err)
	}
	return nil
}
