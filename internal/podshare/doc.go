// Package podshare is an instance pod's half of its fleet's shared folders
// (the agents' state, the shared files, the custom mounts) on a cluster: the
// tree they live in on the fleet's share volume (or, on the copy tier, on the
// instance's own disk), the layout the pod's share-layout init container
// replays there before the dev container mounts its folders out of it
// (`fleet pod-layout`, Apply), and the spoke that keeps the tree converged
// with the daemon host's hub (`fleet pod-agent spoke`, RunSpoke).
//
// The tree, at MountDir in the pod's containers:
//
//	roots/<path>  the fleet's folders, by their path under the fleet's mount
//	              root on the daemon host (~/.fleet/workspaces/<fleet>):
//	              roots/.claude, roots/files, roots/.mnt/opt/data. Each of
//	              the fleet's mounts is the subPath roots/<its SubPath>.
//	state/        the spoke's manifests (its sharesync Tree's state)
//	lease         the spoke's lease: the one process holding its lock is the
//	              tree's spoke, and its epoch counts the spokes there were
//
// MountDir is inside SyncDir, which only root may enter (the init container
// makes it so): the pod's users reach the tree only through their own
// folders' mounts, so the spoke's state and lease are beyond them. In a pod
// without root one uid runs everything, and the tree is as much theirs as
// the spoke's.
//
// Everything that runs as a process (Apply's chown, the spoke) is meant for
// a Linux pod; the types and paths build everywhere, for fleetd.
package podshare
