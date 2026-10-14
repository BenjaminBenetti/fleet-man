package backend

import "os"

// LayoutEntry is one path of a fleet's shared mount tree: a directory mount's
// source, the shared files directory, or one file in it. The resolver makes
// them on the daemon host (~/.fleet/workspaces/<fleet>), where the tree is
// canonical; a backend that keeps the fleet's shared state elsewhere (a
// cluster's share volume) replays them there, so every fleet-shared Mount's
// SubPath exists with the same kind and mode before anything mounts it.
type LayoutEntry struct {
	// Path is slash-separated and relative to the tree's root. It is never
	// absolute and never contains "..". Missing parents are created as mkdir
	// -p would; nothing is enforced on them.
	Path string
	// Dir is true for a directory, false for a regular file.
	Dir bool
	// Mode is the permission enforced on Path.
	Mode os.FileMode
	// Seed is what a file is created with where it does not exist yet ("{}"
	// for a JSON settings file an agent parses at start); "" creates none:
	// an empty file there would read as content of its own, so the file is
	// left to whatever writes it first.
	Seed string
}
