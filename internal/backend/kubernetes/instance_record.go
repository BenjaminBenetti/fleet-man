package kubernetes

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"github.com/BenjaminBenetti/fleet-man/internal/atomicfile"
	"github.com/BenjaminBenetti/fleet-man/internal/backend"
	"github.com/BenjaminBenetti/fleet-man/internal/hostrelay"
	"github.com/BenjaminBenetti/fleet-man/internal/k8s/names"
	"github.com/BenjaminBenetti/fleet-man/internal/state"
)

// instance_record.go is what fleetd keeps of each instance it made on a
// cluster, beside the instance's state.json record: the root generation the
// instance runs, the pod fleetd rendered for it (its spec hash), the
// StatefulSet it created (its UID) and the image it delivered. A live pod is
// verified against this record before it gets a session, a file or a
// command (PodTarget); the cluster's own annotations are never trusted for
// it. One file per instance (state.KubeInstancePath), written when a create
// applies the instance's StatefulSet and removed with the instance.

// InstanceRecordVersion is the record format this fleet writes and reads.
const InstanceRecordVersion = 1

// InstanceRecord is fleetd's record of one instance on a cluster.
type InstanceRecord struct {
	V int `json:"v"`
	// Cluster, Namespace and Key place the instance: its connection, the
	// namespace its objects are in, its key (names.InstanceKey).
	Cluster   string `json:"cluster"`
	Namespace string `json:"namespace"`
	Key       string `json:"key"`
	// Fleet and Instance name it (for the log lines of its pod).
	Fleet    string `json:"fleet"`
	Instance string `json:"instance"`
	// StatefulSet is its StatefulSet (names.Instance), StatefulSetUID the
	// UID of the one fleetd created.
	StatefulSet    string `json:"statefulSet"`
	StatefulSetUID string `json:"statefulSetUID"`
	// Gen is the root generation it runs (its spec Secret is
	// names.SpecSecret(StatefulSet, Gen)).
	Gen int `json:"gen"`
	// SpecHash is the spec hash of fleetd's render of the pod
	// (podspec.SpecHash of the StatefulSet's pod template).
	SpecHash string `json:"specHash"`
	// Image is the image the pod runs, as its spec names it; ImageDigests
	// what its container status may report as the image's id (the
	// delivered digest, the host's image id, and for a node-loaded image the
	// id the node gave it when the instance first ran).
	Image        string   `json:"image"`
	ImageDigests []string `json:"imageDigests"`
	// Strategy is how the image was delivered (kubeimage.Strategy).
	Strategy string `json:"strategy,omitempty"`
	// RemoteUser and WorkspaceFolder are the devcontainer's, which its
	// lifecycle hooks run as and in.
	RemoteUser      string `json:"remoteUser,omitempty"`
	WorkspaceFolder string `json:"workspaceFolder,omitempty"`
	// NonRoot: the pod runs without root (capability tier C).
	NonRoot bool `json:"nonRoot,omitempty"`
	// AudioMissing: a NonRoot pod whose image lacks the audio stack, which
	// nothing there can install: the instance has no virtual microphone or
	// audio output (audio.go). Checked when a create or rebuild starts its
	// pod.
	AudioMissing bool `json:"audioMissing,omitempty"`
	// Created is the instance container's creation time (the
	// container-created annotation the lifecycle hooks' markers compare).
	Created time.Time `json:"created"`
	// Ports are the run's published ports (-p, appPort), which the daemon
	// serves on its host while the instance runs.
	Ports []backend.PublishedPort `json:"ports,omitempty"`
	// HostSockets are the daemon-host sockets the devcontainer binds, their
	// sources resolved when recorded: what the pod's agent binds, and the
	// only daemon-host paths fleetd dials for the pod.
	HostSockets []hostrelay.Socket `json:"hostSockets,omitempty"`
	// HostGateway: the devcontainer declares host-gateway, so the pod
	// mirrors the daemon host's listening ports (all but GatewaySkip, the
	// ports it serves itself) while its connection allows it.
	HostGateway bool     `json:"hostGateway,omitempty"`
	GatewaySkip []uint16 `json:"gatewaySkip,omitempty"`
	// Share is where the instance's fleet mounts live on the connection:
	// the fleet's share, or the instance's own copy (share.go). nil without
	// fleet mounts.
	Share *ShareRecord `json:"share,omitempty"`
}

// ErrInstanceRecordNewer is a record a newer fleet wrote: read-only to this
// one.
var ErrInstanceRecordNewer = errors.New("the instance record was written by a newer fleet")

// errNoInstanceRecord is an instance fleetd has no record of (one whose
// create never applied its StatefulSet, or a record lost with ~/.fleet).
var errNoInstanceRecord = errors.New("fleet has no record of the instance's pod")

// InstanceRecordPath is where the record of the instance with key on
// connection clusterID lives (a var for tests).
var InstanceRecordPath = state.KubeInstancePath

// Validate checks the record is one this fleet can use.
func (r InstanceRecord) Validate() error {
	switch {
	case r.V > InstanceRecordVersion:
		return fmt.Errorf("%w: version %d, this fleet reads up to %d", ErrInstanceRecordNewer, r.V, InstanceRecordVersion)
	case r.Cluster == "" || r.StatefulSet == "":
		return errors.New("the instance record names no connection or StatefulSet")
	case !names.ValidHash(r.Key):
		return fmt.Errorf("instance key %q is not one", r.Key)
	case !names.ValidGeneration(r.Gen):
		return fmt.Errorf("root generation %d is out of range", r.Gen)
	}
	return nil
}

// SaveInstanceRecord writes r (0600, its directory 0700).
func SaveInstanceRecord(r InstanceRecord) error {
	r.V = InstanceRecordVersion
	if err := r.Validate(); err != nil {
		return err
	}
	path := InstanceRecordPath(r.Cluster, r.Key)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("instance record: %w", err)
	}
	data, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	if err := atomicfile.Write(path, append(data, '\n'), 0o600); err != nil {
		return fmt.Errorf("instance record: %w", err)
	}
	return nil
}

// LoadInstanceRecord reads the record of the instance with key on
// connection clusterID; a missing one is errNoInstanceRecord.
func LoadInstanceRecord(clusterID, key string) (InstanceRecord, error) {
	path := InstanceRecordPath(clusterID, key)
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return InstanceRecord{}, errNoInstanceRecord
	}
	if err != nil {
		return InstanceRecord{}, fmt.Errorf("instance record: %w", err)
	}
	var r InstanceRecord
	if err := json.Unmarshal(data, &r); err != nil {
		return InstanceRecord{}, fmt.Errorf("instance record %s: %w", filepath.Base(path), err)
	}
	if err := r.Validate(); err != nil {
		return r, fmt.Errorf("instance record %s: %w", filepath.Base(path), err)
	}
	if r.Cluster != clusterID || r.Key != key {
		return InstanceRecord{}, fmt.Errorf("instance record %s names %s/%s", filepath.Base(path), r.Cluster, r.Key)
	}
	return r, nil
}

// RemoveInstanceRecord deletes the record; a missing one is fine.
func RemoveInstanceRecord(clusterID, key string) error {
	if err := os.Remove(InstanceRecordPath(clusterID, key)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("instance record: %w", err)
	}
	return nil
}

// addImageDigest adds d to the record's image digests, once.
func (r *InstanceRecord) addImageDigest(d string) {
	if d == "" {
		return
	}
	for _, have := range r.ImageDigests {
		if have == d {
			return
		}
	}
	r.ImageDigests = append(r.ImageDigests, d)
}
