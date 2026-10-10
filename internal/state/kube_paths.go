package state

import "path/filepath"

// The daemon's Kubernetes state on its host. Everything here is daemon-owned:
// clients reach it only through the kube RPCs. The hourly backup captures
// kube_clusters.json, daemon_id, the footprint, the job journal, each
// connection's image index and its instance records
// (internal/server/backup.go), under these same paths relative to ~/.fleet.

// KubeClustersPath is the registry of Kubernetes connections
// (~/.fleet/kube_clusters.json, see KubeClusters).
func KubeClustersPath() string {
	return filepath.Join(FleetDir(), "kube_clusters.json")
}

// DaemonIDPath holds the daemon's 128-bit id (~/.fleet/daemon_id), which
// names and labels every object the daemon creates in a cluster.
func DaemonIDPath() string {
	return filepath.Join(FleetDir(), "daemon_id")
}

// KubeDir is the directory of the daemon's other Kubernetes state
// (~/.fleet/kube), created 0700.
func KubeDir() string {
	return filepath.Join(FleetDir(), "kube")
}

// KubeIncarnationPath holds the daemon's incarnation nonce and the machine
// fingerprint it was made for (~/.fleet/kube/incarnation.json). It is not
// backed up: a copy of ~/.fleet on another machine must not inherit it.
func KubeIncarnationPath() string {
	return filepath.Join(KubeDir(), "incarnation.json")
}

// KubeFootprintPath records what the daemon has created and destroyed in its
// clusters (~/.fleet/kube/footprint.json).
func KubeFootprintPath() string {
	return filepath.Join(KubeDir(), "footprint.json")
}

// KubeJobsDir holds the job journal, one file per instance being created,
// cloned or rebuilt (~/.fleet/kube/jobs).
func KubeJobsDir() string {
	return filepath.Join(KubeDir(), "jobs")
}

// KubeJobPath is the journal of the job running on the instance with the
// given key (~/.fleet/kube/jobs/<key8>.json).
func KubeJobPath(key string) string {
	return filepath.Join(KubeJobsDir(), safeLogSegment(key)+".json")
}

// KubeClusterDir holds one connection's state (~/.fleet/kube/<cluster id>).
// Connection ids are single path segments (names.ValidateClusterID) and never
// "jobs" or "images"; a malformed one is still reduced to one safe segment
// here.
func KubeClusterDir(clusterID string) string {
	return filepath.Join(KubeDir(), safeLogSegment(clusterID))
}

// KubeImagesPath is one connection's image index
// (~/.fleet/kube/<cluster id>/images.json).
func KubeImagesPath(clusterID string) string {
	return filepath.Join(KubeClusterDir(clusterID), "images.json")
}

// KubeInstancesDir holds what the daemon recorded of each instance it made on
// a connection (~/.fleet/kube/<cluster id>/instances): the pod it rendered and
// the image it delivered, which a live pod is verified against.
func KubeInstancesDir(clusterID string) string {
	return filepath.Join(KubeClusterDir(clusterID), "instances")
}

// KubeInstancePath is the record of the instance with key on a connection
// (~/.fleet/kube/<cluster id>/instances/<key8>.json).
func KubeInstancePath(clusterID, key string) string {
	return filepath.Join(KubeInstancesDir(clusterID), safeLogSegment(key)+".json")
}

// KubeImageCacheDir is the gateway image cache
// (~/.fleet/kube/images): the manifests and blobs fleetd serves to clusters
// through the gateway, content-addressed, with their index (store.json). It
// is not backed up: everything in it is made again from the daemon host's
// docker.
func KubeImageCacheDir() string {
	return filepath.Join(KubeDir(), "images")
}
