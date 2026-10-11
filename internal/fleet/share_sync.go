package fleet

import "time"

// ShareSync is a fleet's shared-state sync, as `fleet sync status` shows it:
// its shared folders (agent dirs, shared files, custom mounts) on the
// daemon host, in a share on each cluster it has instances on, and the
// daemon's hub keeping them converged.
type ShareSync struct {
	Fleet string
	// Paused are the reasons sync is paused anywhere, as the `sync paused:
	// <reason>` badge words them.
	Paused []string
	Roots  []ShareSyncRoot
	// Shares are the fleet's shares on its clusters.
	Shares []ShareUsage
	// Conflicts counts the conflict copies kept.
	Conflicts int
}

// ShareSyncRoot is one synced folder of a fleet (".claude", "files",
// ".mnt", ...).
type ShareSyncRoot struct {
	Root  string
	Files int
	Bytes int64
	// Cap is the root's size cap, 0 for none.
	Cap int64
	// Paused says why changes made in it wait; Broken why it is not synced.
	Paused string
	Broken string
	// Warnings flag parts of it grown large.
	Warnings []string
	Sides    []ShareSyncSide
}

// ShareSyncSide is one side's state in one root: "host" (the daemon host's
// folders), "store:<cluster>" (a cluster's logins kept on the daemon host),
// or "k8s:<cluster>/<share>" (a share's spoke).
type ShareSyncSide struct {
	Side     string
	Location string
	// Pending counts what is still to be sent the side, PendingBytes its
	// files' bytes.
	Pending      int
	PendingBytes int64
	// Paused says why it cannot take files now ("share full").
	Paused string
	// Refused lists the paths it refused, with why.
	Refused   []string
	LastError string
}

// ShareConflict is a conflict copy the sync kept on the daemon host: the
// version of a path that lost to a concurrent change.
type ShareConflict struct {
	ID    string
	Time  time.Time
	Fleet string
	Root  string
	Path  string
	// Location is set for a path kept per location (a login).
	Location string
	// Side is the side whose version it is; Reason what it lost to.
	Side   string
	Reason string
	Size   int64
	// File holds its content on the daemon host.
	File string
}
