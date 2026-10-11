package sharesync

// Space is a filesystem's size and free room in bytes, as an unprivileged
// writer may use it.
type Space struct {
	Total int64
	Free  int64
}

// SpaceFunc reports the space of the filesystem holding dir.
type SpaceFunc func(dir string) (Space, error)
