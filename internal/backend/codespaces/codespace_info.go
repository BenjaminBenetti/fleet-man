package codespaces

// codespaceInfo represents the JSON structure returned by `gh codespace view`.
type codespaceInfo struct {
	Name        string `json:"name"`
	DisplayName string `json:"displayName"`
	State       string `json:"state"`
}
