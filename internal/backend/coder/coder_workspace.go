package coder

// coderWorkspace is the JSON structure returned by `coder list -o json`.
type coderWorkspace struct {
	ID           string `json:"id"`
	Name         string `json:"name"`
	TemplateName string `json:"template_name"`
	LatestBuild  struct {
		Status    string `json:"status"`
		Resources []struct {
			Agents []coderAgent `json:"agents"`
		} `json:"resources"`
	} `json:"latest_build"`
}
