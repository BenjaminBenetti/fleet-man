package devmeta

import (
	"encoding/json"
	"strings"
)

// RemoteUser extracts the effective remoteUser from a devcontainer.metadata
// label value: the JSON array of merged metadata entries (base image, then
// each feature, then the user's devcontainer.json last). Scalars merge by
// array order with later entries winning, so the effective user is the LAST
// non-empty remoteUser; absent any remoteUser it falls back to the last
// non-empty containerUser (what the CLI itself defaults exec to). Returns ""
// for an empty/unparseable label or one that names neither.
//
// The value is returned as recorded. Callers that hand it to an exec as a
// user check it with LooksLikeUsername first.
func RemoteUser(metadataJSON string) string {
	if strings.TrimSpace(metadataJSON) == "" {
		return ""
	}
	var entries []struct {
		RemoteUser    string `json:"remoteUser"`
		ContainerUser string `json:"containerUser"`
	}
	if err := json.Unmarshal([]byte(metadataJSON), &entries); err != nil {
		return ""
	}
	user := ""
	for _, e := range entries {
		if e.RemoteUser != "" {
			user = e.RemoteUser
		}
	}
	if user != "" {
		return user
	}
	for _, e := range entries {
		if e.ContainerUser != "" {
			user = e.ContainerUser
		}
	}
	return user
}

// LooksLikeUsername reports whether s is a plausible single-token Unix
// username: non-empty, no whitespace, and free of shell-expansion leftovers
// like an unsubstituted ${...} that a metadata template may carry.
func LooksLikeUsername(s string) bool {
	return s != "" && !strings.ContainsAny(s, " \t\r\n") && !strings.Contains(s, "${")
}
