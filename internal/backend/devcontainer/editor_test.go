package devcontainer

import (
	"context"
	"encoding/json"
	"testing"
)

func TestEditorInfoReadsTheLabelAndTheWorkspaceMount(t *testing.T) {
	inspectJSON := `{"Mounts":[
	  {"Type":"bind","Source":"/home/u/.fleet/workspaces/repo/alpha/repo/","Destination":"/work/repo"},
	  {"Type":"volume","Name":"v","Source":"/var/lib/docker/volumes/v","Destination":"/home/vscode/.cache"}],
	  "Config":{"Labels":{
	    "devcontainer.local_folder":"/home/u/.fleet/workspaces/repo/alpha/repo",
	    "devcontainer.metadata":"[{\"remoteUser\":\"vscode\"}]"}}}`
	var inspected inspectedContainer
	if err := json.Unmarshal([]byte(inspectJSON), &inspected); err != nil {
		t.Fatal(err)
	}
	old := inspectForEditor
	inspectForEditor = func(id string) (*inspectedContainer, error) {
		if id != "cid" {
			t.Errorf("inspected %q", id)
		}
		return &inspected, nil
	}
	t.Cleanup(func() { inspectForEditor = old })

	info, err := (&DevcontainerBackend{}).EditorInfo(context.Background(), "cid")
	if err != nil {
		t.Fatal(err)
	}
	if info.WorkspaceFolder != "/work/repo" || info.Metadata != `[{"remoteUser":"vscode"}]` {
		t.Errorf("EditorInfo = %+v", info)
	}

	// No bind of the workspace: the CLI's default folder.
	inspected.Mounts = nil
	info, _ = (&DevcontainerBackend{}).EditorInfo(context.Background(), "cid")
	if info.WorkspaceFolder != "/workspaces/repo" {
		t.Errorf("default folder = %q", info.WorkspaceFolder)
	}
	if _, err := (&DevcontainerBackend{}).EditorInfo(context.Background(), ""); err == nil {
		t.Error("an instance with no container was read")
	}
}
