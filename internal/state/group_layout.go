package state

// GroupLayout records the outer tmux pane layout for a session group so
// it can be restored after a fleet restart. The Layout field is a tmux
// window_layout string (geometry + pane sizes); Sessions preserves the
// pane-to-session mapping by screen position.
type GroupLayout struct {
	FleetName    string   `json:"fleetName"`
	GroupID      string   `json:"groupID"`
	InstanceName string   `json:"instanceName"`
	Sessions     []string `json:"sessions"`
	Layout       string   `json:"layout"`
	PaneCount    int      `json:"paneCount"`
}

// Key scopes a saved layout to its fleet as well as its instance and group.
func (g GroupLayout) Key() string {
	return g.FleetName + "/" + g.InstanceName + "/" + g.GroupID
}

// UniqueInstanceFleet resolves layouts written by clients predating fleet
// identity. Ambiguous names must never select a fleet arbitrarily.
func (s *State) UniqueInstanceFleet(instanceName string) string {
	owner := ""
	for name, f := range s.Fleets {
		for _, inst := range f.Instances {
			if inst.Name == instanceName {
				if owner != "" {
					return ""
				}
				owner = name
			}
		}
	}
	return owner
}

// migrateGroupLayouts upgrades legacy instance/group keys when ownership is
// unambiguous. An ambiguous layout cannot safely be restored; the live tmux
// sessions remain available and will supply a new snapshot when opened.
func (s *State) migrateGroupLayouts() {
	for key, layout := range s.GroupLayouts {
		if layout.FleetName != "" {
			continue
		}
		delete(s.GroupLayouts, key)
		layout.FleetName = s.UniqueInstanceFleet(layout.InstanceName)
		if layout.FleetName == "" {
			continue
		}
		if _, exists := s.GroupLayouts[layout.Key()]; !exists {
			s.GroupLayouts[layout.Key()] = layout
		}
	}
}
