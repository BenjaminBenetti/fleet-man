package tui

// ActiveGroup names a specific tmux session group on a specific
// instance. Two groups on different instances may share an ID; the
// surrounding ref disambiguates them.
type ActiveGroup struct {
	Ref     InstanceRef
	GroupID string
}

// Empty reports whether the active group is unset.
func (a ActiveGroup) Empty() bool {
	return a.GroupID == "" || !a.Ref.Valid()
}
