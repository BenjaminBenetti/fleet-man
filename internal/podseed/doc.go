// Package podseed materializes a Kubernetes instance's persistent root: the
// in-pod half of the seed Job (`fleet pod-job`), and the layout of the
// instance PVC that the pod renderer mounts back by subPath.
//
// On docker an instance's root filesystem is the image plus a writable layer,
// kept across stop/start and replaced by rebuild. On a cluster the PVC is that
// layer. A seed Job runs the instance's own image with the PVC mounted, and
// pod-job copies the image root onto it, one top-level directory at a time,
// keeping everything docker's layer keeps: owners, modes with setuid/setgid,
// file capabilities and ACLs (every extended attribute but security.selinux,
// which the node relabels), hard links, sparse files, device nodes, FIFOs and
// times. A self-check re-reads every file capability and setuid bit the copy
// wrote, and a dir the Go copier fails is copied again with GNU cp where the
// image has it. The instance pod then mounts root/<dir> over each /<dir>.
//
// Around the copy the seed does what docker and the devcontainer CLI would
// have done to the container:
//
//   - Mountpoint parents the image lacks (/workspaces for the workspace mount,
//     a new top-level dir a volume lands in) get their own root-extra/<dir>, so
//     a sibling repo cloned next to the workspace persists too. Image content
//     under a mount target is not copied: the mount hides it, as on docker.
//   - The kubelet's /etc/hosts, /etc/hostname and /etc/resolv.conf get empty
//     placeholders in root/etc, and meta/ gets the files the pod mounts at the
//     latter two (CRI-O would otherwise leave them shadowed by the /etc mount).
//   - remoteUser's uid and gid are remapped to the share group's in the copied
//     /etc and its home is chowned, the CLI's updateRemoteUserUID step: one uid
//     across a fleet's instances, whatever their images.
//   - Volumes are copied up from the image into vol/<name> the first time, as
//     docker fills a new named volume.
//   - The staged fleet binary is installed content-addressed into fleet/bin,
//     and /usr/bin/fleet in the new root links to it.
//
// A rebuild reseeds with Reset: the current root and root-extra become
// root.g<N>.trash and root-extra.g<N>.trash before generation N+1 is copied, a
// rollback Job restores them, and a commit Job deletes them once the rebuild
// succeeded. Every step is a rename or is redone from scratch, so a Job killed
// at any point can simply be run again. The result, meta/rootfs.json, is a
// versioned Manifest; a newer version than this binary knows reads as
// ErrNewerManifest.
//
// The spec and manifest types build everywhere, for the daemon that renders
// and reads them; copying needs linux.
package podseed
