package fleet

// CoderParameter holds a single Coder template parameter binding for a
// fleet (issue #221). Name+Value are the user's binding; the remaining
// fields are template-derived metadata refreshed whenever the TUI fetches
// the template's rich parameters, kept so the edit-fleet dialog can render
// labels/defaults without re-fetching.
type CoderParameter struct {
	Name         string `json:"name"`
	Value        string `json:"value"`                   // may contain ${GIT_URL}, ${GIT_BRANCH}, ${INSTANCE_NAME}
	DefaultValue string `json:"default_value,omitempty"` // from template
	DisplayName  string `json:"display_name,omitempty"`  // from template
	Description  string `json:"description,omitempty"`   // from template
	Type         string `json:"type,omitempty"`          // "string", "number"
}
