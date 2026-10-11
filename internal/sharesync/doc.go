// Package sharesync keeps a fleet's shared folders converged between their
// copies: the canonical tree on the daemon host (~/.fleet/workspaces/<fleet>,
// bind-mounted by the fleet's docker instances) and the fleet's share on each
// cluster (or each instance's own copy, on the copy tier).
//
// Each copy is a side. A side runs a Tree over its folders: it scans them
// (on watcher events and periodically) against its manifest, the state it
// last synced, and reports what changed; it applies the changes the hub
// sends it, never over local content the hub has not seen, and never an op
// older than the rev it holds; and it serves its files' content. The Tree
// writes through internal/safefs, so a symlink planted in the folders by an
// instance can never redirect a write, and it never replaces or removes a
// reserved (Layout) path. A Side wraps a Tree with its watchers and its scan
// loop, and is the hub's Peer for it.
//
// The hub, on the daemon host, is the single sequencer. Every change it
// accepts for a root gets rev = ++root.rev, on disk before the hub answers,
// and every report names the rev its change was made on (its base). A change
// on the current rev is a fast forward; any other is concurrent with what the
// hub accepted since, and the path's policy (internal/mounts/syncpolicy)
// decides: mirror keeps the newest change and a conflict copy of the other,
// append-union merges the lines, and the policies of part B (credentials,
// json3, toml3) plug in through the same Merger interface: a policy's scope
// (fleet-wide or per location), its lane, the earlier contents it keeps for
// a three-way merge, and a decision to accept, keep or merge. Nothing is
// dropped: a change the hub cannot take now (a root over its cap) waits on
// the side that made it, a side that cannot take a change (its disk full) is
// paused for files and catches up later, and a change that loses to a newer
// one is kept as a conflict copy, the decision waiting while no reachable
// side holds the losing version.
//
// The hub talks to sides through two small interfaces, Peer (hub to side:
// apply a batch, fetch content, rescan) and Reporter (side to hub: report
// changes), so the daemon host's own Tree is driven in process and a pod's
// spoke over the agent and bulk sessions with the same engine; the messages
// are plain JSON-ready structs. Lanes say how soon a change travels and on
// which session: priority at once, normal after 100ms, bulk (files over 1MiB)
// after 1s. A side attaches with a Hello (what it holds); revs of another
// epoch than the hub's (the hub's state is new to it), or a rev the hub would
// have handed out again after losing state, make what the side holds count as
// changes the hub has not seen. A root whose folder is no longer the one its
// Tree opened stops with ErrRootMoved rather than read as emptied, and a
// manifest that describes another folder is dropped when the Tree opens.
//
// State lives under directories the caller gives (FleetDir: ~/.fleet/sync/
// <fleet> for the hub and the host's Tree) as versioned JSON-lines journals,
// a snapshot and a log of the records written since; a newer version is
// refused (ErrNewerState) and that root is not synced.
package sharesync
